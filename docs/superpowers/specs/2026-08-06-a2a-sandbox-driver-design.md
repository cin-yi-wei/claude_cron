# A2A 沙盒驅動與收尾修正 設計

日期：2026-08-06
狀態：規格已定案，尚未實作
前置：`2026-08-05-a2a-integration-design.md`（已實作，commits 6daacee..7906b50，最終整體 review 判定 DO NOT SHIP）

## 目的

前一份規格的實作漏了一塊：**沒有任何東西把 prompt 真正送進沙盒**。本規格補上沙盒驅動，並修掉最終整體 review 列出的一個 Critical 與五個 Important。

整套 A2A 目前包在 `cfg.A2A.Enabled` 底下、預設關閉，`serve` 在關閉時行為與改動前完全一致（已由 review 逐條確認）。因此以下修正對線上零風險。

## C1：沙盒驅動

### 誤判更正

最終 review 把 C1 描述成「缺一個子系統」。實際讀 code 後確認**不是**：

`RunWorkerOnce(ctx context.Context, root string, injector Injector, timeout time.Duration) (bool, error)`（`worker.go:70`）**完全不綁 binding** — 函式體內沒有任何一處引用 `LoadRegistry` / `Binding` / `bindings.json`。它只需要一個含 `inbox/outbox` 的目錄與一個 injector。`TmuxInjector{Session, Root, AutoStart}`（`adapters.go:39`）本身就是「把訊息打進指定的 tmux session」。

review 說「三個呼叫端都傳 binding root」正確，但那是**呼叫端的慣例，不是函式的限制**。

### 做法

每個進入 `working` 的沙盒，由一條專屬 goroutine 反覆呼叫：

```go
RunWorkerOnce(ctx, SandboxRoot(root, task.Session),
    TmuxInjector{Session: task.Session, Root: SandboxRoot(root, task.Session)},
    timeout)
```

沙盒各自有獨立 root，因此各自持有自己的 `claude.lock`，與 40 個 cc- binding 無競爭。

### 為何用「每個沙盒一條 goroutine」

`RunWorkerOnce` 同步阻塞。三個選項：

| 做法 | 問題 |
|---|---|
| 排進既有 scheduler goroutine | 8 個沙盒 × timeout 會卡住那條 30 秒 ticker，而它同時服務 40 個正式 binding |
| **每個沙盒一條 goroutine（採用）** | 需管理生命週期；並發寫 tasks.json — 但 C2 的 mutex 本來就要加 |
| 專用 A2A ticker，序列跑 | 與 cc- 隔離，但沙盒之間仍互相排隊 |

隔離本來就是這套設計的前提（contextId 隔離），沙盒之間不該互相阻塞。

### 生命週期

- goroutine 在任務轉入 `working` 時啟動，key 為 session 名，**同一 session 不得重複啟動**
- 任務進入任一終止狀態（`completed` / `failed` / `canceled`）時停止
- `serve` 關閉時全部停止
- 需有一份 in-memory 的 `session -> cancel func` 對照表，並發安全

## C2：tasks.json 的 lost-update

`LoadTasks` / `SaveTasks` 目前無鎖，使用者有四處：server handler、executor 的 `persist`/`markFailed`、`CollectResults`、`SweepTimeouts`。任務 13 之後，HTTP handler 與 scheduler goroutine 會真正並發；加上 C1 的 per-sandbox goroutine 後更多。

可達的最壞情況：

- handler 的 load..save 跨越 `Executor.Start`（`git worktree add` + 最長 90 秒 `waitSessionReady`）。它的舊快照會覆蓋這段期間 executor 與 scheduler 寫入的一切。若 `CollectResults` 在該窗口內完成了某任務，該任務會被回寫成 `working`，而其結果檔已被搬到 `outbox/sent` → **永遠無法再完成**
- 任務列在窗口內消失 → 沙盒已啟動卻無紀錄 → sweep 看不到、`RunningCount` 不計數 → 8 併發上限被突破

`AtomicWriteJSON` 只保證不會寫出半截檔案，擋不住 lost update。

**做法**：一支 package 層級的 mutex，涵蓋所有 `Load..Save` 區段。只有 `serve` 會寫 `tasks.json`（已確認無 CLI / admin 路徑），因此行程內互斥即足夠。

**必須**：handler 不得在持鎖期間呼叫 `Executor.Start`。持鎖只涵蓋讀改寫，dispatch 在鎖外。

## Important 修正

### I1：同一 contextId 的追問被靜默吞掉

`a2a_executor.go` 對每個 contextId 產生固定的 `MessageID`，而 `IngestMessages` 以 `platform:channel:messageID` 去重（`watcher.go:57-60`）。第二則訊息回傳 `created=0` 且無錯誤，任務轉 `working` 後永遠等不到東西。

這正是規格第 110 行「完成後保留 10 分鐘讓呼叫方追問」的功能。

**做法**：`MessageID` 必須每則訊息唯一（contextId + 遞增序號或 taskId）。並補一個測試：同一 contextId 連送兩則，第二則必須真的進入 inbox。

### I2：終止狀態的 contextId 可被他人接管

`a2a_server.go` 的擁有權檢查只擋非終止狀態。`SessionNameFor` 與 `SandboxWorktree` 與呼叫方無關，且 `EnsureWorktree` 在路徑已存在時直接 no-op（`worktree.go:239`）→ 呼叫方 B 會繼承 A 的 checkout、未提交檔案、分支與 SandboxRoot。

**做法**：只要該 contextId 已有紀錄且 `CallerID` 不同，一律拒絕，**不分狀態**。

### I3：dispatch 綁在 request context

`a2a_server.go` 以 `r.Context()` 呼叫 `Executor.Start`，客戶端斷線會在 `git worktree add` / tmux 啟動中途取消，留下半成品 worktree 並因 forensics 規則保留不清。此外對外監聽器未設任何逾時。

**做法**：dispatch 改用與請求生命週期脫鉤的 context。監聽器補 `ReadHeaderTimeout` / `ReadTimeout` / `WriteTimeout` / `IdleTimeout`。

### I4：DrainQueue 阻塞 cron ticker

`main.go` 把 collect → sweep → drain 放進既有的 30 秒 scheduler goroutine，`DrainQueue` 最多同步啟動 8 個沙盒、每個最長 90 秒。

**做法**：A2A 的 per-cycle 工作移到專屬 goroutine，與 cron scheduler 分離。仍須維持 `DrainQueue` **不得與自己並發**。

### I5：worktree 永不回收

規格第 108 行要求回收 session **與** worktree；`a2a_lifecycle.go` 只清 `Session`。contextId 由呼叫方指定（128 字元英數），一個已核准的呼叫方即可無上限地製造 ~80MB 的 worktree 與 `sandboxes/*` 目錄。

**做法**：沙盒回收時一併移除 worktree 與 sandbox root。`failed` 仍依 forensics 規則保留 — 但需補一條**上限**（保留數量或保留天數），否則 forensics 規則本身就是一條無上限成長路徑。

## 沙盒的內建對話框（C1 的上游問題）

Claude Code 自己的確認框（信任資料夾、`Do you want to proceed?`）**不受 PreToolUse hook 管**。現有唯一的處理是 `supervisor.go:660` 的 confirm watchdog，它把對話框發到**該 binding 的頻道**再把回覆打進 pane。沙盒不是 binding，這條路不存在。

而 `EnsureWorktree` 會對每個新 worktree 執行 `EnsureAgentSettings`，新資料夾第一次開 session 必定跳信任提示（2026-08-06 `cc-calc-dev` 實測如此）。因此**每個新沙盒都會在 prompt 送進去之前就卡住**，無人可答，直到 2 小時後被 sweep 砍掉。這比 C1 更前面。

**做法（採 1+3）**：

1. **預先消除**：沙盒建立時就把信任狀態預先寫好，讓提示根本不跳。需先確認 Claude Code 把信任記錄在哪、是否可預設。
2. **自動回答**：仍然跳出來的內建對話框，由驅動 goroutine 偵測並自動回答（信任 = 1、proceed = 1）。這與「授權清單就是全部政策」一致——對外的門已在 A2A 層守過。

自動回答**只適用於 `aa-` 沙盒**。`cc-` binding 的 confirm watchdog 行為完全不動。

## agent 頻道（唯讀輸出）

每個 `aa-<agent>` **身分**擁有一個 Discord 頻道；`aa-<agent>-<ctx>` 任務實例**不另開頻道**。同一個 agent 的多個併發任務共用該頻道。用途是能見度與監控——看得出它到底有沒有在動。

- **單向輸出**。該頻道**絕不 ingest**。這是安全要求，不只是簡化：若吃進使用者輸入，任何能在 Discord 打字的人就能直接對沙盒下指令，繞過整個 A2A 認證與能力授權。實作上必須確保它不被註冊進任何 poll/push ingest 路徑。
- **每則輸出必須標註 contextId**（短前綴即可）。同一頻道會有多個併發任務交錯，沒有標註就是一團無法解讀的雜訊。
- **沿用既有 activity mirror**（`activity.go`）。它已經在做「把 session 正在做什麼串到頻道」，且 `discord.go:255-268` 已有 per-channel throttle 與 429 `retry_after` 退避——那是 2026-06 activity ticker 爆量 429 之後補的。不要另寫一條發送路徑繞過它。
- **併發量需留意**：最多 8 個沙盒同時串流到少數幾個 agent 頻道，是既有 throttle 未曾承受過的形狀。需驗證 throttle 是 per-channel 而非 per-binding。

`cc-` 任務委派給某個 agent 時，該工作的輸出出現在該 agent 的頻道，而非委派來源的頻道。

## 明確不做

- 不改動 `cc-` 機制（`bindings.json` / `registry.go` / `supervisor.go` / `reap.go`）
- 不加 pre-auth 稽核（失敗登入嘗試）— 那會把此 log 從「委派紀錄」擴成「安全 log」，屬獨立範圍決策
- 不補 `message/get` 或 cancel 方法 — 結果與分支名尚無法回傳給呼叫方，屬後續獨立工作
- 不做容器層隔離（DB 共用仍為已知限制）
