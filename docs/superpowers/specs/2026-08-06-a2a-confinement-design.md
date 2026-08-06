# A2A 沙盒約束（confinement）與收尾修正 設計

日期：2026-08-06
狀態：規格已定案，尚未實作；其中「三級能力授權」一節需使用者確認後才可動工
前置：
- `2026-08-05-a2a-integration-design.md`（已實作）
- `2026-08-06-a2a-sandbox-driver-design.md`（已實作，commits `37ccd84..b4a2c4d`）

## 目的

前兩輪把 A2A 從協定層做到沙盒驅動，第三方 review 以三個視角（安全 / 併發 / 正確性）逐條複查後，**三個視角一致判定 DO NOT SHIP**。本規格是第三輪：把「委派任務跑在一個沒有任何工具限制的 Claude Code session 裡」這個結構性缺陷關掉，並補上讓這套東西第一次真的能跑完一趟的三層開機修正、呼叫方取得結果的路徑、以及 operator 管理它的介面。

整套 A2A 仍包在 `cfg.A2A.Enabled` 底下、預設關閉；三份 review 都各自獨立確認了這個 kill switch 是真的（`main.go:218` 與 `main.go:302` 兩個 `if` 涵蓋所有正式引用，`cmd/`、`internal/` 內無漏網）。因此本輪修正對線上零風險，而在這些修正完成前，`a2a.enabled` 必須維持 `false`。

---

## 一、最終 review 發現

以下每一條都對照 HEAD（`b4a2c4d`）的原始碼逐一驗證過。行號為驗證當下的實際行號。

### Critical

**S1 — 沙盒的 permission gate 完全 fail-open。**
`permission.go:271-275`：

```go
b, ok := bindingByWorktree(reg, hi.CWD)
if !ok {
    fmt.Fprint(out, hookDecisionJSON(true, "permission gate: no binding for cwd, allowing"))
    return nil
}
```

沙盒 session 以 `CC_REGISTRY_ROOT = SandboxRoot(root, session)` 啟動（`a2a_executor.go:141,169` → `a2a_session.go:31` → `worktree.go:275`）。該目錄由 `Init()`（`init.go:8`）建立，只有 `inbox/outbox/state/locks/logs/`，**沒有 `bindings.json`**；`LoadRegistry` 對缺檔回傳空 registry + `nil` error（`registry.go:211-220`），`bindingByWorktree`（`permission.go:511`）必然 miss，於是六個 matcher（`Bash`/`Edit`/`Write`/`WebFetch`/`WebSearch`/`mcp__.*`，`worktree.go:117-135`）全部無條件放行。而 hook 本身是有裝的——`StartTmuxClaude` 對每個沙盒 worktree 呼叫 `EnsureAgentSettings`（`worktree.go:275,168`）。

review 以**實際編譯出的 binary**對一個 sandbox 形狀的 root 實跑驗證，`Bash: curl … | sudo sh` 與 `mcp__planetscale__run_sql: drop table x` 都拿到 `allow`。

後果不是「任務可能改到不該改的檔」，而是：一個已核准的呼叫方送出一則 `message/send`，得到一個以 `conray` 身分執行、能讀 `~/.claude/.credentials.json`、能讀 `$CLAUDE_CODE_OAUTH_TOKEN`（`worktree.go:300-305` 刻意注入）、能讀 `callers.json`（明文憑證，`0644`）、能寫進 40 個 production worktree 的無限制 shell。

這與 `2026-08-05-a2a-integration-design.md:58` 白紙黑字寫的「範圍外自動拒絕」相反，而該規格第 59 行早就點名了這個死結。實作繼承了既有的 fail-open。

**S2 — capability 只被記錄，從不被執行。**
`a2a_server.go:173-201` 是全 repo 唯一讀 capability 的地方，且只在 dispatch 當下比對。`Caller.Allows`（`a2a_callers.go:102`）沒有第二個呼叫端；`agent.Capabilities` 另一個用途只是填 agent card（`a2a_card.go:34`）。`SandboxExecutor.Start`、`SandboxDriver.loop`、`permission.go` 全部沒有任何 capability 引用。所以宣告 `["docs-only"]` 的 agent 拿到的沙盒，與任何其他 agent 拿到的完全一樣——依 S1 就是完整主機權限。

（跨 agent 提權是**擋住的**：dispatch 要求呼叫方持有該 agent 宣告的每一項 capability，且宣告零項的 agent fail-closed。缺的是授權**之內**的最小權限。）

**C1 — 同一 contextId 換 agent 會永久孤兒化一個活著的沙盒。**
`a2a_server.go:227-234` 的擁有權檢查只比對 `CallerID`；`TaskStore.Upsert`（`a2a_tasks.go:61-69`）以 `ContextID` 為唯一 key 整列覆寫；而 `SessionNameFor`（`a2a_tasks.go:122`）與 `SandboxWorktree`（`a2a_executor.go:31`）都含 agent 名。同一個呼叫方對同一個 contextId 換 agent 再送一次，舊的 `aa-<oldagent>-<ctx>`（活著的 tmux session + ~80MB worktree + `sandboxes/` 目錄）就不再被任何 row 參照。`SweepTimeouts` 只走 `tasks.Tasks`（`a2a_lifecycle.go:195` 起），沒有任何程式碼掃 `sandboxes/`，`EnsureSandboxDrivers`（`a2a_driver.go:282`）只停 goroutine 不停 tmux。同時 `RunningCount`（`a2a_tasks.go:106`）是對 row 計數，孤兒不列入 → 8 併發上限對它們完全無效。

**C2 — handler 與 DrainQueue 幾乎每次都會重複派送同一個 task。**
`DrainQueue`（`a2a_lifecycle.go:46`）用**未上鎖**的 `LoadTasks`；`SandboxExecutor.Start` 開頭的 `persist(task)` 寫回去時 state 仍是 `submitted`（`a2a_executor.go:148` 附近），要到 `EnsureWorkspace` + `Start` + `Inject` 全部成功後才寫 `TaskWorking`（`a2a_executor.go:207` 起）。而 `waitSessionReady` 上限 90 秒（`worktree.go:52` `sessionBootDelay`），A2A cycle 預設 10 秒（`config.go:131`）。所以 handler 派送的任務幾乎必然在開機窗口內被 DrainQueue 再派一次：因為 message id 現在刻意保證唯一（`a2a_executor.go:58-61`），第二則 prompt 會**真的**送進同一個沙盒，同一段委派工作跑兩遍（可能含 commit / push）。

**X1 — 沙盒開機必卡，而規格指定的前置緩解是死碼。**
三件事疊在一起：

1. `EnsureFolderTrusted` / `ClaudeConfigPath`（`a2a_trust.go:33,11`）**零正式呼叫端**（`grep` 排除 `_test.go` 後只剩定義本身）。規格 `2026-08-06:111` 的第 1 項緩解沒有被接上 `SandboxExecutor.Start`。
2. `StartTmuxClaude` 對每個沙盒 worktree 寫入含 `SessionStart` hook 的 `.claude/settings.local.json`（`worktree.go:117-119`），Claude Code 開機時因此跳「Managed settings require approval」閘（使用者記憶檔 `managed-settings-hooks-gate` 記載此閘擋住每一個全新 worker 開機）。
3. 該畫面被 `classifyScreen` 判為 **`ScreenLogin`**（`screen.go:55` 呼叫 `paneAwaitingManagedSettings`，`screen.go:180`），而 `autoAnswerSandboxConfirm` 只在 `ScreenConfirm` 時動作（`a2a_driver.go:217`）→ 什麼都不做。唯一會處理它的 `SelectTrustSettings`（`adapters.go:265`）只從 binding 迴圈呼叫（`supervisor.go:174`）。

再加上 `TmuxInjector.paneBusy` 只把 `ScreenWorking`/`ScreenConfirm` 視為忙碌（`adapters.go:208-217`），`ScreenLogin` 不算 → `RunWorkerOnce` 照常 `typeAndSubmit`，把 prompt 打進核准畫面；驗證條件是「輸入框裡還有沒有字」（`adapters.go:94`），核准畫面沒有輸入框 → **Inject 回報成功**、job 移進 `inbox/done`、prompt 消失。任務停在 `working` 兩小時後被 sweep 轉 `canceled`，呼叫方永遠不知道。

### Important

| # | 位置 | 具體後果 |
|---|---|---|
| I1 撤銷不影響已排隊/執行中的工作 | `a2a_lifecycle.go:45-65`、`a2a_executor.go:132-137` | `DrainQueue` 不重讀 `callers.json`、不查 `Status`、不重查 capability；`Start` 只查 agent 存在、不查 `agent.Enabled`（與 `a2a_server.go:163` 不同）。呼叫方灌爆佇列後被 operator 撤銷，backlog 仍會被一路排空成新沙盒，已在跑的完全不受影響。 |
| I2 8 併發上限在並發提交下只是建議值 | `a2a_server.go:226-234` + `a2a_tasks.go:99-107` | `hasCapacity` 在 upsert 之後於同一 callback 內算，但剛 upsert 的 row 是 `submitted`，而 `HasCapacity` 走 `RunningCount` 只數 `working`。40 個並發請求會全部算出 `true` 並全部 dispatch。 |
| I3 sweep 第 2 步無身分守衛 | `a2a_lifecycle.go` 第 2 步（`sm.Stop` / `sm.RemoveWorkspace` / `os.RemoveAll(SandboxRoot(...))`） | 三個破壞動作對第 1 步記下的**確定性路徑**執行，中間沒有任何重新確認。第 3 步的四欄位比對（TaskID/State/Worktree/Session）是正確且完整的，但它只決定「要不要清欄位」——保住了帳，沒保住磁碟。合法的同 contextId 追問若落在這個窗口，新起的 session 會被殺、新建的 worktree 會被 `--force` 刪，而 row 完好地指向已不存在的東西，一路掛到 2 小時硬逾時。 |
| I4 sweep 在 driver 還活著時回收它的沙盒 | `main.go:325-334` 順序為 collect → sweep → drain → `EnsureSandboxDrivers` | `a2a_driver.go:271-277` 的註解自己寫明「Stop blocks until the goroutine has actually exited, which is precisely the guarantee reclamation needs」，但 wiring 把順序反過來，中間還隔著一個可同步阻塞數分鐘的 `DrainQueue`。sweep 刪掉 sandbox root 後，還活著的 driver 下一輪 `RunWorkerOnce` 第一件事就是 `Init(root)`（`worker.go:71`）把目錄樹重建回來，而第 3 步已把該 row 的 Session/Worktree 清空 → 每一次硬逾時取消就留下一個永不回收的目錄。同時 `git worktree remove --force` 是在 claude 行程正把該目錄當 cwd 時執行的。 |
| I5 task row 與稽核 log 無上限成長 | `a2a_tasks.go`（無任何移除路徑）、`a2a_audit.go`（無 rotation） | contextId 由呼叫方指定、1-128 字元；每次 `WithTasks` 是整檔讀+整檔寫（`a2a_store.go:24,31`），cycle 每 10 秒至少碰一次，於是每個 handler 的擁有權檢查都排在一次單調成長的 O(N) 讀寫後面。`AuditEntry.Summary` 是呼叫方原文未截斷，只受 1 MiB body cap 限制；`ReadAudit` 的 per-line scanner 上限是 1 MiB（`a2a_audit.go:53`），JSON 對控制字元做 6 倍展開，約 180 KB 控制位元組就能造出超長行，之後 `ReadAudit` 整份失敗。`MaxRetainedFailedSandboxes = 20`（`a2a_lifecycle.go:82`）只綁 worktree，不綁 row。 |
| I6 呼叫方拿不到任何結果 | `a2a_server.go:139-142`、`:325` | 只支援 `message/send`，回應永遠是 `submitted`（`task.State` 從頭到尾沒被改過，`Start` 收的是值複本）。排隊後 `DrainQueue` 的啟動失敗在 `a2a_lifecycle.go:59-61` 被 `continue` 整個吞掉。跑完寫進 `Detail` 的結果沒人讀得到，兩小時硬逾時也沒人通知。 |
| I7 沒有任何「沙盒死掉」的偵測 | `a2a_*.go` 內無 `has-session`；`SweepTimeouts` 只看時間戳 | 規格 `2026-08-05:124` 要求「session 不存在但任務未完成 → 標 `failed`，worktree 保留」。實際上機器重開或 session 被砍後，任務停在 `working`，driver 每輪失敗、每秒往 agent 頻道推一行錯誤（`a2a_driver.go:167-171`，無去重、無退避、無次數上限，最長兩小時約 7200 則），prompt 在 `requeueOrFail` 三次後進 `inbox/failed` 永久遺失，最後被判成 `canceled` 而非 `failed`——forensics 保留規則因此套錯邊。 |
| I8 認證失敗完全不留稽核 | `a2a_server.go:133-137` 在 `AppendAudit` 之前就 return | 對憑證做暴力嘗試會在 `a2a-audit.jsonl` 產生**零**行。對一個以「需要誰要求了什麼的持久紀錄」為存在理由的對外監聽器（`a2a_audit.go:10-11`），這是最該有的一筆。未知方法、格式錯誤的 params、未知/停用 agent、store 寫入失敗也同樣沒有紀錄。 |
| I9 agent / caller 建檔與核准在正式路徑上不存在 | `SaveAgents`(`a2a_agents.go:47`)、`Add`(:60)、`Remove`(:71)、`SaveCallers`(`a2a_callers.go:45`)、`Register`(:49)、`Approve`(:62)、`Revoke`(:73) | 全部零正式呼叫端：`handleRPC` 無註冊方法、`main.go` 的 case 清單無 a2a、`admin.go` 無 a2a 路由。`agents.json` 與 `callers.json` 只能手寫。 |
| I10 `CollectResults` 在持鎖 callback 內做檔案 I/O | `a2a_result.go:62-84` | `a2a_store.go:17-19` 明文規定 callback 內不得做慢工。`pendingResultFile` 對每個可轉 completed 的 row 做 `os.ReadDir` + 逐檔 `ReadJSON`，`moveFile` 也在鎖內。單次是毫秒級，但成本與 I5 的無上限 row 數同步成長，而這段時間 `tasksMu` 被 handler、executor、sweep 共用。sweep 刻意把 `LoadAgents` 提到鎖外（`a2a_lifecycle.go:182`）、把 tmux/git 留在鎖外，`CollectResults` 沒有比照。 |

### Minor（本輪一併處理）

- `callers.json` 為 `0644`（`SaveCallers` → `AtomicWriteJSON` → `AtomicWriteFile(path, payload, 0o644)`，`fileutil.go:16`），明文 bearer 憑證世界可讀。
- agent 名只在 `Add` 被 `a2aNameRe`（`a2a_agents.go:32`）驗證，而 `Add` 無正式呼叫端 → `LoadAgents` 不驗證。含 `/` 或 `..` 的名字會流進 `SessionNameFor` → `SandboxRoot`/`SandboxWorktree` 與 tmux session 名。
- agent 的 `ChannelID` 與某個 binding 的 channel 相同時，`dcRoute`（`supervisor.go`）會把該頻道的人類訊息吃進那個 `cc-` session。目前無寫入路徑可達，但這是「唯讀輸出」這個不變量唯一可能被打破的方式，而它現在靠慣例而非驗證維持。
- `p.TaskID` 未驗證、未設長度上限（`a2a_server.go` params）。不可達路徑或 session 名，但可讓呼叫方在 task store 裡塞 ~1 MiB blob。
- `pendingResultFile`（`a2a_result.go:36-46`）取 `outbox/pending` 裡**任一** `.json`，不比對來源；`ReadJSON` 失敗只 `continue`，無 log、不搬檔。`failed` 沙盒依 forensics 規則保留，同一 caller 之後重用該 contextId 時，殘留檔會立刻把新任務判為完成。
- `A2AConfig.Listen` 未對照 admin address 驗證，儘管 docstring 寫著 MUST differ（`config.go:111-122`）。
- 單一 sender goroutine 服務所有 agent 頻道（`a2a_output.go:65,72`），一個卡住的頻道最多佔住 12 秒並讓共用的 256 格 queue（`agentOutputQueueSize`，`a2a_output.go:16`）溢出丟行。

### review 與程式碼不一致之處（以程式碼為準）

1. **安全 review 的 M1 說「folder trust 實務上由 `autoAnswerSandboxConfirm` 打 `1` 處理」——不成立。** `autoAnswerSandboxConfirm` 只在 `classifyScreen(pane) == ScreenConfirm` 時動作（`a2a_driver.go:217`）。managed-settings 閘被 `classifyScreen` 判為 `ScreenLogin`（`screen.go:55`），永遠不會被它答到；資料夾信任框是否被判為 `ScreenConfirm` 也從未驗證過（測試用的是 `create SKILL.md?` fixture，`a2a_driver_test.go`）。所以「已經有替代緩解在跑」這個推論是錯的，第 2 層緩解目前對 X1 的主要形態完全無效。正確性 review 的 C2 才是對的。
2. **正確性 review 對 `a2a_callers.go` 的行號整體偏移。** 它標 `Authenticate` 在 `:175`、`Register/Approve/Revoke` 在 `:139,:152,:163`；實際分別是 `:85`、`:49`、`:62`、`:73`。結論（零正式呼叫端）成立，行號不可用。安全 review 標的 `:49-81` 才是對的。
3. **安全 review 的 S1 修法建議「接上 binding 分支即可」不足。** 即使 gate 找得到一個 binding，`permission.go:296` 對「worktree 內的 Edit/Write」本來就自動放行，`AutoApprove` 更是整包放行。所以沙盒的範圍判斷必須自己實作，不能靠複用 binding 分支——這直接決定了下面第三節的設計。
4. 併發 review 的 C-2 第二種後果（並發 `git worktree add` / `tmux new-session` 是否必然回非零）標記為未驗證，本規格不依賴它；第一種後果（no-op 路徑上的重複注入）是必然發生的，D2 的修法對兩者都有效。

---

## 二、五項已定案決策

以下五項由使用者決定，已定案。此處只記錄決策與推導出的後果，不重新討論替代方案。

### 決策 1：沙盒約束走 permission gate

gate 必須認得沙盒 cwd，並執行該任務的能力授權，**預設拒絕**。`cc-` binding 經過 gate 的行為必須一位元都不變。

後果：
- `permission.go` 會被編輯，但新分支必須插在 `LoadRegistry`（`permission.go:265`）**之前**，於是 `cc-` 完全走原路徑。需補一條回歸測試：以一個真實 binding 形狀的 root 跑 gate，斷言 `Edit`（worktree 內）、`Bash`、`AutoApprove` 的判定與改動前逐字相同。
- 沙盒的 gate **不得有任何等待路徑**——不寫 pending 檔、不問頻道、不等 timeout。它立刻回答。這是與 `cc-` 最大的行為差異，也正是 `2026-08-05:58`「執行當下不再詢問」的意思。

### 決策 2：能力授權是三個預設等級，集中定義，呼叫方只能選等級

呼叫方永遠不能自行組裝規則集。三個等級的具體內容見第三節「三個等級」，**該節需使用者確認後才可動工**。

一條規則固定且不可設定：**沙盒只能寫在自己的 worktree 內，之外一律拒絕。**

後果：
- 既有的 `capabilities` / `granted_capabilities` 字串清單**降級為路由標籤**，不再宣稱是執行期限制。dispatch 當下的比對（`a2a_server.go:188`）保留不動，但兩處欄位註解與 admin UI 文案都必須寫明「這不是沙盒權限，沙盒權限由 level 決定」。這是安全 review 對 S2 給的兩個選項中的後者，配合 level 一起成立。
- `callers.json` 的 `Caller` 增加 `grant_level`；`message/send` params 增加選用的 `level`。有效等級 = `min(請求的 level, caller.grant_level)`，請求未給則取 caller 的。請求高於授權 → `RPCForbidden`，且寫一筆稽核。等級序：`readonly < develop < full`。

### 決策 3：沙盒開機阻塞在三層都修

1. **拿掉沙盒 worktree template 的 `SessionStart` hook**，讓 managed-settings 閘根本不出現。
2. **接上 `EnsureFolderTrusted`**（已寫好、零正式呼叫端）。
3. **保留自動回答作為最後一道 backstop**，並正確分類 managed-settings 閘（目前被誤判為 login 畫面且只對 binding 處理）。

後果：
- 第 1 層必須在**不改變 `cc-` 行為**的前提下做。`agentSettings`（`worktree.go:116`）與 `EnsureAgentSettings` 一個字都不能改。做法見第四節 F1。
- 第 3 層**不得修改 `classifyScreen`**：`supervisor.go:174` 的登入 watchdog 依賴這兩個畫面被判為 `ScreenLogin`。driver 改為直接呼叫 `paneAwaitingManagedSettings` / `paneAwaitingLoginContinue`（`screen.go:180,174`）這兩個既有 helper。

### 決策 4：呼叫方以兩種方式得知結果——查詢方法 + 完成回呼

新增 A2A 標準的任務查詢方法，以及進入終止狀態時觸發的 callback。

**callback 的安全規則固定：目的地 URL 記在 caller 記錄裡、由 operator 設定，永遠不接受請求提供**——否則這台主機就成了 SSRF 跳板。只允許 HTTPS、不跟隨 redirect、不允許內網或 loopback 目的地。

後果：callback 失敗**絕不可以卡住任務**。任務狀態機與 callback 的成敗完全解耦，見第四節 F4。

### 決策 5：管理介面同時做 CLI 與 admin UI 頁面

涵蓋 agent 的 create/list/remove 與 caller 的 register/approve/revoke/set-level，加一個 admin UI 頁面。**撤銷必須對已排隊與執行中的工作生效，不只對新請求生效。**

後果（重要）：`a2a_store.go:10` 的註解寫著「Only `serve` writes tasks.json, so an in-process mutex is sufficient」。CLI 是另一個行程，若直接寫 `tasks.json` / `agents.json` / `callers.json` 就會打破這個不變量。因此：

> **所有 A2A 狀態的寫入，在 `serve` 執行中時一律經由 admin API 在 serve 行程內完成。** CLI 子命令是 `/api/a2a/*` 的薄客戶端（沿用 `cfg.Admin.Token`）。另提供 `--offline` 旗標直接改檔，但它必須先探 `/api/healthz`，探得到就拒絕執行並提示改用線上模式。

---

## 三、約束模型（本規格的核心）

新檔 `internal/channelagent/a2a_policy.go`（型別、等級定義、政策檔讀寫）與 `internal/channelagent/a2a_gate.go`（gate 的沙盒分支）。`permission.go` 只增加一段呼叫。

### 3.1 gate 如何辨識沙盒

判別依據是 **`registryRoot`，不是 `cwd`**。`RunPermissionGate(ctx, registryRoot, in, out, timeout)`（`permission.go:254`）的 `registryRoot` 來自 `CC_REGISTRY_ROOT`（`main.go:88-90`），而該值對兩種 session 是截然不同的：

| session | `CC_REGISTRY_ROOT` |
|---|---|
| `cc-<name>` binding | `<root>`（`.channel-agent` 本身） |
| `aa-<agent>-<ctx>` 沙盒 | `<root>/sandboxes/aa-<agent>-<ctx>`（`a2a_executor.go:26`） |

```go
// SandboxSessionFromRegistryRoot 從 registryRoot 反推沙盒 session 名。
// 兩個條件必須同時成立，缺一即視為非沙盒。
func SandboxSessionFromRegistryRoot(registryRoot string) (session string, ok bool) {
    clean := cleanAbs(registryRoot)
    if filepath.Base(filepath.Dir(clean)) != "sandboxes" {
        return "", false
    }
    base := filepath.Base(clean)
    if !strings.HasPrefix(base, "aa-") {
        return "", false
    }
    return base, true
}
```

`RunPermissionGate` 的插入點在讀完 hook input、**在 `LoadRegistry` 之前**：

```go
if session, ok := SandboxSessionFromRegistryRoot(registryRoot); ok {
    return runSandboxGate(ctx, registryRoot, session, hi, out)
}
// ↓ 以下一行不動，cc- 完全走原路徑
reg, err := LoadRegistry(registryRoot)
```

不用 `cwd` 判別的理由：`hi.CWD` 是 Claude 自己回報的，沙盒內 `cd` 到別處就會改變它；`registryRoot` 來自 tmux 環境變數，沙盒內的工具呼叫改不到 hook 行程的環境。

### 3.2 政策放在哪裡

**`<root>/a2a-policies/<session>.json`，mode `0600`，由 `SandboxExecutor.Start` 在 `Sessions.Start` 之前寫入。**

```go
type SandboxPolicy struct {
    Session    string     `json:"session"`
    ContextID  string     `json:"context_id"`
    Agent      string     `json:"agent"`
    CallerID   string     `json:"caller_id"`
    Level      GrantLevel `json:"level"`     // readonly | develop | full | revoked
    Worktree   string     `json:"worktree"`  // 絕對路徑
    SandboxRoot string    `json:"sandbox_root"`
    WrittenAt  string     `json:"written_at"`
}
```

三個設計取捨，明確記錄：

1. **不從 `tasks.json` 讀。** gate 是每次工具呼叫都被 spawn 的獨立行程，讀不到 `tasksMu`；而 `tasks.json` 是整檔 O(N)、N 由呼叫方決定。每個 session 一個小檔是 O(1) 且與 task store 的成長無關。
2. **不放在 `<root>/sandboxes/<session>/` 底下。** 那是沙盒自己的 root（inbox/outbox 就在裡面），把政策放在受限主體看得見的目錄裡是自找麻煩。放在 `<root>/a2a-policies/` 讓它落在「worktree 之外」這條固定規則的保護範圍內。
3. **`full` 級沙盒的 Bash 仍能改寫它。** 這不構成提權（`full` 本來就等同 `cc-` session），但需記錄為已知殘留，見第六節開放問題 5。`readonly` / `develop` 改不到：它們的 Edit/Write 受範圍規則擋下，Bash 受指令允許清單 + metacharacter 禁令擋下。

政策檔的寫入與刪除：
- 寫入：`SandboxExecutor.Start` 在 `EnsureWorkspace` 成功之後、`Sessions.Start` 之前。寫入失敗 = dispatch 失敗（`markFailed`），**不可以**降級成「先開起來再說」。
- 撤銷：`caller revoke` / `agent disable` 把該 session 的政策檔整個覆寫成 `{"level":"revoked"}`，於是還沒被殺掉的 in-flight 工具呼叫立刻開始被拒。
- 刪除：sweep 回收該 sandbox 時一併 `os.Remove`。清不掉只 log，不影響回收判定（下一趟會重試）。

### 3.3 評估順序與每一條失敗路徑

`runSandboxGate` 的完整判定順序。任何一步做不出「允許」的結論就是拒絕；**沒有任何路徑會 fail-open**。

| 步 | 條件 | 判定 | gate log outcome |
|---|---|---|---|
| 1 | 政策檔不存在 | deny：`a2a gate: 沒有 <session> 的政策檔` | `denied_no_policy` |
| 2 | 政策檔讀取/解析失敗、或 `Level` 不是三個已知值之一 | deny：`a2a gate: 政策檔無法解讀` | `denied_bad_policy` |
| 3 | `Level == "revoked"` | deny：`a2a gate: 呼叫方已被撤銷` | `denied_revoked` |
| 4 | `hi.ToolName` 是 `Edit` / `Write` / `NotebookEdit`：以 `filePathOf`（`permission.go:76`）取路徑，`cleanAbs` 後要求 `inScope(policy.Worktree, path) \|\| inScope(filepath.Join(policy.SandboxRoot,"outbox"), path)` | 不在範圍 → deny；在範圍 → 進第 6 步 | `denied_out_of_scope` |
| 5 | `filePathOf` 取不到路徑（工具輸入沒有 `file_path`） | deny（無法判斷範圍就不放行） | `denied_no_path` |
| 6 | 等級規則（見 3.4） | allow / deny | `allowed` / `denied_level` / `denied_bash_rule` / `denied_mcp` |
| 7 | 以上都沒命中的工具名 | deny（hook 只裝六個 matcher，這是縱深防禦） | `denied_unknown_tool` |

其他失敗路徑：
- **gate log 寫入失敗**：判定照舊生效，另寫一行 stderr。不因為記錄失敗而改判——磁碟抖一下不該讓一個任務卡死。（列為開放問題 6。）
- **任何非預期 error 或 panic**：`recover` 後輸出 `hookDecisionJSON(false, "a2a gate: internal error")`。deny。
- **政策檔的 `Worktree` 為空字串**：視同第 2 步的壞政策，deny。（`inScope("", path)` 的語意不可以被信任。）

gate log 是**獨立檔** `<root>/a2a-gate.jsonl`，欄位 `{at, session, context_id, caller_id, agent, level, tool, outcome, detail}`。與 `a2a-audit.jsonl` 分開：後者是委派紀錄（誰要求了什麼），前者是執行期判定紀錄（沙盒想做什麼），量級差兩個數量級，混在一起會把委派紀錄淹掉。兩者共用同一套 rotation 規則（見 F6）。寫入沿用 `AppendAudit` 的 `O_APPEND` 單次 write 形式——Linux 上 < 4KB 的 append 是原子的，多個並發 gate 行程不會互相截斷。

### 3.4 三個等級的內容 ⚠️ 需使用者確認

集中定義在 `a2a_policy.go`，呼叫方只能選等級，不能改內容。

```go
type GrantLevel string

const (
    GrantReadOnly GrantLevel = "readonly"
    GrantDevelop  GrantLevel = "develop"
    GrantFull     GrantLevel = "full"
    GrantRevoked  GrantLevel = "revoked" // 內部狀態，不可授予
)
```

**共通、不可設定的規則（三級皆適用）**

- 可寫範圍 = 自己的 worktree ∪ 自己 `sandboxes/<session>/outbox/`，此外一律拒絕。**含 `full`**。
  - `outbox/` 這個例外是必要的，不是放寬：注入的 prompt 指示沙盒把結果寫成 `.tmp` 再 rename 進 outbox（`adapters.go:388-392`），`CollectResults` 靠它判定完成。沒有這個例外，**沒有任何任務能夠完成**。`cc-` gate 有一模一樣的例外，理由也一模一樣（`permission.go:290-296` 的註解）。
  - session scratchpad（`<TMPDIR>/claude-<uid>/<slug>`，`cc-` gate 的第三個放行區）**不放行**。沙盒沒有寫草稿給人看的需求。（列為開放問題 2。）
- `Read` / `Glob` / `Grep` 不在 hook 的六個 matcher 內，因此三級都不經過 gate。這代表**三級都能讀主機上任何檔案**，包括 `~/.claude/.credentials.json`、其他 binding 的 worktree、`.env`。這是本輪不做容器隔離的直接後果，必須寫在文件裡而不是假裝不存在。（`readonly` 之所以仍有意義，是它擋掉所有寫入與所有對外動作。）
- Bash 的判定只能做到「首個 token 的允許清單 + 禁止 shell metacharacter」。含 `;` `&&` `||` `|` `` ` `` `$(` `>` `>>` `<` 或換行的指令一律 deny（`readonly` 與 `develop`）。**這不保證路徑侷限在 worktree 內**——`rm -rf /home/conray/project/x` 的首 token 是允許的 `rm`。真正的路徑侷限需要容器層隔離，本輪不做。

**Level 1 — `readonly`（唯讀）**

| 工具 | 判定 |
|---|---|
| `Edit` / `Write` / `NotebookEdit` | **全部 deny**（含 worktree 內；outbox 例外仍成立，否則無法回報結果） |
| `Bash` | 僅允許首 token ∈ `{git, ls, cat, head, tail, wc, find, rg, grep, file, stat, du, tree}`；`git` 的子命令必須 ∈ `{status, log, diff, show, branch, remote, describe, blame, ls-files, rev-parse}`；其餘 deny |
| `WebFetch` / `WebSearch` | deny |
| `mcp__*` | deny |

用途：程式碼審閱、回答「這個 repo 怎麼做 X」、產出報告。

**Level 2 — `develop`（可改自己的 worktree、可跑測試）**

| 工具 | 判定 |
|---|---|
| `Edit` / `Write` / `NotebookEdit` | 在 worktree ∪ outbox 內 → allow；之外 → deny |
| `Bash` | 首 token ∈ `readonly` 清單 ∪ `{go, make, npm, pnpm, yarn, node, bundle, rake, rspec, pytest, python, python3, cargo, mkdir, cp, mv, rm, touch, chmod, sed, awk, sort, uniq, diff, patch, test}`；`git` 的子命令額外允許 `{add, commit, checkout, switch, restore, stash, fetch, merge, rebase, push}`；`git push` 額外要求 argv 不含 `--force` / `-f` / `--delete` / `:`（保護分支命名空間 `aa/<session>`，`a2a_executor.go:39`）；`sudo` 一律 deny |
| `WebFetch` / `WebSearch` | allow（見開放問題 1） |
| `mcp__*` | **deny**。MCP server 直通 production（planetscale、openobserve、Atlassian、Slack），一個被委派的開發任務不該無聲碰到它們。 |

用途：實作一張票、修一個 bug、跑測試、推分支。這是預期中最常用的等級。

**Level 3 — `full`（等同 `cc-` session）**

| 工具 | 判定 |
|---|---|
| `Edit` / `Write` / `NotebookEdit` | 在 worktree ∪ outbox 內 → allow；之外 → **deny**（唯一與 `cc-` 不同之處） |
| `Bash` | allow（無清單、無 metacharacter 限制） |
| `WebFetch` / `WebSearch` | allow |
| `mcp__*` | allow |

`full` 與 `cc-` 的唯一差別是那條寫入範圍規則：`cc-` session 遇到範圍外的寫入會問人並可能拿到核准，沙盒沒有人可以問，依 `2026-08-05:58`「範圍外自動拒絕」。

`full` 等同把主機交出去（Bash 無限制 = 能讀憑證、能 `curl | sh`）。**只授予信任程度等同 operator 本人的呼叫方。** 建議：`a2a.listen` 為非 loopback 時一律禁止 `full`（見開放問題 4）。

### 3.5 `cc-` 不變的保證

- `permission.go` 的沙盒分支插在 `LoadRegistry` 之前並直接 `return`，`cc-` 路徑的每一行都不動。
- 需補回歸測試 `TestPermissionGateBindingPathUnchanged`：以 `t.TempDir()` 建一個含 `bindings.json` 的 root，對 `Edit`（worktree 內 / 外）、`Bash`、`AutoApprove`、`mcp__x` 各跑一次 gate，斷言輸出 JSON 與改動前逐字相同（把期望值寫死在測試裡）。
- 需補測試 `TestSandboxGateNeverAsksChannel`：斷言沙盒分支不會在 `permissions/pending` 底下產生任何檔案，且在 timeout 遠大於 0 的情況下**立即**返回。

---

## 四、其餘必修項目

每一條都已對照原始碼確認，皆不需使用者輸入。

### F1 — 沙盒 worktree template 拿掉 `SessionStart` hook（決策 3 第 1 層）

`worktree.go` 新增 `sandboxAgentSettings` 常數：與 `agentSettings`（`worktree.go:116`）**逐字相同，只刪掉 `SessionStart` 區塊**，六條 `PreToolUse` 一條不少。新增 `EnsureSandboxSettings(dir)`。

`StartTmuxClaude` 的函式體抽成 `startTmuxClaudeWith(ctx, session, cwd, registryRoot string, ensure func(string) error)`；`StartTmuxClaude` 維持現有簽章與行為，傳 `EnsureAgentSettings`。新增 `StartTmuxClaudeSandbox` 傳 `EnsureSandboxSettings`。`TmuxSessionManager.Start`（`a2a_session.go:30`）改呼叫後者。

需補測試：斷言 `EnsureAgentSettings` 寫出的內容仍含 `"SessionStart"`，且 `EnsureSandboxSettings` 寫出的內容不含 `"SessionStart"` 但含全部六個 matcher。

### F2 — 接上 `EnsureFolderTrusted`（決策 3 第 2 層）

`SessionManager` 介面（`a2a_session.go:11`）新增 `TrustFolder(ctx context.Context, worktree string) error`。

- `TmuxSessionManager.TrustFolder`：`abs, _ := filepath.Abs(worktree)`；`return EnsureFolderTrusted(ClaudeConfigPath(), abs)`。
- `FakeSessionManager.TrustFolder`：只記錄呼叫，**絕不碰真實檔案**。

> 走介面而不是直接呼叫是強制要求：`EnsureFolderTrusted` 寫的是 `~/.claude.json`，那是這台機器上所有 claude 行程共用的活檔。一個直接呼叫它的單元測試會改寫 operator 的線上設定。

呼叫點：`SandboxExecutor.Start` 中 `EnsureWorkspace` 成功之後、寫政策檔之前。失敗只 `log.Printf` 不中止——它只是省一個對話框，不是必要條件（第 3 層 backstop 仍在）。

### F3 — driver 的畫面處理（決策 3 第 3 層）

`SandboxDriver.loop`（`a2a_driver.go:143` 起）每輪**只 capture 一次 pane**（目前是 `autoAnswerSandboxConfirm` 內部一次；不得因為新增分支而變成兩次，`capture-pane` 是 fork/exec，8 個沙盒 = 每秒 8 次），然後：

```
low := strings.ToLower(stripANSI(pane))
switch {
case paneAwaitingManagedSettings(low):     // screen.go:180
    TmuxInjector{Session: session}.SelectTrustSettings(ctx)   // adapters.go:265
    → 本輪 skip RunWorkerOnce
case paneAwaitingLoginContinue(low):       // screen.go:174
    送 Enter
    → 本輪 skip RunWorkerOnce
case classifyScreen(pane) == ScreenConfirm:
    既有的 autoAnswerSandboxConfirm（含 lastConfirmHash 去重）
    → 本輪 skip RunWorkerOnce
case classifyScreen(pane) == ScreenLogin:  // 真的登出了
    任務標 TaskFailed，Detail = "sandbox session needs login"；停止本 driver
default:
    RunWorkerOnce(...)
}
```

**「命中任一畫面分支就 skip 本輪 `RunWorkerOnce`」是這條修正真正的重點**：`paneBusy` 不把 `ScreenLogin` 算成忙碌（`adapters.go:208-217`），`RunWorkerOnce` 會把 prompt 打進核准畫面並回報成功（`adapters.go:94` 的驗證條件在無輸入框的畫面上必然為 false），prompt 就此消失。skip 掉才擋得住。

沙盒**永遠不驅動登入流程**：那是 operator 的事，一個沙盒去操作 `/login` 會動到全機共用的憑證。

`classifyScreen` 一個字都不改。

### F4 — `tasks/get` 與完成回呼（決策 4）

**查詢方法。** `handleRPC` 新增 `case "tasks/get"`：params `{"contextId": "...", "taskId": "..."}`（`contextId` 必填）。認證後，若該 row 的 `CallerID` 不等於已認證的 caller → `RPCForbidden`，訊息與「查無此 row」完全相同（不洩漏存在性）。回應：

```json
{"contextId":"…","taskId":"…","state":"working","branch":"aa/aa-pm-abc",
 "startedAt":"…","completedAt":"…","detail":"…"}
```

`detail` 是沙盒自撰文字，回應中截斷至 64 KiB。這是對安全 review「控制 10」（沙盒文字不流出 HTTP）的**刻意放寬**，因為沒有它就沒有交付；記錄於此並列入開放問題 8。

**完成回呼。**
- `Caller` 新增 `callback_url`、`callback_token`，只能由 operator 經 CLI / admin API 設定。`message/send` params 若出現 `callbackUrl` / `webhookUrl` 之類欄位 → `RPCInvalidParams` 拒絕整個請求（不是忽略）。
- 目的地驗證在**設定當下與觸發當下各做一次**：scheme 必須是 `https`；解析主機名，若任何一個回傳 IP 落在 loopback（`127.0.0.0/8`、`::1`）、私有（`10/8`、`172.16/12`、`192.168/16`、`fc00::/7`）、link-local（`169.254/16`、`fe80::/10`）或未指定位址 → 拒絕；主機名以 `.local` / `.internal` 結尾 → 拒絕。
- **DNS rebinding 防護**：解析一次、檢查所有回傳 IP、然後用自訂 `DialContext` 直接連那個已檢查過的 IP，`Host` header 保留原主機名。不可以用會重新解析的 `http.Get`。
- `CheckRedirect` 回 `http.ErrUseLastResponse`——不跟隨任何轉址。
- `Timeout: 10 * time.Second`。`POST`，`Content-Type: application/json`，body = `tasks/get` 的回應形狀外加 `"event":"task.terminal"`。`callback_token` 非空時帶 `X-A2A-Callback-Token` header。
- **重試**：最多 3 次，間隔 5s / 30s / 120s，只對傳輸錯誤與 `5xx`、`429` 重試。`2xx` 視為成功，其他 `4xx` 視為永久失敗立刻放棄。
- **絕不卡住任務**：由一條專屬 goroutine 消費一個容量 256 的 channel；佇列滿就直接標記 `callback_state = "dropped"` 並丟棄。任務狀態機不看 callback 結果。任何時候都不得在持有 `tasksMu` 時發 callback。
- **觸發點只有一處**：A2A cycle 在 collect / sweep 之後掃出「terminal 且 `callback_state == ""`」的 row 入列並標 `pending`。不在 `CollectResults` / `SweepTimeouts` / `markFailed` 三處各接一次——那三處都在鎖內。
- serve 重啟後仍是 `pending` 的會被重送一次（at-least-once，callee 需對 `taskId` 冪等；列入開放問題 7）。

### F5 — 管理 CLI 與 admin UI（決策 5）

**CLI**，比照 `runManageCommand`（`main.go:445`）的旗標解析風格，新增 `case "a2a":` 與第二層動詞：

```
claude-cron a2a agent add <name> --project=<dir> [--description=…] [--capabilities=a,b]
                                 [--channel=<id>] [--max-level=develop] [--enabled]
claude-cron a2a agent list
claude-cron a2a agent remove <name>
claude-cron a2a agent enable|disable <name>
claude-cron a2a caller register <id> [--credential=…]     # 未給則產生 32-byte base64url，只印一次
claude-cron a2a caller list                               # 永遠不印 credential
claude-cron a2a caller approve <id> --level=readonly|develop|full [--capabilities=a,b]
claude-cron a2a caller revoke <id>
claude-cron a2a caller set-level <id> --level=…
claude-cron a2a caller set-callback <id> --url=https://… [--token=…]
claude-cron a2a task list [--state=…]
claude-cron a2a task cancel <contextId>
claude-cron a2a audit [--limit=200]
```

全部接受 `--root`。全部預設走 admin API（`cfg.Admin.Listen` + `cfg.Admin.Token`）；`--offline` 才直接改檔，且必須先探 `/api/healthz`，探得到就拒絕（理由見決策 5 的後果）。

**撤銷的完整語意**（`caller revoke`，在 serve 行程內一次完成）：

1. `Revoke(id)` + `SaveCallers`。
2. `WithTasks`：所有 `CallerID == id` 且非終止的 row → `TaskCanceled`，`Detail = "caller revoked"`，`CompletedAt = now`；收集其 session / worktree。
3. 鎖外：對每個 session 先 `driver.Stop(session)`（阻塞到 goroutine 真的結束），再 `TmuxSessionManager.Stop`。
4. 每個 session 的政策檔覆寫成 `{"level":"revoked"}`——在 session 真的死掉之前，in-flight 的工具呼叫就已經開始被 gate 拒絕。
5. worktree 回收交給下一趟 sweep（它已經會回收 `canceled` 且仍持有 session/worktree 的 row）。
6. 寫一筆稽核 `outcome: "revoked"`。

`agent disable` 對該 agent 的非終止 row 做同樣的 2-6 步。

**admin API**，插在 `admin.go` 既有的 `switch` 內，形狀比照 `/api/bindings`：

```
GET|POST   /api/a2a/agents
DELETE     /api/a2a/agents/<name>
POST       /api/a2a/agents/<name>/enable | /disable
GET|POST   /api/a2a/callers
POST       /api/a2a/callers/<id>/approve | /revoke | /level | /callback
GET        /api/a2a/tasks
POST       /api/a2a/tasks/<contextId>/cancel
GET        /api/a2a/audit?limit=200        ← 這給了 ReadAudit 第一個正式呼叫端
GET        /api/a2a/gate-log?session=…&limit=200
```

全部走既有的 `h.authorized(r)`。**任何 GET 都不得回傳 `credential` 或 `callback_token`**，改回 `has_credential: true` / `has_callback: true`；credential 只在 `POST /api/a2a/callers` 的回應裡出現一次。

`cfg.A2A.Enabled == false` 時 `/api/a2a/*` 一律 404。

**admin UI**：`web/admin/src/App.svelte` 的 `nav` 陣列新增 `{ id: 'agents', key: 'nav.agents', href: '#/agents' }`，新檔 `web/admin/src/Agents.svelte`（三個分頁：Agents / Callers / Tasks，各對應上面的路由；Tasks 頁顯示 state、level、started、branch 與一個取消按鈕）。`nav` 項目在 `/api/config` 回報 `a2a.enabled == false` 時隱藏。i18n key 依 `lib/i18n.svelte.js` 既有慣例補齊。

### F6 — 逐條缺陷修正

| 代號 | 對應發現 | 要求的行為 |
|---|---|---|
| **D1** | C1 | 擁有權檢查在同一個 `WithTasks` callback（`a2a_server.go:227`）內增加：`existing.Agent != p.Agent` → 回 `errContextAgentSwitch` → `RPCForbidden`「contextId 已綁定至 agent X」+ 稽核 `forbidden_agent_switch`。**拒絕而非拆除**：在 handler 內拆掉舊沙盒需要在鎖內碰 tmux / git，違反 `a2a_store.go:17-19`。測試：先送 `(pm, ctx)` 再送 `(codereview, ctx)`，斷言第二次被拒且 `FakeSessionManager.Started` 長度為 1。 |
| **D2** | C2 + I2 | 新增狀態 `TaskDispatching = "dispatching"`。`CanTransition`：`submitted→dispatching`、`dispatching→{working, failed, canceled}`。`RunningCount` 同時計入 `working` 與 `dispatching`。handler 與 `DrainQueue` **都**必須在 `WithTasks` 內原子地「只把仍是 `submitted` 的 row 翻成 `dispatching`」才取得派送權；`DrainQueue` 因此必須從 `LoadTasks`（`a2a_lifecycle.go:46`）改為 `WithTasks`。容量預約與這次翻轉在同一個 critical section 完成——這同時修掉 I2 的並發突破上限。`EnsureSandboxDrivers` 仍只驅動 `working`。停留在 `dispatching` 超過 `DispatchStaleAfter = 5 * time.Minute` 的 row 由 sweep 判為派送中崩潰 → `failed`。 |
| **D3** | I3 | 兩件事都要做：(a) 新增以 session 名為 key 的行程內鎖 `sessionLocks`，`SandboxExecutor.Start` 與 sweep 第 2 步都必須持有，於是重新提交不可能在拆除進行中重建同名 session；(b) 第 2 步在對每個 candidate 動手**之前**，用一次短的 `WithTasks` 重新確認該 contextId 的 row 仍是同一身分（TaskID / State / Worktree / Session 四欄位，與第 3 步同一組比較），不符就跳過該 candidate。只做 (b) 仍有窗口，只做 (a) 擋不住跨行程；兩者一起才完整。 |
| **D4** | I4 | `SweepTimeouts` 增加參數 `stopper SandboxStopper`（介面只有 `Stop(session string)`，`nil` 代表不停，供測試用）。第 1 步收集 candidate 時一併收集 session；**第 2 步動手之前，先對每個 candidate 呼叫 `stopper.Stop(session)`**（`SandboxDriver.Stop` 已保證阻塞到 goroutine 真的結束，`a2a_driver.go:271-277`）。`main.go` 把 driver 傳進去。cycle 順序維持 collect → sweep → drain → `EnsureSandboxDrivers`。 |
| **D5** | I5 | (a) `MaxTaskRows = 500`、`TaskRetention = 14 * 24 * time.Hour`：每個 cycle 結束時，終止狀態的 row 依 `CompletedAt` 由新到舊保留前 500 筆，且丟棄超過 14 天者；**非終止的 row 永不丟棄**。(b) 寫入時截斷：`Prompt` 8 KiB、`Detail` 64 KiB。(c) `AuditEntry.Summary` 截斷至 512 個 rune，尾綴 `…（截斷）`。(d) `AppendAudit` 在檔案超過 `AuditMaxBytes = 32 MiB` 時先 rename 成 `<name>.1`（只留一代）再 append；`a2a-gate.jsonl` 共用同一機制。(e) `ReadAudit` 改用可跳過超長行的讀法，一行壞掉不得讓整份 log 無法讀取。(f) `p.TaskID` 長度超過 128 → `RPCInvalidParams`。 |
| **D6** | I1 | `DrainQueue` 每次呼叫載入一次 `callers.json` 與 `agents.json`；row 的 caller 不是 `approved`、或 agent 不存在 / `!Enabled`、或 row 記錄的 level 高於該 caller 目前的 `grant_level` → 轉 `TaskFailed` 並寫明 `Detail` + 一筆稽核，**不是靜默 `continue`**。`SandboxExecutor.Start`（`a2a_executor.go:132-137`）補上 `!agent.Enabled` 的拒絕。 |
| **D7** | I7 | `SessionManager` 新增 `Alive(ctx, session) (bool, error)`（`tmux has-session -t`）。sweep 對每個處於 `working` / `dispatching` 且已停留 ≥ `LivenessGrace = 2 * time.Minute` 的 row 檢查存活；不存活 → `TaskFailed`，`Detail = "sandbox session vanished"`，**worktree 保留**（依 `2026-08-05:124` 的 forensics 規則，並受 `MaxRetainedFailedSandboxes` 上限約束）。`FakeSessionManager.Alive` 回傳腳本化的值，測試不得起 tmux。driver 端：連續 3 次 `RunWorkerOnce` 失敗且 `Alive` 為 false → 停止驅動，交給 sweep 判定。 |
| **D8** | I8 | `handleRPC` 在認證失敗時寫稽核 `outcome: "unauthorized"`，`CallerID` 留空，另記 `credential_fp`（憑證的 SHA-256 前 8 個 hex 字元，**絕不記憑證本身**）與來源位址。未支援方法 / params 解析失敗 / contextId 格式錯誤 / 未知或停用 agent → `outcome: "bad_request"`。`unauthorized` 條目以來源 IP 為 key 限流至每秒 1 筆（行程內 map，上限 1024 個 key，滿了就整批清空），避免 log 被灌爆。註：`2026-08-06` 規格把 pre-auth 稽核列為「明確不做」，本輪推翻該決定——I8 顯示這是對外監聽器最該有的一筆紀錄。 |
| **D9** | I10 | `CollectResults` 把 `os.ReadDir` / `ReadJSON` / `moveFile` 全部移到 `WithTasks` **之外**：先在鎖外掃出 `contextId → (path, text)`，`WithTasks` 內只做純記憶體的狀態轉移，搬檔在鎖後執行；搬檔失敗只 log，不回退已完成的判定。比照 sweep 既有的做法（`a2a_lifecycle.go:182` 把 `LoadAgents` 提到鎖外）。 |
| **D10** | Minor 群 | (a) `SaveCallers` 改用新的 `AtomicWriteJSONMode(path, v, 0o600)`；`AtomicWriteJSON` 的預設 `0644` 不動（`bindings.json` 等共用）。(b) `LoadAgents` 對每個 agent 名跑 `a2aNameRe`，不符者跳過並 log。(c) `LoadAgents` 另讀 `bindings.json`（唯讀，不寫），任何 `ChannelID` 與某 binding 相同的 agent 一律跳過並 log——把「唯讀輸出」從慣例變成驗證。(d) `A2ATask` 新增 `LastMessageID`（記下最後注入的 `MessageID`）；`pendingResultFile` 只接受 `job_id` / `request_id` 對得上的結果檔，殘留檔不得完成新任務。(e) `pendingResultFile` 對 `ReadJSON` 失敗改為 log + 搬去 `outbox/failed`，不再靜默 `continue`。(f) `LoadConfig` 在 `A2A.Listen == Admin.Listen` 時拒絕啟動並印出明確錯誤。(g) driver 對 agent 頻道的錯誤行去重與退避：同一 session 相同錯誤 60 秒內最多一行，且每 session 每分鐘最多 60 行。 |

---

## 五、明確不做

- **不改動 `cc-` 機制**：`bindings.json`、`registry.go`、`supervisor.go`、`reap.go` 不動。`permission.go`、`worktree.go`、`a2a_session.go` 會被編輯，但 `cc-` 路徑的行為必須逐位元不變，且各補一條把期望輸出寫死的回歸測試。
- **不改 `classifyScreen`**（`screen.go`）：`supervisor.go:174` 的登入 watchdog 依賴它現有的分類。
- **不做容器層隔離**（使用者已否決）。DB、Docker、cache 仍為共用，這是**已知且接受的限制**。直接後果，必須寫進使用者文件：三個等級都能**讀**主機上任何檔案（`Read`/`Glob`/`Grep` 不經過 gate）；`develop` 的 Bash 侷限只到「指令名 + 無 metacharacter」，不保證路徑侷限在 worktree 內。
- **agent 頻道永遠不 ingest**。不得把 agent 的 `ChannelID` 註冊進任何 poll / push / gateway ingest 路徑。這是安全要求：吃進使用者輸入，任何能在 Discord 打字的人就能直接對沙盒下指令，繞過整個 A2A 認證與能力授權。D10(c) 把它從慣例升級為驗證。
- **confirm 自動回答只適用於 `aa-` session**。`cc-` 的 confirm watchdog（`supervisor.go`）行為完全不動，仍然問人。現有的兩層前綴檢查（`a2a_driver.go:97` 與 `:213`）保留。
- **不做 `tasks/cancel` RPC 方法**：取消由 operator 經 CLI / admin 執行。呼叫方自助取消屬獨立範圍決策。
- **不做自動重試**（沿用 `2026-08-05:122`）。
- **不做開放自助註冊**：`Register` 仍只由 operator 觸發，`2026-08-05:56` 的「Discord 按鈕核准」流程不在本輪。
- **不修 graceful shutdown**（`main.go:188` `supCtx := context.Background()` 永不取消，因此 `defer sink.Wait()` / `defer driver.StopAll()` 在正式環境走不到）。這是既有問題、非本分支引入，且行程被 systemd 殺掉時 OS 會回收，重啟後 `EnsureSandboxDrivers` 與 `requeueProcessing` 會重建狀態。
- **不改 agent card 的認證**：未認證可讀是 A2A 的設計，且它只曝光 `Enabled` agent 的 name/description/tags，不含 `ProjectDir` / `ChannelID`（`a2a_card.go:22-38`）。

### 測試約束（硬性）

沿用前兩份規格並強化：測試**永遠不得**啟動 tmux session、`claude` 行程、真實 `git`，或發出真實 Discord / 網路呼叫。真實 `claude` 行程另外會消耗使用者的付費訂閱額度。本輪新增兩條：

- 測試**不得寫真實的 `~/.claude.json`**。`EnsureFolderTrusted` 一律經 `SessionManager.TrustFolder` 由 `FakeSessionManager` 攔下；直接測 `EnsureFolderTrusted` 時必須傳 `t.TempDir()` 內的 configPath。
- callback 測試一律打 `httptest.Server`；SSRF 防護的測試用假解析器注入 IP，不做真實 DNS 查詢。

另補三條併發 review 指出「目前完全沒有涵蓋」的測試（現有 `-race` 乾淨，但整個 a2a 測試套件只有兩處真的開並發，對每一個 Critical 都零涵蓋力）：

1. 同時起 `httptest` handler 與 `DrainQueue` 對同一 root 打同一個 contextId，斷言 `FakeSessionManager.Injected` 只有一則。
2. N 條 goroutine 送 N 個不同 contextId，斷言 `len(FakeSessionManager.Started) <= 8`。
3. 在 `FakeSessionManager.OnRemove` 內模擬同 contextId 重新提交，斷言 sweep 第 2 步**沒有**動到新身分的 session / worktree（現有的 `TestSweepSkipsRowChangedDuringTeardown` 只驗第 3 步的帳面守衛）。

---

## 六、待使用者確認的開放問題

1. **三個等級的具體內容（第 3.4 節整節）。** 尤其三點：(a) `develop` 是否放行 `WebFetch` / `WebSearch`（本規格暫定放行）；(b) `develop` 是否放行 `git push`（暫定放行，並限制在 `aa/<session>` 命名空間、禁 `--force`/`--delete`/refspec）；(c) `full` 是否放行 `mcp__*`（暫定放行）。
2. **可寫範圍是否包含 `<sandboxRoot>/outbox/`。** 本規格認定「必須包含」——不包含就沒有任何任務能回報完成，`CollectResults` 永遠收不到東西。同時暫定**不**包含 session scratchpad（`cc-` gate 的第三個放行區）。若實跑後發現 Claude Code 的 harness 硬性要求 scratchpad，再放寬。
3. **`agents.json` 是否新增 `max_level`**（agent 端的等級上限，有效等級 = `min(請求, caller.grant_level, agent.max_level)`）。本規格已在 CLI 預留 `--max-level`，但若不需要可拿掉。
4. **`a2a.listen` 為非 loopback 時是否一律禁止 `full`。** 本規格建議禁止：`full` 等同交出主機，對外開放時「取得一個 bearer 字串」就等於「取得主機」。
5. **政策檔位置與 `full` 的殘留風險。** 政策檔在 `<root>/a2a-policies/<session>.json`（`0600`）；`readonly` / `develop` 改不到它，但 `full` 級沙盒的無限制 Bash 可以改寫它。這不構成提權（`full` 本來就等同 `cc-`），但需確認可接受。
6. **gate log 寫入失敗時維持原判定**（不因記錄失敗而改判為拒絕）。本規格暫定如此，理由是磁碟抖動不該讓任務卡死；但這代表在磁碟滿的情況下會有未留紀錄的放行。
7. **callback 的 at-least-once 語意**：serve 重啟後仍是 `pending` 的會被重送一次，callee 必須對 `taskId` 冪等。
8. **`tasks/get` 回傳沙盒自撰的 `detail`**（上限 64 KiB）給呼叫方，是對安全 review「沙盒文字不流出 HTTP」這條既有性質的刻意放寬。沒有它就沒有交付，但請確認。
9. **保留與門檻的具體數值**：`MaxTaskRows = 500`、`TaskRetention = 14d`、`AuditMaxBytes = 32 MiB`（各留一代）、`DispatchStaleAfter = 5m`、`LivenessGrace = 2m`、callback 重試 `3 次 / 5s / 30s / 120s`、gate log 與 audit 共用 rotation。這些是本規格自行判斷的值，未經使用者裁示。
