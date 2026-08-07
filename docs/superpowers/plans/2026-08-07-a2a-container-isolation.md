# A2A 沙盒容器隔離 實作計畫

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 A2A 沙盒從「與 `serve` 同一個 unix uid 的 tmux session」搬進「掛載表列舉、核心強制的 Docker 容器」，關掉 `develop` 改寫自己政策檔的自我提權路徑，同時讓 47 個 cc- binding 的行為逐位元不變。

**Architecture:** `SessionManager`（`a2a_session.go:14`）是既有介面，`TmuxSessionManager` 與新的 `ContainerSessionManager` 是兩個實作，靠一個 `RoutingSessionManager` 依每一列任務的 `Isolation` 欄位選擇。跨界的路徑一律「路徑同構」bind mount（host 路徑 == 容器內路徑），於是 `RunWorkerOnce`、`BuildClaudePrompt`、`CollectResults`、gate 的範圍判定全部一行不改；唯一真正換掉的是「tmux 指令在哪裡執行」（`paneDriver`）與「政策檔是唯讀掛載」。

**Tech Stack:** Go 1.26、module `claude_cron`、package `channelagent`，標準函式庫。外部工具只有 `docker`（29.1.3，rootful，`conray` 已在 `docker` 群組）。基底映像 `ubuntu:26.04`（arm64，glibc 2.43）。

**Spec:** `docs/superpowers/specs/2026-08-07-a2a-container-isolation-design.md`（commit `a20f479`）

**前置（全部已實作並併入 `dev`）:**
- `docs/superpowers/specs/2026-08-05-a2a-integration-design.md`
- `docs/superpowers/specs/2026-08-06-a2a-sandbox-driver-design.md`
- `docs/superpowers/specs/2026-08-06-a2a-confinement-design.md` —— 49 個 commit（`1cdc174`..`06b48c6`），permission gate、三個授權等級、per-sandbox 政策檔、單一守衛拆除路徑、admin API、CLI、admin UI **全部已存在且可用**。本計畫**建立在它之上，不重做它的任何一部分**。

---

## 零、規格寫成之後才確定的兩件事

**這兩條會直接改變規格的內容，實作者必須以本節為準。**

### 0.1 `--internal` 網路已在本機實測，規格第七節那個開放問題**已關閉**

規格第七節（以及「已接受的限制」第 8 條、開放問題）把「`--internal` 網路上的容器是否仍碰得到 host 自己 listen 在 `0.0.0.0` 的埠」列為未實測，並預留一條需要 root 的 `iptables -I INPUT` 備援。**實測結果：`--internal` 擋得住。** 具體三條：

1. 擋掉 host 自己發佈的 `0.0.0.0:3306`（`fatgame-mysql`）——連不上。
2. 擋掉 `docker0` 這個預設 bridge 的 gateway ——連不上。
3. 沒有任何對外 NAT ——沒有 internet egress。

**因此：本計畫不需要任何 root 動作，也不寫任何 iptables 規則。** 「零新特權」這個容器路線最主要的賣點成立，不帶星號。

同一組實測也確認了一件必須寫進設計的推論：既然 `--internal` 連 internet 都出不去，**出口代理容器（`cc-a2a-egress`，雙網卡）不是選配而是必要條件**——沒有它，容器內的 `claude` 連 `api.anthropic.com` 都到不了，沙盒一啟動就是廢的。它的建立因此排在 Task 2（風險前置那一輪），不是排在後面。

### 0.2 confinement 已經完成並併入 `dev`

規格寫的時候把 confinement 當成「已實作」的前置，本計畫再確認一次：permission gate（fail-closed）、`readonly`/`develop`/`full` 三個等級、`<root>/a2a-policies/<session>.json`、`tryLockSandboxSessionForTeardown` 的單一守衛拆除路徑、`/api/a2a/*` admin API、`claude-cron a2a …` CLI、admin UI 的 Agents 頁——**全部存在**。

本計畫的每一個 task 都是在這套東西上加東西或換實作，**不重寫、不平行實作、不複製**。哪些部分在容器下變成多餘，見第二節的對照表——**結論是這一輪一個字都不刪**，理由也寫在那裡。

---

## Global Constraints

以下每一條對每一個 task 都成立，不再逐 task 重述。

- **`WithTasks` 不可重入，callback 內不得有任何 I/O。** 不得在 callback 內呼叫 `docker`、`git`、`tmux`、`LoadTasks`、`SaveTasks`，也不得取得 session 鎖。要動 docker 就先在鎖內收集候選、放掉鎖之後才動手（`SweepTimeouts` 已經是這個形狀，照抄）。
- **鎖序固定：`build → callers/agents mu → tasksMu`，全程不得反向。**
- **所有破壞性拆除只走同一條守衛路徑**：`tryLockSandboxSessionForTeardown` 拿不到就跳過本輪（`TryLock`-and-skip），拿到之後先 `candidateStillMatches` 重新確認身分才動手。新增的 `ReapOrphanContainers` 也必須遵守——它是本計畫唯一新增的破壞性路徑，不得繞過這把鎖。
- **終止狀態的轉換一律 `appendDetail` 疊加 `Detail`，不覆寫**，並正確設定 `DetailSafe`（固定字面文字 `true`；包住 `err.Error()` 的一律 `false`）。
- **admin API 是 `agents.json` / `callers.json` 的唯一寫入者。** CLI 與 UI 只透過 HTTP 呼叫它，任何 task 都不得新增第二個寫入點。
- **一切都在 `cfg.A2A.Enabled` 之後，預設 `false`。** 關掉時 `serve` 行為逐位元不變。容器隔離再多一層 `cfg.A2A.Isolation`，預設 `"tmux"`（= 今天的行為）。
- **不得改變任何 cc- 行為。** `bindings.json`、`registry.go`、`supervisor.go` 的 binding 迴圈、`reap.go`、`permission.go` 的 cc- 分支、`agentSettings`/`EnsureAgentSettings`/`EnsureControlSettings`、`StartTmuxClaude`、`StartControlSession`、`classifyScreen`、`RunActivityStreamOnce`、`RunWorkerOnce`、`CollectResults`、`BuildClaudePrompt`、`IngestMessages`、`Init` —— 一行都不動。唯一被碰到的共用程式碼是 `paneDriver`（Task 3），它的驗收條件就是「nil 時 argv 逐字不變」。
- **測試永遠不得啟動 tmux、`claude` 行程、真實 `git`、容器，或發出真實網路請求。** docker 一律走可替換的 `dockerRunner` fake；tmux 一律走既有的 `runExternalCommand` / `runExternalCommandOutput` 變數替換；session 一律 `FakeSessionManager`。
- **`docker.sock` 永遠不掛進沙盒。** 這台是 rootful daemon，掛了等於直接給 host root。任何 task 加掛載都必須通過 Task 5 的不變量測試。
- **不啟用 dockerd userns-remap、不改 rootless、不動已在跑的 `siyuan` / `fatgame-mysql`。**
- 每個 task 最後一步是 commit；commit 訊息用專案既有慣例（`feat(a2a):` / `fix(a2a):` / `test(a2a):` / `docs:`）。

---

## 一、需要 operator 親自動手的步驟（不得埋在 task 裡）

下列步驟**不是 agent 能自己完成的**，每一項在對應 task 裡都以 **【OP】** 標記成獨立的一步，執行到那裡必須停下來等人。

| # | 在哪個 task | 內容 | 為什麼非人不可 |
|---|---|---|---|
| OP-1 | Task 1 | 跑 `make a2a-image` 建映像 | 需要對外網路拉 `ubuntu:26.04` 與 apt 套件；agent 的沙盒環境不保證有 |
| OP-2 | Task 2 | 到 Anthropic console **查證 `setup-token` 的計費歸屬是訂閱還是按量**（規格開放問題 3） | 程式碼註解（`worktree.go:416-425`）斷言是訂閱，使用者記憶檔記為未確認。兩者衝突，只有人能查 |
| OP-3 | Task 2 | 執行 `claude setup-token` 產生 **A2A 專屬** token，寫進 `/home/conray/project/claude_cron/.env` 的 `A2A_CLAUDE_CODE_OAUTH_TOKEN` | 互動式 OAuth 流程；且 token 是機密，不得經過 agent 的 transcript |
| OP-4 | Task 2 | 執行機制實證（容器內 `claude` 是否不跳登入、共用 `.git` rw 是否夠用、`:ro` worktree 下 Claude Code 是否還能跑） | 需要真的起容器、真的打上游 API |
| OP-5 | Task 2 | **決策點**：若 `:ro` worktree 下 Claude Code 無法運作（規格開放問題 8），決定 `readonly` 是退回 rw 掛載＋靠 gate，還是加 tmpfs 例外 | 這是取捨，不是事實 |
| OP-6 | Task 2 | **決策點**：egress allowlist 要不要加 `gitlab.jvdtech.dev` / `git.fatcatbet.net` / `registry.npmjs.org` / `proxy.golang.org`（規格開放問題 4） | 每加一條就是一條出口，是 operator 的風險取捨 |
| OP-7 | Task 7 | 決定全域 `a2a.isolation` 與哪一個 agent 先切成 `container` | 上線節奏 |
| OP-8 | Task 13 / 14 | 執行兩次端到端實跑並逐條核對「碰不到」清單 | 需要真的派一個委派任務、真的看 Discord 頻道 |
| OP-9 | Task 15 | kill switch 演練（切回 `tmux`、確認 cc- 無感） | 動到線上 `serve` |

**agent 執行到 【OP】 步驟時的正確行為：把該做的指令、預期輸出、以及判讀標準寫清楚，然後停下來回報，不要自己猜結果、不要跳過往下做。**

---

## 二、confinement 的哪些部分在容器下變成多餘 —— 保留還是刪除

規格第十一節列了一張「可刪」表。**本計畫的判斷是：這一輪一個字都不刪。** 逐條說明：

| confinement 的東西 | 容器下是否多餘 | 本計畫怎麼處理 | 理由 |
|---|---|---|---|
| `a2a_gate.go` 的旗標允許清單全家（`flagPolicy`、`flagTokenAllowed`、`firstDeniedFlag`、`readonlyHeadFlags`、`readonlyFindTokens`、`findDecision`、`readonlyGitSubFlags`，約 250 行） | **在容器模式下多餘**（`readonly` 的 worktree 是 `:ro`，路徑侷限由核心負責） | **保留，一行不動**，只在 `readonlyHeadFlags` 上方加一段註解說明它在 container 模式下是縱深防禦 | 兩種模式**並存**是本計畫的硬性要求。tmux 模式仍然靠它做路徑侷限，刪掉會讓 tmux 模式比今天更弱——那正是規格第十三節第 3 段說的「這一步之前不要刪任何 gate 程式碼」 |
| `bashDecision` 的首 token 允許清單、`gitDecision` 的子命令、`pushArgsAllowed` | **完全不多餘** | **保留** | 共用 repo 的 `P/.git` 是 **rw** 掛載（Task 5）。gate 的 git 規則仍是唯一擋住「動到同專案 47 個 binding 共用分支」的東西。**容器沒有取代它**，這是本計畫最大的殘留洞（見第三節） |
| confinement 開放問題 5：`full` 級沙盒的 Bash 可以改寫自己的政策檔 | **在容器模式下消失**（`:ro` 掛載，核心拒絕，`EROFS`） | 不需要改任何程式碼；Task 4 + Task 5 的不變量測試釘住它 | — |
| 已實測可行的 `develop → full` 自我提權（`python -c` 改一行 JSON） | **在容器模式下消失**（同上，且映像不裝 python） | 同上 | 這是整個容器路線存在的理由 |
| confinement F2：「`EnsureFolderTrusted` 必須走 `SessionManager.TrustFolder` 介面，否則單元測試會改到 operator 線上的 `~/.claude.json`」 | **在容器模式下失去理由**（每個沙盒有自己的 `CLAUDE_CONFIG_DIR`） | **保留這個介面約束**，`ContainerSessionManager.TrustFolder` 改寫到 `<sandboxRoot>/claude/.claude.json` | 介面本身沒有成本，而 tmux 模式仍然會寫 `~/.claude.json`。拆掉介面只會讓兩種模式的測試安全性不一致 |
| `a2a_gate.go:108-115` 的 `mcp__*` 拒絕分支 | **實際效力歸零**（容器內的 `CLAUDE_CONFIG_DIR` 沒有任何 MCP 設定，`full` 能放行的 `mcp__*` 是空集合） | **保留當縱深防禦** | 成本是零；而它在 tmux 模式下仍是真正的防線 |

**什麼時候才刪：** 規格第十三節第 3 段的「收尾」——確認容器路線穩定、`a2a.isolation = "tmux"` 正式標為 deprecated 之後，另開一個計畫刪。**本計畫的 Task 15 只負責把這個決策與當時的實際狀態記錄下來，不執行刪除。**

---

## 三、三條殘留風險（規格自己列的否決理由）與本計畫的最終狀態

規格第十五、十六節列了三條可能讓 operator 直接否決這條路線的風險。**本計畫沒有讓其中任何一條變好，只是把它們寫得更明確；如果 operator 的接受門檻碰到其中任何一條，應該在 Task 2 之後、Task 5 之前就停下來，而不是做完 15 個 task 才發現。**

**風險 1：共用 repo 的 `P/.git` 是 rw 掛載。** `git worktree add` 產生的 `W/.git` 是一個指標檔（`gitdir: P/.git/worktrees/<S>`），沒有 `P/.git` 的 rw 掛載，容器內連 `git status` 都跑不動、更不可能 `git commit`——而 `BuildClaudePrompt`（`adapters.go:408-411`）已經在教沙盒「回覆前必須 commit，否則 worktree 被回收後改動全失」。所以這個掛載是交付模型的必要條件。代價：沙盒可寫同專案所有 cc- binding 共用的 ref 與 object。**gate 的 `gitDecision` / `pushArgsAllowed` 仍是唯一防線，容器完全沒有取代它。** 根除只能改成 per-sandbox `git clone --local`，那會改變交付模型（分支不再直接落在共用 repo），規格估 +2～3 task，本計畫**不做**。Task 2 會實測確認這個掛載真的必要（而不是照抄規格的判斷）。

**風險 2：憑證的 scope 無法縮小。** `claudeAiOauth.scopes` 是登入流程發的五項（`user:file_upload, user:inference, user:mcp_servers, user:profile, user:sessions:claude_code`），operator 沒有任何介面可以縮小它。選項 B（A2A 專屬 `setup-token`）買到的**只有「可獨立撤銷」**，不是「權限更小」——拿到這份 token 的攻擊者仍能對 operator 的 team 訂閱下推論、讀 profile、列 sessions，直到被撤銷。唯一真正解掉的是選項 C（host 端注入認證的反向代理），規格估 +3～4 task 且含一段沒驗證過的實驗（`claude` 在「`ANTHROPIC_BASE_URL` 被換掉 + 沒有 OAuth 憑證」下是否正常運作、計費是否仍算訂閱，都未知）。本計畫採用 **B**，**不做 C**。若 operator 的門檻是「沙盒不得持有可重用憑證」，這 15 個 task 全部白做，應在 Task 2 之前就決定。

**風險 3：task 數與併發。** 規格的誠實區間是 **13–16**，本計畫是 **15**，落在區間內。差異來源逐項說明：
- **+1**：規格的 task 2 只寫「網路 + 代理，含 host-published port 的實測」。本計畫把它擴成 Task 2「風險前置實證」，額外涵蓋認證、rw `.git`、`:ro` worktree 三項——因為這三項才是真正會推翻整條路線的東西，把它們留到後面等於用 12 個 task 去賭一個沒驗證的假設。
- **+1**：規格的 task 4「`ContainerSessionManager`」被拆成 Task 5（掛載表組裝，純函式，可完整單元測試）與 Task 6（生命週期方法 + docker 三分法）。理由：掛載表是這份設計的安全邊界，它的不變量測試（政策檔 `:ro`、`R` 不可見、`docker.sock` 不存在、token 不進 argv）必須能在不碰 docker 的情況下跑，跟「docker 指令錯誤怎麼分類」是兩種完全不同的驗收。
- **+1**：規格的 task 12「端到端實跑」被拆成 Task 13（`readonly`）、Task 14（`develop` + 隔離清單逐條驗證）、Task 15（cc- 無影響 + kill switch 演練 + 收尾決策記錄）。規格自己就預期「task 12 會分裂成 2–3 個」，這裡照它的預期執行。
- **−1**：規格的 task 1（Dockerfile + make + 映像檢查）與 task 2 的 docker 錯誤處理合併進 Task 1，因為映像存在性檢查本身就需要那套三分法，分開會讓 Task 1 沒有可測的 Go 產出。

**併發從 8 降到 4**（Task 11）：可用記憶體 5.2 GiB，單一 `claude` 行程實測 RSS 430–765 MB，`--memory=2g` × 8 = 16 GiB 遠超可用量。本計畫**沿用規格的 4**，並在 Task 11 用實測數字覆核；若實測顯示 4 也吃緊，Task 11 的驗收條件允許再降，但不允許往上調。

---

## 四、排序原則

1. **Task 1–2 先把會推翻整條路線的東西驗掉**（映像能不能建、容器內能不能認證、rw `.git` 夠不夠、`:ro` 會不會弄壞 Claude Code）。這兩個 task 之後如果任何一條是紅的，後面 13 個 task 不該開工。
2. **Task 3–6 只加東西、不接線。** 做完之後 `ContainerSessionManager` 存在且有測試，但**沒有任何路徑會用到它**——分支狀態是「tmux 模式完好，容器模式尚未接上」，不會出現「半成品的隔離看起來像做完了」。
3. **Task 7 是唯一的接線點**，也是顯式開關落地的地方。這一 task 之後兩種模式並存，靠 `A2ATask.Isolation` 逐列路由；`a2a.isolation` 預設仍是 `"tmux"`，所以**接線本身不改變任何現況行為**。
4. **Task 8–12 補齊容器模式下才會壞的東西**（gate log 回不來、活動鏡像失效、孤兒容器沒人收、併發上限、管理介面）。每一個 task 都必須讓 tmux 路徑與容器路徑同時可用——驗收條件一律包含「tmux 模式的既有測試全綠」。
5. **Task 13–15 才真的把開關打開**，且順序是 `readonly` → `develop` → kill switch 演練。

---

## File Structure

| 檔案 | 責任 | 動作 |
|---|---|---|
| `docker/a2a-sandbox/Dockerfile` | 沙盒基底映像：glibc + tmux/git/ripgrep/jq/less/procps，uid 1000。**不烘 `claude`、不烘 `claude-cron`**（兩者掛載進去） | 新增 |
| `scripts/a2a-net-up.sh` | 冪等建立 `cc-a2a`（`--internal`）網路與 `cc-a2a-egress` 雙網卡代理容器 | 新增 |
| `docs/superpowers/notes/2026-08-07-a2a-container-probe.md` | Task 2 實證結果的紀錄（指令、實際輸出、判讀、決策） | 新增 |
| `internal/channelagent/a2a_docker.go` | `dockerRunner` 介面、`execDockerRunner`、`dockerError`、`dockerSaysAbsent`（三分法）、`SandboxImageAvailable` | 新增 |
| `internal/channelagent/a2a_pane.go` | `paneDriver` 介面、`hostTmux`、`dockerTmux`、`paneArgv` | 新增 |
| `internal/channelagent/a2a_container.go` | `ContainerSessionManager`：掛載表組裝（`runArgs`）、`gitCommonDirFor`、`Start`/`Stop`/`Alive`/`EnsureWorkspace`/`RemoveWorkspace`/`TrustFolder` | 新增 |
| `internal/channelagent/a2a_isolation.go` | `IsolationTmux`/`IsolationContainer`、`RoutingSessionManager`、`taskIsolation` | 新增 |
| `internal/channelagent/a2a_gatespool.go` | `GateSpoolPath` / `GateSpoolOffsetPath` / `DrainGateSpool` | 新增 |
| `internal/channelagent/a2a_reap.go` | `ReapOrphanContainers`、`OrphanGrace` | 新增 |
| `internal/channelagent/a2a_policy.go` | `PolicyPath` 改成 per-session 目錄（`:ro` 掛目錄才不會被 rename 換 inode 打敗） | 修改 |
| `internal/channelagent/adapters.go` | `TmuxInjector` 加可為 nil 的 `Pane paneDriver`；七個 tmux 呼叫點改走 `paneArgv` | 修改 |
| `internal/channelagent/supervisor.go` | `capturePane` / `capturePaneJoined` 加 `paneDriver` 參數（cc- 傳 nil） | 修改 |
| `internal/channelagent/confirm.go` | `sendConfirmChoice` 加 `paneDriver` 參數 | 修改 |
| `internal/channelagent/a2a_driver.go` | 傳 `dockerTmux{...}`；transcript 目錄覆寫；`autoAnswerSandboxConfirm` 帶 paneDriver | 修改 |
| `internal/channelagent/activity.go` | 新增 `CollectActivityIn`（`CollectActivity` 變成它的零值包裝，cc- 呼叫端零改動） | 修改 |
| `internal/channelagent/a2a_cycle.go` | 加 `DrainGateSpool`（排在 `CollectResults` 之前）與 `ReapOrphanContainers`（排在 `SweepTimeouts` 之後） | 修改 |
| `internal/channelagent/a2a_lifecycle.go` | 併發上限可設定；`removeCandidate` 一併清 gate spool | 修改 |
| `internal/channelagent/a2a_tasks.go` | `A2ATask.Isolation` | 修改 |
| `internal/channelagent/a2a_agents.go` | `Agent.Isolation` / `Agent.Image` | 修改 |
| `internal/channelagent/config.go` | `A2AConfig.Isolation` / `SandboxImage` / `SandboxNetwork` / `EgressProxy` / `OAuthTokenEnv` + 取值方法 | 修改 |
| `internal/channelagent/a2a_admin.go` | agent add/update 接受 `isolation` / `image`；task 列表帶容器名 | 修改 |
| `cmd/claude-cron/main.go` | 依設定組出 `RoutingSessionManager`；容器模式的前置檢查 | 修改 |
| `cmd/claude-cron/a2a_cmd.go` | `agent add/update --isolation --image` | 修改 |
| `web/admin/src/Agents.svelte` | 顯示隔離模式與容器狀態 | 修改 |
| `Makefile` | `make a2a-image` | 修改 |

---

### Task 1: 沙盒映像、`make a2a-image`，與 docker 指令的三分法錯誤分類

**Files:**
- Create: `docker/a2a-sandbox/Dockerfile`
- Create: `internal/channelagent/a2a_docker.go`
- Create: `internal/channelagent/a2a_docker_test.go`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `runExternalCommandOutput`（`adapters.go:330`，已是可替換的 `var`）
- Produces:
  - `type dockerRunner interface { Run(ctx context.Context, env []string, args ...string) (string, error) }`
  - `type execDockerRunner struct{}`（正式實作）
  - `type dockerError struct { Args []string; Stderr string; Err error }`，含 `Error() string` 與 `Unwrap() error`
  - `func dockerSaysAbsent(err error) bool`
  - `func SandboxImageAvailable(ctx context.Context, dr dockerRunner, image string) (bool, error)`
  - `const DefaultSandboxImage = "cc-a2a-sandbox:1"`

**為什麼三分法是這個 task 的主軸：** `TmuxSessionAlive`（`a2a_session.go:75`）已經寫了一大段註解在防同一件事——「問到了，答案是沒有」與「根本沒問到答案」對呼叫方的意義完全相反。`SweepTimeouts` 只有在 `sm.Stop` 回 nil 時才敢刪 worktree（`a2a_lifecycle.go:1140-1145`）；若 `docker rm -f` 在 daemon 掛掉時被誤讀成「已經沒有這個容器」，sweep 會在容器還活著的時候刪掉它的 bind mount 目標，行為未定義。整個容器路線的安全性從這個函式開始。

- [ ] **Step 1: 寫失敗的測試**

```go
package channelagent

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// fakeDocker 記錄每一次呼叫，並依序回放腳本化的結果。所有 docker 相關測試
// 共用它——測試永遠不得真的執行 docker。
type fakeDocker struct {
	calls [][]string
	envs  [][]string
	out   []string
	errs  []error
	n     int
}

func (f *fakeDocker) Run(_ context.Context, env []string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	i := f.n
	f.n++
	var o string
	var e error
	if i < len(f.out) {
		o = f.out[i]
	}
	if i < len(f.errs) {
		e = f.errs[i]
	}
	return o, e
}

// exitErr 造出一個「docker 真的跑起來、回報了非零離開碼」的錯誤，附上 stderr。
func exitErr(stderr string) error {
	return &dockerError{Args: []string{"image", "inspect"}, Stderr: stderr, Err: &exec.ExitError{}}
}

func TestDockerSaysAbsentOnlyForRealNegativeAnswers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no such image", exitErr("Error response from daemon: No such image: cc-a2a-sandbox:1"), true},
		{"no such object", exitErr("Error: No such object: cc-a2a-aa-x"), true},
		{"no such container", exitErr("Error response from daemon: No such container: cc-a2a-aa-x"), true},
		{"daemon down", exitErr("Cannot connect to the Docker daemon at unix:///var/run/docker.sock."), false},
		{"permission denied", exitErr("permission denied while trying to connect to the Docker daemon socket"), false},
		{"binary missing", &dockerError{Stderr: "", Err: exec.ErrNotFound}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := dockerSaysAbsent(c.err); got != c.want {
			t.Errorf("%s: dockerSaysAbsent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSandboxImageAvailableThreeWay(t *testing.T) {
	// 1. 存在
	dr := &fakeDocker{out: []string{"sha256:abc\n"}, errs: []error{nil}}
	ok, err := SandboxImageAvailable(context.Background(), dr, "cc-a2a-sandbox:1")
	if err != nil || !ok {
		t.Fatalf("present image: ok=%v err=%v, want true/nil", ok, err)
	}
	want := []string{"image", "inspect", "--format", "{{.Id}}", "cc-a2a-sandbox:1"}
	if len(dr.calls) != 1 || !equalStrings(dr.calls[0], want) {
		t.Fatalf("argv = %v, want %v", dr.calls, want)
	}

	// 2. 明確不存在 → (false, nil)
	dr = &fakeDocker{errs: []error{exitErr("Error: No such image: cc-a2a-sandbox:1")}}
	ok, err = SandboxImageAvailable(context.Background(), dr, "cc-a2a-sandbox:1")
	if err != nil || ok {
		t.Fatalf("absent image: ok=%v err=%v, want false/nil", ok, err)
	}

	// 3. 問不到答案 → 必須回非 nil error，絕不可以跟「不存在」混在一起
	dr = &fakeDocker{errs: []error{exitErr("Cannot connect to the Docker daemon")}}
	ok, err = SandboxImageAvailable(context.Background(), dr, "cc-a2a-sandbox:1")
	if err == nil {
		t.Fatalf("daemon down must return an error, got ok=%v err=nil", ok)
	}
}

func TestSandboxImageAvailableRefusesEmptyImage(t *testing.T) {
	dr := &fakeDocker{}
	if _, err := SandboxImageAvailable(context.Background(), dr, ""); err == nil {
		t.Fatal("empty image name must be an error, not a docker call")
	}
	if len(dr.calls) != 0 {
		t.Fatalf("empty image name must not shell out, got %v", dr.calls)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestDocker|TestSandboxImage' -race -v`
Expected: FAIL —— `undefined: dockerError`、`undefined: dockerSaysAbsent`、`undefined: SandboxImageAvailable`

- [ ] **Step 3: 寫最小實作**

```go
package channelagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultSandboxImage 是沙盒映像的預設 tag。tag 是「映像自己的版本號」
// （作業系統 + 工具集），不是 claude 的版本號 —— claude 與 claude-cron
// 都不烘進映像，由 bind mount 從 host 帶進去，版本永遠跟 host 一致。
const DefaultSandboxImage = "cc-a2a-sandbox:1"

// dockerRunner 是唯一執行 docker CLI 的地方，抽成介面只有一個理由：測試
// 永遠不得真的起容器。env 為 nil 時沿用 runExternalCommand 的環境處理
// （去掉 TMUX/TMUX_PANE）；非 nil 時整份取代 —— 這是把 OAuth token 放進
// 子行程「環境」而不是「argv」的機制（見 ContainerSessionManager.Start），
// argv 會出現在同一台機器上任何人的 ps 輸出裡，環境不會。
type dockerRunner interface {
	Run(ctx context.Context, env []string, args ...string) (string, error)
}

// dockerError 保留 docker 的 stderr。分類「明確不存在」與「問不到答案」
// 需要看 stderr，光看離開碼不夠：docker 對兩者都是非零離開。
type dockerError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *dockerError) Error() string {
	return fmt.Sprintf("docker %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

func (e *dockerError) Unwrap() error { return e.Err }

type execDockerRunner struct{}

func (execDockerRunner) Run(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if env == nil {
		cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	} else {
		cmd.Env = env
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), &dockerError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return string(out), nil
}

// dockerAbsentMarkers 是 docker 用來表達「我查過了，沒有這個東西」的字串。
// 刻意只列這三條、且要求同時滿足「docker 真的執行起來並回報離開碼」：任何
// 其他失敗（daemon 沒起來、權限不足、執行檔找不到、ctx 取消）都是「問不到
// 答案」，必須讓呼叫方看到 error 而不是一個看起來很確定的 false。
var dockerAbsentMarkers = []string{"no such image", "no such object", "no such container"}

func dockerSaysAbsent(err error) bool {
	var de *dockerError
	if !errors.As(err, &de) {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(de.Err, &exitErr) {
		return false // docker 沒有真的跑起來並回報離開碼
	}
	low := strings.ToLower(de.Stderr)
	for _, m := range dockerAbsentMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// SandboxImageAvailable 回報映像是否存在。三分法與 TmuxSessionAlive 完全
// 相同：(true, nil) 存在；(false, nil) 明確不存在；(_, err) 問不到答案。
func SandboxImageAvailable(ctx context.Context, dr dockerRunner, image string) (bool, error) {
	if strings.TrimSpace(image) == "" {
		return false, errors.New("sandbox image name is empty")
	}
	if _, err := dr.Run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if dockerSaysAbsent(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
```

- [ ] **Step 4: 跑測試確認它通過**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestDocker|TestSandboxImage' -race -v`
Expected: PASS（4 個測試全綠）

- [ ] **Step 5: 寫 Dockerfile**

```dockerfile
# 基底必須是 glibc：host 的 claude 2.1.223 與 claude-cron 都是動態連結
# glibc 2.43 的原生 ELF，alpine（musl）跑不動它們。
FROM ubuntu:26.04

# tmux：claude 需要 PTY。git：沙盒要 commit。ripgrep/jq/less/procps：
# Claude Code 的 Grep 工具與分頁需要。
# 刻意不裝 python3 / node / go —— 那正是 develop 用來改寫自己政策檔的東西。
# 代價是 develop 在這個共用映像裡跑不動 pytest / npm test / go test，
# 這是明確的功能倒退，解法是 per-agent 映像（Agent.Image，Task 7/12）。
RUN apt-get update && apt-get install -y --no-install-recommends \
      tmux git ca-certificates ripgrep jq less procps \
 && rm -rf /var/lib/apt/lists/*

# uid 1000 與 host 的 conray 對齊：bind 進來的 worktree 與 sandbox root
# 屬於 conray，容器內用同一個 uid 才寫得動。
RUN useradd -u 1000 -m -s /bin/bash agent

USER 1000:1000
```

- [ ] **Step 6: 加 `make a2a-image`**

在 `Makefile` 的 `.PHONY` 行加上 `a2a-image`，並在檔尾新增：

```makefile
A2A_IMAGE ?= cc-a2a-sandbox:1

a2a-image:
	docker build --platform linux/arm64 -t $(A2A_IMAGE) docker/a2a-sandbox
```

- [ ] **Step 7: 【OP-1】operator 建映像**

**這一步 agent 不做，停下來請 operator 執行。**

```bash
cd /home/conray/project/claude_cron && make a2a-image
docker image inspect --format '{{.Id}} {{.Architecture}} {{.Size}}' cc-a2a-sandbox:1
```

判讀標準：
- `docker build` 成功結束（需要對外網路拉 `ubuntu:26.04` 與 apt 套件）。
- `Architecture` 必須是 `arm64`。
- `Size` 預期 150–250 MB 之間。明顯更大代表誤把 `claude` 烘進去了，必須查。

失敗處理：`ubuntu:26.04` 若尚未發佈或拉不到，改用 `ubuntu:24.04` **只有在確認它的 glibc ≥ 2.43 時才可以**（`docker run --rm ubuntu:24.04 ldd --version`）；glibc 版本不夠就停下來回報，不要自己降版硬上——`claude` 會直接跑不起來，而症狀會出現在十個 task 之後。

- [ ] **Step 8: 全套測試 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... 2>&1 | tail -5`
Expected: 全部 ok（本 task 沒有碰到任何既有程式碼路徑）

```bash
cd /home/conray/project/claude_cron
git add docker/a2a-sandbox/Dockerfile Makefile internal/channelagent/a2a_docker.go internal/channelagent/a2a_docker_test.go
git commit -m "feat(a2a): sandbox image + docker CLI runner with three-way absence classification"
```

---

### Task 2: 【風險前置／需 operator】網路與出口代理，以及認證、共用 `.git`、`:ro` worktree 三項機制實證

**這個 task 存在的唯一理由是：後面 13 個 task 全部建立在三個尚未驗證的假設上。** 規格自己列的三條否決風險有兩條在這裡見真章。**任何一項是紅的，就停下來回報，不要往下做。**

**Files:**
- Create: `scripts/a2a-net-up.sh`
- Create: `docs/superpowers/notes/2026-08-07-a2a-container-probe.md`
- Modify: `.env`（**只有 operator 動**，且只加一個變數名，不進 git —— `.env` 已在 `.gitignore`）

**Interfaces:**
- Consumes: Task 1 的 `cc-a2a-sandbox:1` 映像
- Produces（給後面所有 task 當事實用）：
  - docker 網路 `cc-a2a`（`--internal`）與容器 `cc-a2a-egress`（雙網卡，`http://cc-a2a-egress:3128`）
  - `.env` 的 `A2A_CLAUDE_CODE_OAUTH_TOKEN`
  - 一份記錄了實際輸出與判讀的 probe 筆記，Task 5 的掛載表與 Task 13/14 的驗收都引用它

- [ ] **Step 1: 寫 `scripts/a2a-net-up.sh`**

```bash
#!/usr/bin/env bash
# 冪等地建立 A2A 沙盒的網路與出口代理。開機時由 boot-claude-cron.sh 呼叫，
# 也可以隨時手動重跑。不需要 root：conray 已在 docker 群組。
#
# 網路模型（2026-08-07 於本機實測）：
#   cc-a2a 是 --internal 網路，實測確認它同時擋掉
#     (1) host 自己發佈的 0.0.0.0:3306（fatgame-mysql）
#     (2) docker0 預設 bridge 的 gateway
#     (3) 任何對外 NAT（沒有 internet egress）
#   因此不需要任何 iptables 規則，也因此沙盒必須經由 cc-a2a-egress 才連得到
#   上游 API —— 代理不是選配，是必要條件。
set -euo pipefail

NET=cc-a2a
PROXY=cc-a2a-egress
PROXY_IMAGE=${PROXY_IMAGE:-alpine:3.20}
# 出口允許清單。預設只有上游 API。每加一行就是一條出口 —— 要不要加
# gitlab.jvdtech.dev / git.fatcatbet.net / registry.npmjs.org 是 operator
# 的取捨（規格開放問題 4），不是實作者可以自己決定的。
ALLOW=${A2A_EGRESS_ALLOW:-api.anthropic.com}

if ! docker network inspect "$NET" >/dev/null 2>&1; then
  docker network create --internal "$NET"
  echo "created internal network $NET"
else
  echo "network $NET already exists"
fi

if docker inspect -f '{{.State.Running}}' "$PROXY" 2>/dev/null | grep -qx true; then
  echo "proxy $PROXY already running"
  exit 0
fi

docker rm -f "$PROXY" >/dev/null 2>&1 || true

# tinyproxy：只放行 CONNECT 到允許清單裡的主機的 443。
CONF=$(mktemp)
{
  echo "Port 3128"
  echo "Listen 0.0.0.0"
  echo "Timeout 600"
  echo "Allow 0.0.0.0/0"
  echo "DisableViaHeader Yes"
  echo "ConnectPort 443"
  for h in $ALLOW; do echo "Filter /etc/tinyproxy/allow"; break; done
} > "$CONF"
ALLOWFILE=$(mktemp)
for h in $ALLOW; do echo "^${h//./\\.}$"; done > "$ALLOWFILE"
echo "FilterURLs Off"          >> "$CONF"
echo "FilterExtended On"       >> "$CONF"
echo "FilterDefaultDeny Yes"   >> "$CONF"

# 代理先接預設 bridge（它自己要出得去），再接 cc-a2a（沙盒找得到它）。
docker run -d --name "$PROXY" --restart unless-stopped \
  --network bridge \
  -v "$CONF":/etc/tinyproxy/tinyproxy.conf:ro \
  -v "$ALLOWFILE":/etc/tinyproxy/allow:ro \
  "$PROXY_IMAGE" \
  sh -c 'apk add --no-cache tinyproxy >/dev/null && exec tinyproxy -d -c /etc/tinyproxy/tinyproxy.conf'

docker network connect "$NET" "$PROXY"
echo "started proxy $PROXY on network $NET (allow: $ALLOW)"
```

`chmod +x scripts/a2a-net-up.sh`

- [ ] **Step 2: 起網路與代理，確認 `--internal` 的三條實測結論在本機重現**

```bash
cd /home/conray/project/claude_cron && ./scripts/a2a-net-up.sh
docker network inspect cc-a2a --format '{{.Internal}} {{range $k,$v := .Containers}}{{$v.Name}} {{end}}'
```
Expected: `true cc-a2a-egress`

```bash
# (1) 碰不到 host 發佈的 3306
docker run --rm --network cc-a2a "$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')" 2>/dev/null || true
GW=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')
docker run --rm --network cc-a2a cc-a2a-sandbox:1 \
  timeout 5 bash -c "</dev/tcp/$GW/3306" ; echo "exit=$?"
```
Expected: 非 0 的 exit（連不上）。**若這裡連得上，第零節 0.1 的前提就不成立，停下來回報。**

```bash
# (2) 沒有 internet egress
docker run --rm --network cc-a2a cc-a2a-sandbox:1 \
  timeout 5 bash -c "</dev/tcp/api.anthropic.com/443" ; echo "exit=$?"
```
Expected: 非 0（DNS 或連線失敗）——這正是需要代理的證據。

```bash
# (3) 經代理出得去
docker run --rm --network cc-a2a -e HTTPS_PROXY=http://cc-a2a-egress:3128 cc-a2a-sandbox:1 \
  bash -c 'exec 3<>/dev/tcp/cc-a2a-egress/3128 && echo -e "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r" >&3 && head -1 <&3'
```
Expected: `HTTP/1.0 200 Connection established`（或等價的 200）

```bash
# (4) 代理不放行清單外的主機
docker run --rm --network cc-a2a cc-a2a-sandbox:1 \
  bash -c 'exec 3<>/dev/tcp/cc-a2a-egress/3128 && echo -e "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r" >&3 && head -1 <&3'
```
Expected: 403 或 4xx，**不可以是 200**。若是 200，tinyproxy 的 filter 沒生效，必須先修好再往下——否則整個 egress allowlist 是裝飾品。

- [ ] **Step 3: 【OP-2】operator 到 console 查證 `setup-token` 的計費歸屬**

**停下來等人。** 程式碼註解（`worktree.go:416-425`）斷言 `CLAUDE_CODE_OAUTH_TOKEN` 是訂閱 OAuth token、計費留在方案內；使用者記憶檔 `auth-token-strategy` 把「訂閱 vs API 計費」記為**未確認**。兩者衝突，只有 operator 能查。

判讀標準：若查證結果是「按量計費」，選項 B 的成本模型整個改變（每一個委派任務都在燒 API 額度），**必須先回報並重新決定**，不要照抄程式碼註解往下做。

- [ ] **Step 4: 【OP-3】operator 產生 A2A 專屬 token**

**停下來等人。** 指令：

```bash
claude setup-token     # 互動式，產出一個約一年效期的 token
```

把結果寫進 `/home/conray/project/claude_cron/.env`：

```
A2A_CLAUDE_CODE_OAUTH_TOKEN=<剛剛產生的 token>
```

**必須用這個變數名，不可以用 `CLAUDE_CODE_OAUTH_TOKEN`。** 理由：`oauthTokenEnvArgs()`（`worktree.go:421`）讀的是後者，會把 token 注進**每一個 cc- session**。用不同的變數名，是「這份 token 只給 aa- 容器、可獨立撤銷」這個唯一買到的好處在機制上成立的前提。

`.env` 已在 `.gitignore`，token 不會進版控。agent 不得把 token 值寫進任何檔案、log 或回報訊息。

- [ ] **Step 5: 【OP-4a】實證一：容器內的 `claude` 不跳登入**

**停下來等人執行。**

```bash
set -a; . /home/conray/project/claude_cron/.env; set +a
S=aa-probe-1
R=/home/conray/project/claude_cron/.channel-agent
mkdir -p "$R/sandboxes/$S"/{claude,home}

docker run -d -t --name cc-a2a-$S \
  --network cc-a2a --user 1000:1000 --init \
  --cap-drop=ALL --security-opt no-new-privileges \
  --memory=2g --memory-swap=2g --pids-limit=512 --cpus=2 \
  -v "$R/sandboxes/$S":"$R/sandboxes/$S":rw \
  -v "$HOME/.local/share/claude/versions/$(readlink -f "$HOME/.local/bin/claude" | xargs basename)":/usr/local/bin/claude:ro \
  -e CC_REGISTRY_ROOT="$R/sandboxes/$S" \
  -e CLAUDE_CONFIG_DIR="$R/sandboxes/$S/claude" \
  -e HOME="$R/sandboxes/$S/home" \
  -e CLAUDE_CODE_OAUTH_TOKEN="$A2A_CLAUDE_CODE_OAUTH_TOKEN" \
  -e HTTPS_PROXY=http://cc-a2a-egress:3128 \
  -e HTTP_PROXY=http://cc-a2a-egress:3128 \
  cc-a2a-sandbox:1 \
  tmux new-session -s $S 'env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN claude'

sleep 45
docker exec cc-a2a-$S tmux capture-pane -pt $S | tail -30
```

判讀標準（**這是整個 task 最重要的一項**）：
- **綠**：畫面出現 Claude Code 的一般輸入框（`❯`），**沒有** `/login`、`Select login method`、`Paste code here` 之類的字樣。
- **紅**：卡在登入畫面 → 選項 B 不成立。此時**停下來回報**，並附上 pane 內容。可能原因：token 格式不對、`CLAUDE_CONFIG_DIR` 需要預先放某些檔案、或代理擋掉了登入以外的某個必要 endpoint。
- **黃**：出現 managed-settings / 資料夾信任之類的閘 → 不是紅燈（driver 本來就會答這些閘，`a2a_driver.go:245-275`），但要記進 probe 筆記，Task 13 會再確認一次。

這一步同時順帶驗證：`--cap-drop=ALL` + `no-new-privileges` 之下 `claude` 能正常執行（不需要任何 capability）。

- [ ] **Step 6: 【OP-4b】實證二：共用 repo 的 `.git` rw 掛載是否真的必要、以及夠不夠**

**停下來等人執行。** 先在 host 上造一個真的 worktree（用一個 scratch repo，不要用線上專案）：

```bash
P=/tmp/a2a-probe-repo
rm -rf "$P" /tmp/aa-probe-2 && mkdir -p "$P" && cd "$P"
git init -q && git config user.email probe@local && git config user.name probe
echo hello > a.txt && git add a.txt && git commit -qm init
git worktree add -q /tmp/aa-probe-2 -b aa/aa-probe-2
cat /tmp/aa-probe-2/.git          # 應該是 "gitdir: /tmp/a2a-probe-repo/.git/worktrees/aa-probe-2"
```

**先驗證「沒有 `P/.git` 掛載會怎樣」——這一項是要證明這個洞是必要的，不是照抄規格的判斷：**

```bash
docker run --rm --network cc-a2a --user 1000:1000 \
  -v /tmp/aa-probe-2:/tmp/aa-probe-2:rw \
  cc-a2a-sandbox:1 git -C /tmp/aa-probe-2 status
```
Expected: 失敗（`not a git repository` 或 `cannot chdir to ...`）。**這就是 rw `.git` 掛載無法迴避的證據。**

**再驗證「加上掛載之後 commit 走得通」：**

```bash
docker run --rm --network cc-a2a --user 1000:1000 \
  -v /tmp/aa-probe-2:/tmp/aa-probe-2:rw \
  -v /tmp/a2a-probe-repo/.git:/tmp/a2a-probe-repo/.git:rw \
  cc-a2a-sandbox:1 bash -c '
    cd /tmp/aa-probe-2 &&
    git config user.email probe@local && git config user.name probe &&
    echo changed > a.txt && git add -A && git commit -qm "from container" &&
    git log --oneline -1'
```
Expected: 印出 `<sha> from container`。

**然後從 host 確認那個 commit 真的落在共用 repo 裡**（這正是交付模型的依據，也正是風險 1 的具體形狀）：

```bash
git -C /tmp/a2a-probe-repo log --oneline -1 aa/aa-probe-2
```
Expected: 同一個 sha。

判讀標準：
- 兩者都成立 → 交付模型可行，**同時確認風險 1 是真的**：容器內的行程能寫共用 repo 的 ref。probe 筆記必須明寫「gate 的 `gitDecision` / `pushArgsAllowed` 是唯一防線」。
- commit 失敗（權限、ownership、`safe.directory`）→ 記下實際錯誤。若是 `detected dubious ownership`，解法是在映像裡加 `git config --system --add safe.directory '*'`，**但那要記進 probe 筆記與 Dockerfile，不能只在 probe 裡臨時加。**

- [ ] **Step 7: 【OP-4c / OP-5】實證三：`readonly` 的 `:ro` worktree 下 Claude Code 還能不能運作**

**停下來等人執行。** 這是規格開放問題 8，直接決定第二節那張表裡「旗標允許清單」的價值。

```bash
S=aa-probe-3
R=/home/conray/project/claude_cron/.channel-agent
mkdir -p "$R/sandboxes/$S"/{claude,home}
set -a; . /home/conray/project/claude_cron/.env; set +a

docker run -d -t --name cc-a2a-$S \
  --network cc-a2a --user 1000:1000 --init \
  --cap-drop=ALL --security-opt no-new-privileges \
  -v /tmp/aa-probe-2:/tmp/aa-probe-2:ro \
  -v /tmp/a2a-probe-repo/.git:/tmp/a2a-probe-repo/.git:rw \
  -v "$R/sandboxes/$S":"$R/sandboxes/$S":rw \
  -v "$HOME/.local/share/claude/versions/$(readlink -f "$HOME/.local/bin/claude" | xargs basename)":/usr/local/bin/claude:ro \
  -e CLAUDE_CONFIG_DIR="$R/sandboxes/$S/claude" -e HOME="$R/sandboxes/$S/home" \
  -e CLAUDE_CODE_OAUTH_TOKEN="$A2A_CLAUDE_CODE_OAUTH_TOKEN" \
  -e HTTPS_PROXY=http://cc-a2a-egress:3128 -e HTTP_PROXY=http://cc-a2a-egress:3128 \
  -w /tmp/aa-probe-2 \
  cc-a2a-sandbox:1 \
  tmux new-session -s $S 'env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN claude'

sleep 45
docker exec cc-a2a-$S tmux capture-pane -pt $S | tail -30
# 順帶確認核心真的擋住寫入：
docker exec cc-a2a-$S bash -c 'echo x > /tmp/aa-probe-2/a.txt' ; echo "write exit=$?"
```

判讀標準：
- **綠**：Claude Code 正常起到輸入框，且寫入 `a.txt` 失敗（`Read-only file system`）。→ `readonly` 的路徑侷限由核心負責，第二節那張表裡「旗標允許清單多餘」的判斷成立（但仍然保留，見第二節）。
- **紅**：Claude Code 因為無法在 cwd 寫暫存檔而起不來。→ **【OP-5】決策點**，兩個選項擇一，寫進 probe 筆記：
  - (a) `readonly` 退回 rw worktree，路徑侷限繼續靠 gate 的旗標允許清單。此時第二節那張表的第一列從「在容器模式下多餘」改成「仍然是唯一防線」，Task 5 的掛載表也要跟著改。
  - (b) 加 tmpfs 例外（`--tmpfs /tmp` 之類）並重測。規格第二節第 3 點刻意選擇不逐一挖例外，若要走這條必須明確記錄挖了哪些。

- [ ] **Step 8: 【OP-6】決策點：egress allowlist 的最終內容**

**停下來等人決定並記錄。** 預設只有 `api.anthropic.com`，其直接後果必須讓 operator 知道：
- **`git push` 到 `gitlab.jvdtech.dev` / `git.fatcatbet.net` 會被擋掉。** 目前 `develop` 等級是允許 `git push` 的（`a2a_gate.go:480-504`）。交付模型改成「commit 到 `aa/<S>` 分支，分支已存在於共用 repo」——這正是 `BuildClaudePrompt`（`adapters.go:408-411`）已經在教沙盒做的事，所以預設不放行是自洽的。
- **`WebFetch` 會失效**（`develop` 與 `full` 目前放行它）。`WebSearch` 走上游 API，應該還能用，但這一點也要在 Task 14 實測確認，不要當成事實。

決定寫進 probe 筆記與 `scripts/a2a-net-up.sh` 的 `A2A_EGRESS_ALLOW` 預設值。

- [ ] **Step 9: 收拾 probe 容器、寫筆記**

```bash
docker rm -f cc-a2a-aa-probe-1 cc-a2a-aa-probe-3 2>/dev/null || true
rm -rf /home/conray/project/claude_cron/.channel-agent/sandboxes/aa-probe-1 \
       /home/conray/project/claude_cron/.channel-agent/sandboxes/aa-probe-3
git -C /tmp/a2a-probe-repo worktree remove --force /tmp/aa-probe-2 2>/dev/null || true
rm -rf /tmp/a2a-probe-repo
```

`docs/superpowers/notes/2026-08-07-a2a-container-probe.md` 必須包含：每一步的**實際指令與實際輸出**（不是預期輸出）、四個判讀結論（`--internal`、認證、rw `.git`、`:ro` worktree）、以及 OP-2/OP-5/OP-6 三個決策點的決定與理由。後面每一個 task 都會引用這份筆記，**它不是選配的文件**。

- [ ] **Step 10: Commit**

```bash
cd /home/conray/project/claude_cron
git add scripts/a2a-net-up.sh docs/superpowers/notes/2026-08-07-a2a-container-probe.md
git commit -m "feat(a2a): internal egress network script + record the container mechanism probe"
```

**繼續往下之前的檢查點：** probe 筆記的四個結論全部是綠（或紅的那一項已經有明確的替代決定並改寫進本計畫）。任何一項還懸著就停下來，不要開 Task 3。

---

### Task 3: `paneDriver` 抽象，以及 cc- argv 逐字不變的回歸測試

**Files:**
- Create: `internal/channelagent/a2a_pane.go`
- Create: `internal/channelagent/a2a_pane_test.go`
- Modify: `internal/channelagent/adapters.go`（`TmuxInjector` 的七個 tmux 呼叫點）
- Modify: `internal/channelagent/supervisor.go`（**只新增** `capturePaneVia`，既有 `capturePane` 簽章不動）
- Modify: `internal/channelagent/confirm.go`（**只新增** `sendConfirmChoiceVia`，既有 `sendConfirmChoice` 簽章不動）

**Interfaces:**
- Consumes: `runExternalCommand` / `runExternalCommandOutput`（`adapters.go:318,330`）
- Produces:
  - `type paneDriver interface{ argv(tmuxArgs []string) (name string, args []string) }`
  - `type hostTmux struct{}`、`type dockerTmux struct{ container string }`
  - `func paneArgv(p paneDriver, tmuxArgs ...string) (string, []string)`
  - `func paneRun(ctx context.Context, p paneDriver, tmuxArgs ...string) error`
  - `func paneOutput(ctx context.Context, p paneDriver, tmuxArgs ...string) (string, error)`
  - `TmuxInjector` 新欄位 `Pane paneDriver`（可為 nil）
  - `func capturePaneVia(ctx context.Context, p paneDriver, session string) string`
  - `func sendConfirmChoiceVia(ctx context.Context, p paneDriver, session string, choice int) error`

**設計決定，明確記錄：** 規格說「`capturePane` / `sendConfirmChoice` 各加一個參數，cc- 傳 nil」。本計畫改成**新增 `…Via` 版本、既有函式改成傳 nil 的包裝**——結果完全相同，但 `supervisor.go` 與 `confirm.go` 的既有呼叫端**一個字都不用改**，於是「cc- 行為逐位元不變」這件事從「靠 review 逐一確認每個新加的 nil」變成「那些行根本沒被編輯過」。這是 diff 大小與可驗證性的直接改善，不是風格偏好。

**不做的事：** `TmuxSessionAlive`（`a2a_session.go:75`）與 `StopTmuxSession`（`:540`）**不改成走 `paneDriver`**。它們是「這個執行體還在不在／把它停掉」，容器的答案不是「在容器裡跑 tmux」，而是 `docker inspect` / `docker rm -f`——那屬於 `ContainerSessionManager`（Task 6），不是同一件事。硬套 `paneDriver` 會做出一個「在已經死掉的容器裡 exec tmux」的荒謬呼叫。

- [ ] **Step 1: 寫失敗的測試（含 cc- argv 逐字不變的回歸測試）**

```go
package channelagent

import (
	"context"
	"testing"
)

func TestPaneArgvNilIsPlainTmux(t *testing.T) {
	name, args := paneArgv(nil, "send-keys", "-t", "cc-x", "-l", "hello")
	if name != "tmux" {
		t.Fatalf("name = %q, want tmux", name)
	}
	want := []string{"send-keys", "-t", "cc-x", "-l", "hello"}
	if !equalStrings(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestPaneArgvDockerTmuxWrapsWithExec(t *testing.T) {
	name, args := paneArgv(dockerTmux{container: "cc-a2a-aa-x-1"}, "capture-pane", "-pt", "aa-x-1")
	if name != "docker" {
		t.Fatalf("name = %q, want docker", name)
	}
	want := []string{"exec", "cc-a2a-aa-x-1", "tmux", "capture-pane", "-pt", "aa-x-1"}
	if !equalStrings(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

// paneArgv 不得改動呼叫方傳進來的 slice。dockerTmux 用 append 到一個字面
// slice 上，若寫成 append(tmuxArgs, ...) 就會在容量夠時就地覆寫呼叫方的
// 陣列 —— 對一個每拍都被呼叫、且共用底層陣列的路徑而言是典型的隱性 bug。
func TestPaneArgvDoesNotAliasCallerSlice(t *testing.T) {
	orig := []string{"send-keys", "-t", "aa-x", "C-c"}
	cp := append([]string(nil), orig...)
	_, _ = paneArgv(dockerTmux{container: "c"}, orig...)
	if !equalStrings(orig, cp) {
		t.Fatalf("caller slice mutated: %v, want %v", orig, cp)
	}
}

// 這是本 task 的驗收條件：TmuxInjector 在 Pane == nil 時送出的 argv 序列，
// 必須與改動前逐字相同。47 個 cc- binding 全部走這條路。
func TestTmuxInjectorNilPaneEmitsUnchangedArgv(t *testing.T) {
	origRun, origOut := runExternalCommand, runExternalCommandOutput
	defer func() { runExternalCommand, runExternalCommandOutput = origRun, origOut }()

	var got [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		got = append(got, append([]string{name}, args...))
		return nil
	}
	// capture-pane 回傳一個「不忙碌、輸入框為空」的畫面，讓 typeAndSubmit
	// 走完整條路徑而不中途 defer。
	runExternalCommandOutput = func(_ context.Context, name string, args ...string) (string, error) {
		got = append(got, append([]string{name}, args...))
		return "❯ \n", nil
	}

	origDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = origDelay }()

	i := TmuxInjector{Session: "cc-demo"}
	if err := i.typeAndSubmit(context.Background(), "hello world"); err != nil {
		t.Fatalf("typeAndSubmit: %v", err)
	}

	want := [][]string{
		{"tmux", "send-keys", "-t", "cc-demo", "C-c"},
		{"tmux", "send-keys", "-t", "cc-demo", "-l", "hello world"},
		{"tmux", "capture-pane", "-pt", "cc-demo"}, // typeAndSubmit 的 paneBusy 重檢
		{"tmux", "send-keys", "-t", "cc-demo", "Enter"},
	}
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot: %v", len(got), len(want), got)
	}
	for i := range want {
		if !equalStrings(got[i], want[i]) {
			t.Fatalf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestTmuxInjectorDockerPaneGoesThroughDockerExec(t *testing.T) {
	origRun, origOut := runExternalCommand, runExternalCommandOutput
	defer func() { runExternalCommand, runExternalCommandOutput = origRun, origOut }()

	var got [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		got = append(got, append([]string{name}, args...))
		return nil
	}
	runExternalCommandOutput = func(_ context.Context, name string, args ...string) (string, error) {
		got = append(got, append([]string{name}, args...))
		return "❯ \n", nil
	}
	origDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = origDelay }()

	i := TmuxInjector{Session: "aa-x-1", Pane: dockerTmux{container: "cc-a2a-aa-x-1"}}
	if err := i.typeAndSubmit(context.Background(), "hi"); err != nil {
		t.Fatalf("typeAndSubmit: %v", err)
	}
	for n, c := range got {
		if c[0] != "docker" || c[1] != "exec" || c[2] != "cc-a2a-aa-x-1" || c[3] != "tmux" {
			t.Fatalf("call %d = %v, want docker exec cc-a2a-aa-x-1 tmux …", n, c)
		}
	}
}

func TestCapturePaneViaNilMatchesCapturePane(t *testing.T) {
	origOut := runExternalCommandOutput
	defer func() { runExternalCommandOutput = origOut }()
	var got [][]string
	runExternalCommandOutput = func(_ context.Context, name string, args ...string) (string, error) {
		got = append(got, append([]string{name}, args...))
		return "pane", nil
	}
	_ = capturePane(context.Background(), "cc-demo")
	_ = capturePaneVia(context.Background(), nil, "cc-demo")
	if len(got) != 2 || !equalStrings(got[0], got[1]) {
		t.Fatalf("capturePaneVia(nil) must be identical to capturePane, got %v", got)
	}
}

func TestSendConfirmChoiceViaNilMatchesOriginal(t *testing.T) {
	origRun := runExternalCommand
	defer func() { runExternalCommand = origRun }()
	var got [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		got = append(got, append([]string{name}, args...))
		return nil
	}
	_ = sendConfirmChoice(context.Background(), "cc-demo", 2)
	_ = sendConfirmChoiceVia(context.Background(), nil, "cc-demo", 2)
	if len(got) != 4 {
		t.Fatalf("call count = %d, want 4", len(got))
	}
	if !equalStrings(got[0], got[2]) || !equalStrings(got[1], got[3]) {
		t.Fatalf("sendConfirmChoiceVia(nil) must be identical, got %v", got)
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestPane|TestTmuxInjector.*Pane|TestCapturePaneVia|TestSendConfirmChoiceVia' -race -v`
Expected: FAIL —— `undefined: paneArgv`、`undefined: dockerTmux`、`unknown field Pane`

- [ ] **Step 3: 寫 `a2a_pane.go`**

```go
package channelagent

import "context"

// paneDriver 決定 tmux 指令在哪裡執行。nil = 今天的行為（host 的 tmux
// server），cc- 路徑永遠是 nil —— 這不是預設值的巧合，是硬性要求：47 個
// 正式 binding 全部走 nil，nil 產生的 argv 必須與抽象化之前逐字相同
// （TestTmuxInjectorNilPaneEmitsUnchangedArgv 釘住這一點）。
type paneDriver interface {
	argv(tmuxArgs []string) (name string, args []string)
}

// hostTmux 是 nil 的具體形式：原樣執行 tmux。
type hostTmux struct{}

func (hostTmux) argv(tmuxArgs []string) (string, []string) { return "tmux", tmuxArgs }

// dockerTmux 把同一組 tmux 參數包成 `docker exec <container> tmux …`。
// 容器內每個沙盒有自己的 tmux server，session 名與 host 時代完全相同，
// 所以 -t <session> 這種參數一個字都不用改。
type dockerTmux struct{ container string }

func (d dockerTmux) argv(tmuxArgs []string) (string, []string) {
	// 一定要 append 到一個新的字面 slice 上，不能 append(tmuxArgs, …)：
	// 後者在容量足夠時會就地覆寫呼叫方的底層陣列。
	args := []string{"exec", d.container, "tmux"}
	return "docker", append(args, tmuxArgs...)
}

func paneArgv(p paneDriver, tmuxArgs ...string) (string, []string) {
	if p == nil {
		return hostTmux{}.argv(tmuxArgs)
	}
	return p.argv(tmuxArgs)
}

func paneRun(ctx context.Context, p paneDriver, tmuxArgs ...string) error {
	name, args := paneArgv(p, tmuxArgs...)
	return runExternalCommand(ctx, name, args...)
}

func paneOutput(ctx context.Context, p paneDriver, tmuxArgs ...string) (string, error) {
	name, args := paneArgv(p, tmuxArgs...)
	return runExternalCommandOutput(ctx, name, args...)
}
```

- [ ] **Step 4: 改 `TmuxInjector`**

`adapters.go` 的結構加一個欄位：

```go
type TmuxInjector struct {
	Session   string
	Root      string
	AutoStart bool
	// Pane 決定 tmux 指令在哪裡執行。nil（零值）= host tmux，與這個欄位
	// 存在之前完全相同；cc- 路徑永遠不設定它。只有 A2A 的容器沙盒會傳
	// dockerTmux{...}。
	Pane paneDriver
}
```

然後把 `TmuxInjector` 內**所有** `runExternalCommand(ctx, "tmux", …)` 換成 `paneRun(ctx, i.Pane, …)`、所有 `runExternalCommandOutput(ctx, "tmux", …)` 換成 `paneOutput(ctx, i.Pane, …)`。完整清單（改完用 `grep -n '"tmux"' internal/channelagent/adapters.go` 確認只剩 `ensureSession` 裡的 `new-session`，見下）：

| 位置 | 原本 | 改成 |
|---|---|---|
| `Inject` 的驗證 capture（`:93`） | `runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", i.Session)` | `paneOutput(ctx, i.Pane, "capture-pane", "-pt", i.Session)` |
| `submitLine` 的驗證 capture（`:139`） | 同上 | 同上 |
| `LooksGlitched`（`:153`） | 同上 | 同上 |
| `SessionWorking`（`:165`） | 同上 | 同上 |
| `paneBusy`（`:209`） | 同上 | 同上 |
| `typeAndSubmit`（`:185,188,200`） | `runExternalCommand(ctx, "tmux", "send-keys", …)` ×3 | `paneRun(ctx, i.Pane, "send-keys", …)` ×3 |
| `PasteLoginCode`（`:225,229`） | ×2 | 同上 |
| `SendLogin`（`:237,241`） | ×2 | 同上 |
| `SelectLoginSubscription`（`:248,252`） | ×2 | 同上 |
| `PressEnter`（`:258`） | ×1 | 同上 |
| `SelectTrustSettings`（`:278,286`） | ×2 | 同上 |

`ensureSession`（`:354`）**特別處理**：它的 `has-session` 走 `paneRun`，但 `new-session -d -s <s> claude` **不轉**——在容器模式下「session 不存在」代表容器本身有問題，重建 session 不是正確反應（正確反應是 `ContainerSessionManager.Start` 重建整個容器）。改成：

```go
func (i TmuxInjector) ensureSession(ctx context.Context) error {
	err := paneRun(ctx, i.Pane, "has-session", "-t", i.Session)
	if err == nil {
		return nil
	}
	// 容器沙盒不自己重建 session：session 不在代表容器不在或壞了，那是
	// ContainerSessionManager 與 sweep 的職責。AutoStart 只服務 cc-。
	if !i.AutoStart || i.Pane != nil {
		return err
	}
	if err := runExternalCommand(ctx, "tmux", "new-session", "-d", "-s", i.Session, "claude"); err != nil {
		return err
	}
	waitSessionReady(ctx, i.Session)
	return nil
}
```

- [ ] **Step 5: 加兩個 `…Via` 包裝**

`supervisor.go`，緊接在既有 `capturePane` 之後（既有函式改成一行委派，簽章與行為不變）：

```go
// capturePane returns the tmux pane snapshot for a session (empty on error).
func capturePane(ctx context.Context, session string) string {
	return capturePaneVia(ctx, nil, session)
}

// capturePaneVia 與 capturePane 相同，但可指定 tmux 指令在哪裡執行。
// p == nil 時與 capturePane 逐位元相同（cc- 的每一個呼叫端都走這條）。
func capturePaneVia(ctx context.Context, p paneDriver, session string) string {
	out, err := paneOutput(ctx, p, "capture-pane", "-pt", session)
	if err != nil {
		return ""
	}
	return out
}
```

`confirm.go`，同樣的形狀：

```go
// sendConfirmChoice types the chosen option number into the session and submits.
func sendConfirmChoice(ctx context.Context, session string, choice int) error {
	return sendConfirmChoiceVia(ctx, nil, session, choice)
}

// sendConfirmChoiceVia 與 sendConfirmChoice 相同，但可指定執行位置。
func sendConfirmChoiceVia(ctx context.Context, p paneDriver, session string, choice int) error {
	if err := paneRun(ctx, p, "send-keys", "-t", session, strconv.Itoa(choice)); err != nil {
		return err
	}
	return paneRun(ctx, p, "send-keys", "-t", session, "Enter")
}
```

`capturePaneJoined`（`supervisor.go:150`）與 `resolveConfirmReply`（`confirm.go:109`）**不動**：前者只服務 cc- 的登入 URL 擷取，後者只服務 cc- 的頻道回覆。

- [ ] **Step 6: 跑測試確認它通過**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestPane|TestTmuxInjector|TestCapturePane|TestSendConfirm' -race -v`
Expected: PASS

- [ ] **Step 7: 跑全套，確認 cc- 的既有測試一條都沒動到**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -10`
Expected: 全部 ok。特別確認 `TestPermissionGateBindingPathUnchanged`、`supervisor_test.go`、`adapters_test.go`、`confirm_test.go` 全綠且**沒有被修改過**（`git diff --stat` 裡不應出現這幾個 `_test.go`）。

- [ ] **Step 8: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_pane.go internal/channelagent/a2a_pane_test.go \
        internal/channelagent/adapters.go internal/channelagent/supervisor.go internal/channelagent/confirm.go
git commit -m "feat(a2a): paneDriver so tmux commands can run inside a container; nil path byte-identical"
```

---

### Task 4: 政策檔改成 per-session 目錄，讓 `:ro` 掛載下的撤銷仍然即時生效

**Files:**
- Modify: `internal/channelagent/a2a_policy.go`（`PolicyPath`、`WriteSandboxPolicy`、`RemoveSandboxPolicy`）
- Modify: `internal/channelagent/a2a_policy_test.go`

**Interfaces:**
- Consumes: `AtomicWriteJSONMode`、`ReadJSON`、`sandboxSessionRe`、`PolicyDir(root)`
- Produces:
  - `func PolicySessionDir(root, session string) (string, error)` → `<root>/a2a-policies/<session>`
  - `PolicyPath(root, session)` 改為 `<root>/a2a-policies/<session>/policy.json`（簽章不變）
  - `RemoveSandboxPolicy` 改為刪整個 per-session 目錄（簽章不變）

**為什麼非改不可（規格第四節）：** `AtomicWriteJSONMode` 走的是 write-temp-then-rename，**rename 會換掉 inode**。而**單一檔案的 bind mount 綁的是舊 inode** —— 撤銷（`RevokeSandboxPolicy`）在 host 寫出新 inode 之後，容器內會繼續看到舊內容，撤銷等於不生效。解法是掛目錄而不是掛單檔；但掛整個 `a2a-policies/` 會讓每個沙盒看到**所有**沙盒的政策（含別的 caller id）。折衷就是「一個只含自己的專屬目錄」。

**遷移：** 舊格式 `<root>/a2a-policies/<session>.json` 只可能出現在「升級當下正好有沙盒在跑」。**不寫相容讀取路徑**——`LoadSandboxPolicy` 找不到新路徑就回錯，gate 落到 `denied_no_policy`，那是 **fail-closed**（沙盒被全面拒絕），安全方向正確。`RemoveSandboxPolicy` 順手把舊檔也刪掉，避免留垃圾。這一段必須寫進程式碼註解。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestPolicyPathIsPerSessionDirectory(t *testing.T) {
	root := t.TempDir()
	p, err := PolicyPath(root, "aa-demo-ctx")
	if err != nil {
		t.Fatalf("PolicyPath: %v", err)
	}
	want := filepath.Join(root, "a2a-policies", "aa-demo-ctx", "policy.json")
	if p != want {
		t.Fatalf("PolicyPath = %q, want %q", p, want)
	}
	d, err := PolicySessionDir(root, "aa-demo-ctx")
	if err != nil || d != filepath.Dir(want) {
		t.Fatalf("PolicySessionDir = %q/%v, want %q", d, err, filepath.Dir(want))
	}
}

func TestPolicySessionDirRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"", "aa-../escape", "aa-a/b", "cc-worker", "..", "aa-ok/../.."} {
		if _, err := PolicySessionDir(root, bad); err == nil {
			t.Errorf("PolicySessionDir(%q) must be rejected", bad)
		}
		if _, err := PolicyPath(root, bad); err == nil {
			t.Errorf("PolicyPath(%q) must be rejected", bad)
		}
	}
}

// 這是本 task 真正的驗收條件：撤銷之後，「政策檔所在的目錄」這個 inode
// 必須沒有換過 —— 那正是 :ro bind mount 綁住的東西。目錄的 inode 不變，
// 容器內下一次 gate 呼叫就讀得到新內容。
func TestRevokeKeepsSessionDirInodeStable(t *testing.T) {
	root := t.TempDir()
	const s = "aa-demo-ctx"
	if err := WriteSandboxPolicy(root, SandboxPolicy{Session: s, Level: GrantDevelop}); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir, _ := PolicySessionDir(root, s)
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if err := RevokeSandboxPolicy(root, s); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir after revoke: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("session policy dir inode changed across revoke; a :ro bind mount of it would go stale")
	}
	got, err := LoadSandboxPolicy(root, s)
	if err != nil {
		t.Fatalf("load after revoke: %v", err)
	}
	if got.Level != GrantRevoked {
		t.Fatalf("level = %q, want revoked", got.Level)
	}
}

func TestWriteSandboxPolicyDirIsNotWorldReadable(t *testing.T) {
	root := t.TempDir()
	const s = "aa-demo-ctx"
	if err := WriteSandboxPolicy(root, SandboxPolicy{Session: s, Level: GrantReadOnly}); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir, _ := PolicySessionDir(root, s)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o, want 700", fi.Mode().Perm())
	}
	p, _ := PolicyPath(root, s)
	pf, _ := os.Stat(p)
	if pf.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 600", pf.Mode().Perm())
	}
}

func TestRemoveSandboxPolicyDropsDirAndLegacyFile(t *testing.T) {
	root := t.TempDir()
	const s = "aa-demo-ctx"
	if err := WriteSandboxPolicy(root, SandboxPolicy{Session: s, Level: GrantFull}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 模擬升級前留下的舊格式檔案。
	legacy := filepath.Join(PolicyDir(root), s+".json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := RemoveSandboxPolicy(root, s); err != nil {
		t.Fatalf("remove: %v", err)
	}
	dir, _ := PolicySessionDir(root, s)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("per-session dir must be gone")
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy policy file must be cleaned up too")
	}
	// 重複刪除必須是 no-op（sweep 會重試）。
	if err := RemoveSandboxPolicy(root, s); err != nil {
		t.Fatalf("second remove must be nil, got %v", err)
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestPolicy|TestRevokeKeeps|TestWriteSandboxPolicyDir|TestRemoveSandboxPolicy' -race -v`
Expected: FAIL —— `undefined: PolicySessionDir`，以及 `TestPolicyPathIsPerSessionDirectory` 拿到舊的 `<session>.json`

- [ ] **Step 3: 改 `a2a_policy.go`**

```go
// PolicySessionDir 回傳這個 session 專屬的政策目錄。
//
// 為什麼是「一個 session 一個目錄」而不是「一個 session 一個檔」：容器路線
// 把政策檔以 :ro bind mount 掛進沙盒，而 AtomicWriteJSONMode 走的是
// write-temp-then-rename —— rename 會換掉 inode，而單一檔案的 bind mount
// 綁的是舊 inode，容器內會永遠看到撤銷前的內容。掛目錄就沒有這個問題：
// 目錄的 inode 從頭到尾不變，裡面的檔案怎麼 rename 都看得到。
//
// 那為什麼不乾脆掛整個 a2a-policies/：那樣每個沙盒都看得到所有其他沙盒的
// 政策，包含別的 caller id。專屬目錄是這兩個限制之間唯一的交集。
func PolicySessionDir(root, session string) (string, error) {
	if !sandboxSessionRe.MatchString(session) {
		return "", fmt.Errorf("invalid sandbox session name %q", session)
	}
	return filepath.Join(PolicyDir(root), session), nil
}

// PolicyPath 回傳 session 的政策檔路徑，並在拼路徑前先驗證 session 名。
func PolicyPath(root, session string) (string, error) {
	dir, err := PolicySessionDir(root, session)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "policy.json"), nil
}
```

`WriteSandboxPolicy` 在寫檔前先建目錄（0700 —— 政策檔含 caller id，目錄本身也不該讓別人列舉）：

```go
func WriteSandboxPolicy(root string, p SandboxPolicy) error {
	dir, err := PolicySessionDir(root, p.Session)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll 對已存在的目錄不會修正權限，明確再設一次：升級路徑上可能
	// 已經有一個 0755 的 a2a-policies/<session>/。
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path, err := PolicyPath(root, p.Session)
	if err != nil {
		return err
	}
	if p.WrittenAt == "" {
		p.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	}
	return AtomicWriteJSONMode(path, p, 0o600)
}
```

`RemoveSandboxPolicy` 改成刪目錄，並清掉舊格式：

```go
// RemoveSandboxPolicy 在 sweep 回收沙盒時刪除整個 per-session 政策目錄。
// 順手刪掉升級前留下的舊格式 <root>/a2a-policies/<session>.json：那個路徑
// 已經沒有任何讀取者（LoadSandboxPolicy 只看新路徑；讀不到就是
// denied_no_policy，fail-closed），留著只是垃圾。
// 清不掉只由呼叫端 log，不影響回收判定（下一趟會重試）。
func RemoveSandboxPolicy(root, session string) error {
	dir, err := PolicySessionDir(root, session)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	legacy := filepath.Join(PolicyDir(root), session+".json")
	if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
```

`RevokeSandboxPolicy` 與 `LoadSandboxPolicy` **不改**：它們都經由 `PolicyPath`，自動跟著走新路徑。

- [ ] **Step 4: 跑測試確認它通過**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestPolicy|TestRevoke|TestWriteSandboxPolicy|TestRemoveSandboxPolicy|TestSandboxGate|TestSandboxDecision' -race -v`
Expected: PASS。gate 的既有測試也必須全綠——它們經由 `WriteSandboxPolicy` / `LoadSandboxPolicy`，路徑改變對它們應該完全透明。若有測試寫死了 `<session>.json` 這個字面路徑，那個測試要一起改（那正是它該被改的地方）。

- [ ] **Step 5: 跑全套 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -10`
Expected: 全部 ok

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_policy.go internal/channelagent/a2a_policy_test.go
git commit -m "fix(a2a): per-session policy directory so a :ro mount survives atomic rewrites"
```

---

### Task 5: 掛載表與環境變數的組裝（純函式 + 不變量測試）

**這個 task 的產出是本設計的安全邊界本身。** 「哪些路徑存在」從「Bash 允許清單猜出來的近似值」換成「掛載表列舉出來的事實」，而列舉表的正確性只有這一組測試在守。它刻意做成純函式（不呼叫 docker），於是不變量可以在沒有 daemon 的環境跑。

**Files:**
- Create: `internal/channelagent/a2a_container.go`
- Create: `internal/channelagent/a2a_container_test.go`

**Interfaces:**
- Consumes: `PolicySessionDir`（Task 4）、`GateLogPath`（`a2a_gate.go:672`）、`SandboxRoot`、`cleanAbs`、`sandboxSessionRe`、`DefaultSandboxImage`（Task 1）
- Produces:
  - `type ContainerOpts struct{ Image, Network, ProxyURL, ClaudeBinary, CronBinary, TokenEnvVar, Memory, CPUs string; PidsLimit, UID, GID int }`
  - `func DefaultContainerOpts() ContainerOpts`
  - `type ContainerSessionManager struct{ Root string; Opts ContainerOpts; Docker dockerRunner }`
  - `func ContainerName(session string) (string, error)` → `cc-a2a-<session>`
  - `func gitCommonDirFor(worktree string) (string, error)`
  - `func (m ContainerSessionManager) runArgs(pol SandboxPolicy, sandboxRoot string, startedAt time.Time) ([]string, error)`
  - `func GateSpoolPath(root, session string) (string, error)`（Task 8 會用；這裡先定義，因為掛載表要引用它）
  - `const DefaultSandboxNetwork = "cc-a2a"`、`DefaultEgressProxy = "http://cc-a2a-egress:3128"`、`DefaultTokenEnvVar = "A2A_CLAUDE_CODE_OAUTH_TOKEN"`

**兩個設計決定，明確記錄：**

1. **等級從政策檔讀，不從參數傳。** `SessionManager.Start(ctx, session, cwd, registryRoot)` 沒有帶授權等級，而 worktree 要掛 `:ro` 還是 `:rw` 取決於等級。**不改介面**（改了會波及 `FakeSessionManager` 與所有既有測試），改成在 `Start` 裡 `LoadSandboxPolicy(m.Root, session)`。這不是將就：`SandboxExecutor.Start`（`a2a_executor.go:284`）保證政策檔在 `Sessions.Start` **之前**就已落地，而讓「掛載模式」與「gate 判定用的等級」讀同一份檔案，結構上就不可能分岔。政策檔讀不到 = 拒絕啟動（fail-closed）。
2. **OAuth token 走子行程環境，不走 argv。** `docker run -e NAME=value` 會把 token 放進 `docker` 行程的 argv，同一台機器上任何使用者 `ps` 都看得到。改成 `-e CLAUDE_CODE_OAUTH_TOKEN`（**只有名字**），值由 `dockerRunner.Run` 的 `env` 參數帶進子行程環境。這也是 `dockerRunner.Run` 為什麼有 `env` 參數的唯一理由。

- [ ] **Step 1: 寫失敗的測試**

```go
package channelagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mountsOf 從 docker run 的 argv 拆出所有 -v 的值，方便逐條斷言。
func mountsOf(args []string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// seedWorktree 造出一個 git worktree 的「指標檔」佈局，不需要真的 git。
func seedWorktree(t *testing.T) (worktree, gitCommon string) {
	t.Helper()
	base := t.TempDir()
	project := filepath.Join(base, "proj")
	gitCommon = filepath.Join(project, ".git")
	if err := os.MkdirAll(filepath.Join(gitCommon, "worktrees", "aa-demo-ctx"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(base, "aa-demo-ctx")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr := "gitdir: " + filepath.Join(gitCommon, "worktrees", "aa-demo-ctx") + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}
	return worktree, gitCommon
}

func TestGitCommonDirForWorktreePointer(t *testing.T) {
	worktree, gitCommon := seedWorktree(t)
	got, err := gitCommonDirFor(worktree)
	if err != nil {
		t.Fatalf("gitCommonDirFor: %v", err)
	}
	if got != gitCommon {
		t.Fatalf("got %q, want %q", got, gitCommon)
	}
}

func TestGitCommonDirForPlainCheckout(t *testing.T) {
	// 非 worktree 的一般 checkout：.git 是目錄，回傳它自己。
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := gitCommonDirFor(dir)
	if err != nil {
		t.Fatalf("gitCommonDirFor: %v", err)
	}
	if got != filepath.Join(dir, ".git") {
		t.Fatalf("got %q", got)
	}
}

func TestGitCommonDirForRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommonDirFor(dir); err == nil {
		t.Fatal("a .git file without a gitdir: line must be an error, not a guess")
	}
	// 完全沒有 .git 也必須是錯誤，不能默默掛一個不存在的路徑。
	if _, err := gitCommonDirFor(t.TempDir()); err == nil {
		t.Fatal("missing .git must be an error")
	}
}

func TestContainerNameValidatesSession(t *testing.T) {
	if n, err := ContainerName("aa-demo-ctx"); err != nil || n != "cc-a2a-aa-demo-ctx" {
		t.Fatalf("got %q/%v", n, err)
	}
	for _, bad := range []string{"", "cc-worker", "aa-a/b", "aa-../x"} {
		if _, err := ContainerName(bad); err == nil {
			t.Errorf("ContainerName(%q) must be rejected", bad)
		}
	}
}

// testManager 造一個 root 與掛載表都受控的 manager。
func testManager(t *testing.T) (ContainerSessionManager, string) {
	t.Helper()
	root := t.TempDir()
	opts := DefaultContainerOpts()
	opts.ClaudeBinary = filepath.Join(root, "fakebin", "claude")
	opts.CronBinary = filepath.Join(root, "fakebin", "claude-cron")
	return ContainerSessionManager{Root: root, Opts: opts, Docker: &fakeDocker{}}, root
}

func policyFor(t *testing.T, root, session string, level GrantLevel, worktree string) SandboxPolicy {
	t.Helper()
	return SandboxPolicy{
		Session:     session,
		ContextID:   "ctx",
		Agent:       "demo",
		CallerID:    "caller-1",
		Level:       level,
		Worktree:    cleanAbs(worktree),
		SandboxRoot: cleanAbs(SandboxRoot(root, session)),
	}
}

func TestRunArgsMountTableReadonlyLevel(t *testing.T) {
	m, root := testManager(t)
	worktree, gitCommon := seedWorktree(t)
	const s = "aa-demo-ctx"
	sandboxRoot := SandboxRoot(root, s)
	pol := policyFor(t, root, s, GrantReadOnly, worktree)

	args, err := m.runArgs(pol, sandboxRoot, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	got := mountsOf(args)

	polDir, _ := PolicySessionDir(root, s)
	spool, _ := GateSpoolPath(root, s)
	want := []string{
		worktree + ":" + worktree + ":ro",                       // readonly → 核心強制唯讀
		gitCommon + ":" + gitCommon + ":rw",                     // 共用 repo 的 .git，必須 rw（風險 1）
		sandboxRoot + ":" + sandboxRoot + ":rw",                 // inbox/outbox/state
		polDir + ":" + polDir + ":ro",                           // 政策檔：核心強制唯讀
		spool + ":" + GateLogPath(root) + ":rw",                 // 唯一刻意不同構的掛載
		m.Opts.ClaudeBinary + ":/usr/local/bin/claude:ro",
		m.Opts.CronBinary + ":/usr/local/bin/claude-cron:ro",
	}
	if len(got) != len(want) {
		t.Fatalf("mount count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mount %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunArgsWorktreeIsWritableForDevelopAndFull(t *testing.T) {
	for _, lvl := range []GrantLevel{GrantDevelop, GrantFull} {
		m, root := testManager(t)
		worktree, _ := seedWorktree(t)
		const s = "aa-demo-ctx"
		args, err := m.runArgs(policyFor(t, root, s, lvl, worktree), SandboxRoot(root, s), time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatalf("%s: runArgs: %v", lvl, err)
		}
		if !hasArg(mountsOf(args), worktree+":"+worktree+":rw") {
			t.Fatalf("%s: worktree must be rw, mounts=%v", lvl, mountsOf(args))
		}
	}
}

func TestRunArgsRefusesRevokedOrUnknownLevel(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	for _, lvl := range []GrantLevel{GrantRevoked, GrantLevel(""), GrantLevel("wat")} {
		if _, err := m.runArgs(policyFor(t, root, s, lvl, worktree), SandboxRoot(root, s), time.Now()); err == nil {
			t.Errorf("level %q must refuse to build a container", lvl)
		}
	}
}

// 這一組是「沒列在掛載表裡的東西，容器內看不到」這條規則的守門測試。
func TestRunArgsNeverExposesTheRegistryRootOrSecrets(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	args, err := m.runArgs(policyFor(t, root, s, GrantFull, worktree), SandboxRoot(root, s), time.Now())
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, mnt := range mountsOf(args) {
		src := strings.SplitN(mnt, ":", 2)[0]
		// root 本身（含 bindings.json / callers.json / agents.json /
		// tasks.json / a2a-audit.jsonl）絕不可整個掛進去。
		if src == root {
			t.Fatalf("registry root %q must never be mounted, got %q", root, mnt)
		}
		for _, forbidden := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
			if strings.Contains(mnt, forbidden) {
				t.Fatalf("docker socket must never be mounted, got %q", mnt)
			}
		}
	}
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"bindings.json", "callers.json", "agents.json", "tasks.json", ".credentials.json", "/.env"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("argv must not reference %q: %s", forbidden, joined)
		}
	}
}

func TestRunArgsHardening(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	args, err := m.runArgs(policyFor(t, root, s, GrantDevelop, worktree), SandboxRoot(root, s), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, want := range []string{
		"--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"--user", "1000:1000", "--network", DefaultSandboxNetwork,
		"--memory=2g", "--memory-swap=2g", "--pids-limit=512", "--cpus=2",
		"--init", "-d", "-t",
		"--name", "cc-a2a-aa-demo-ctx",
		"--label", "cc.a2a.session=aa-demo-ctx",
		"--label", "cc.a2a.started=1970-01-01T00:00:00Z",
		"-w", worktree,
	} {
		if !hasArg(args, want) {
			t.Errorf("missing %q in argv: %v", want, args)
		}
	}
	// 明確不開 --read-only 根檔案系統（規格第二節決定 3）。
	if hasArg(args, "--read-only") {
		t.Error("--read-only was deliberately rejected; do not add it silently")
	}
}

func TestRunArgsEnvironment(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	sandboxRoot := SandboxRoot(root, s)
	args, err := m.runArgs(policyFor(t, root, s, GrantDevelop, worktree), sandboxRoot, time.Now())
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, want := range []string{
		"CC_REGISTRY_ROOT=" + sandboxRoot,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(sandboxRoot, "claude"),
		"HOME=" + filepath.Join(sandboxRoot, "home"),
		"HTTPS_PROXY=" + DefaultEgressProxy,
		"HTTP_PROXY=" + DefaultEgressProxy,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"CLAUDE_CODE_OAUTH_TOKEN", // 只有名字，值走子行程環境
	} {
		if !hasArg(args, want) {
			t.Errorf("missing env arg %q in %v", want, args)
		}
	}
	// token 的「值」絕不可以出現在 argv 裡（ps 看得到）。
	for _, a := range args {
		if strings.HasPrefix(a, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Fatalf("token value must never appear in argv, got %q", a)
		}
	}
}

func TestRunArgsCommandIsTmuxRunningClaudeWithoutAPIKeys(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	args, err := m.runArgs(policyFor(t, root, s, GrantDevelop, worktree), SandboxRoot(root, s), time.Now())
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	tail := args[len(args)-9:]
	want := []string{
		DefaultSandboxImage,
		"tmux", "new-session", "-s", s,
		"env", "-u", "ANTHROPIC_API_KEY", "-u",
	}
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("tail[%d] = %q, want %q (full: %v)", i, tail[i], want[i], args)
		}
	}
	joined := strings.Join(args, " ")
	if !strings.HasSuffix(joined, "-u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN claude") {
		t.Fatalf("command must strip both API credentials and exec claude, got %q", joined)
	}
}

func TestRunArgsRejectsMismatchedSandboxRoot(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	pol := policyFor(t, root, s, GrantDevelop, worktree)
	pol.SandboxRoot = "/somewhere/else"
	if _, err := m.runArgs(pol, SandboxRoot(root, s), time.Now()); err == nil {
		t.Fatal("policy's SandboxRoot must match the caller's registryRoot, or refuse")
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestGitCommonDir|TestContainerName|TestRunArgs' -race -v`
Expected: FAIL —— `undefined: gitCommonDirFor` 等

- [ ] **Step 3: 寫 `a2a_container.go` 的組裝部分**

```go
package channelagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSandboxNetwork 是 --internal 網路的名字。實測（2026-08-07）
	// 確認 --internal 同時擋掉 host 發佈的 0.0.0.0 埠、docker0 gateway
	// 與所有對外 NAT，因此不需要任何 iptables 規則；也因此沙盒必須經由
	// 出口代理才連得到上游 API。
	DefaultSandboxNetwork = "cc-a2a"
	DefaultEgressProxy    = "http://cc-a2a-egress:3128"
	// DefaultTokenEnvVar 刻意不是 CLAUDE_CODE_OAUTH_TOKEN：後者是
	// oauthTokenEnvArgs()（worktree.go:421）注進「每一個 cc- session」的
	// 變數。用不同的名字，是「這份 token 只給 aa- 容器、可獨立撤銷」
	// 這個唯一買到的好處在機制上成立的前提。
	DefaultTokenEnvVar = "A2A_CLAUDE_CODE_OAUTH_TOKEN"
)

// ContainerOpts 是啟動一個沙盒容器需要的全部外部事實。全部在 serve 啟動時
// 解析一次（main.go），於是 runArgs 是一個純函式，不讀環境、不查檔案系統
// 的家目錄，可以完整單元測試。
type ContainerOpts struct {
	Image     string
	Network   string
	ProxyURL  string
	Memory    string
	CPUs      string
	PidsLimit int
	UID, GID  int
	// ClaudeBinary / CronBinary 是 host 上的絕對路徑，以 :ro 掛進容器。
	// 刻意不烘進映像：275 MB × 每個版本，而且 host 換版之後下一個起來的
	// 沙盒自動用新版，不需要重建映像、不會漂移。
	ClaudeBinary string
	CronBinary   string
	// TokenEnvVar 是 serve 自己的環境裡放 A2A token 的變數名。
	TokenEnvVar string
}

func DefaultContainerOpts() ContainerOpts {
	return ContainerOpts{
		Image:       DefaultSandboxImage,
		Network:     DefaultSandboxNetwork,
		ProxyURL:    DefaultEgressProxy,
		Memory:      "2g",
		CPUs:        "2",
		PidsLimit:   512,
		UID:         1000,
		GID:         1000,
		TokenEnvVar: DefaultTokenEnvVar,
	}
}

// ContainerSessionManager 是 SessionManager 的容器實作。介面本身一個方法都
// 不改（a2a_session.go:14 的六個方法對容器一樣成立）。
type ContainerSessionManager struct {
	Root   string
	Opts   ContainerOpts
	Docker dockerRunner
}

// ContainerName 由 session 名確定性推導，並在拼名字之前驗證 session ——
// 容器名會直接進 docker CLI 的 argv，含 '/' 或 '..' 的名字不可接受。
func ContainerName(session string) (string, error) {
	if !sandboxSessionRe.MatchString(session) {
		return "", fmt.Errorf("invalid sandbox session name %q", session)
	}
	return "cc-a2a-" + session, nil
}

// gitCommonDirFor 找出這個 worktree 背後的共用 .git 目錄。
//
// git worktree add 產生的 <worktree>/.git 是一個指標「檔」，內容形如
//     gitdir: /path/to/project/.git/worktrees/<name>
// 沒有把 /path/to/project/.git 掛進容器，容器內連 git status 都跑不動
// （Task 2 實證過）。這是本設計最大的一個洞：那個掛載是 rw，於是沙盒碰得到
// 同一專案下所有 cc- binding 共用的 ref 與 object。gate 的 gitDecision /
// pushArgsAllowed 仍然是唯一的防線，容器沒有取代它。
//
// 猜不出來就回錯，絕不回一個「大概是這個」的路徑：掛錯目錄的後果是把不該
// 給的東西給出去。
func gitCommonDirFor(worktree string) (string, error) {
	dot := filepath.Join(cleanAbs(worktree), ".git")
	info, err := os.Lstat(dot)
	if err != nil {
		return "", fmt.Errorf("locate git dir for %s: %w", worktree, err)
	}
	if info.IsDir() {
		return dot, nil // 一般 checkout，不是 worktree
	}
	blob, err := os.ReadFile(dot)
	if err != nil {
		return "", fmt.Errorf("read git pointer %s: %w", dot, err)
	}
	line := strings.TrimSpace(string(blob))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("git pointer %s has no gitdir: line", dot)
	}
	gitdir := cleanAbs(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	// gitdir 形如 <common>/worktrees/<name>；往上兩層才是共用的 .git。
	parent := filepath.Dir(gitdir)
	if filepath.Base(parent) != "worktrees" {
		return "", fmt.Errorf("unexpected gitdir layout %q in %s", gitdir, dot)
	}
	return filepath.Dir(parent), nil
}

// runArgs 組出 `docker run` 的完整 argv。這是整份設計的安全邊界：沒有列在
// 這裡的東西，容器內就不存在。任何新增掛載都必須先通過
// TestRunArgsNeverExposesTheRegistryRootOrSecrets。
func (m ContainerSessionManager) runArgs(pol SandboxPolicy, sandboxRoot string, startedAt time.Time) ([]string, error) {
	name, err := ContainerName(pol.Session)
	if err != nil {
		return nil, err
	}
	// 等級決定 worktree 掛 ro 還是 rw。revoked 或未知等級一律拒絕開容器：
	// 一個沒有有效等級的沙盒本來就不該存在（a2a_executor.go 也是這樣擋的）。
	var worktreeMode string
	switch pol.Level {
	case GrantReadOnly:
		worktreeMode = "ro"
	case GrantDevelop, GrantFull:
		worktreeMode = "rw"
	default:
		return nil, fmt.Errorf("sandbox %s has no grantable level (%q); refusing to start a container", pol.Session, pol.Level)
	}
	worktree := cleanAbs(pol.Worktree)
	if worktree == "" {
		return nil, fmt.Errorf("sandbox %s has no worktree", pol.Session)
	}
	sandboxRoot = cleanAbs(sandboxRoot)
	// 政策檔與呼叫方對「這是哪一個沙盒」的認知必須一致，否則就是身分錯位，
	// 不是可以繼續的情況。
	if pol.SandboxRoot != "" && pol.SandboxRoot != sandboxRoot {
		return nil, fmt.Errorf("sandbox %s: policy root %q != registry root %q", pol.Session, pol.SandboxRoot, sandboxRoot)
	}
	gitCommon, err := gitCommonDirFor(worktree)
	if err != nil {
		return nil, err
	}
	polDir, err := PolicySessionDir(m.Root, pol.Session)
	if err != nil {
		return nil, err
	}
	spool, err := GateSpoolPath(m.Root, pol.Session)
	if err != nil {
		return nil, err
	}
	if m.Opts.ClaudeBinary == "" || m.Opts.CronBinary == "" {
		return nil, fmt.Errorf("container opts are missing the claude/claude-cron host paths")
	}

	args := []string{
		"run", "-d", "-t",
		"--name", name,
		"--label", "cc.a2a.session=" + pol.Session,
		"--label", "cc.a2a.started=" + startedAt.UTC().Format(time.RFC3339),
		"--network", m.Opts.Network,
		"--user", strconv.Itoa(m.Opts.UID) + ":" + strconv.Itoa(m.Opts.GID),
		"--init",
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
		"--memory=" + m.Opts.Memory,
		"--memory-swap=" + m.Opts.Memory,
		"--pids-limit=" + strconv.Itoa(m.Opts.PidsLimit),
		"--cpus=" + m.Opts.CPUs,
		"-w", worktree,
	}

	// 掛載表。順序固定，測試逐條比對。
	// 路徑同構（host 路徑 == 容器內路徑）不是美觀問題：它讓
	// BuildClaudePrompt 寫進 prompt 的絕對路徑、CollectResults 掃的 outbox
	// 路徑、gate 的 SandboxSessionFromRegistryRoot 反推、以及
	// sandboxPathInScope 的絕對路徑比對，全部一行都不用改。
	mounts := [][3]string{
		{worktree, worktree, worktreeMode},
		{gitCommon, gitCommon, "rw"},
		{sandboxRoot, sandboxRoot, "rw"},
		{polDir, polDir, "ro"}, // 核心強制唯讀 = develop 的自我提權路徑就此關閉
		// 唯一刻意不同構的掛載：容器內的 gate 以為自己在寫正本，
		// 程式碼一行不改；host 每個 cycle 把 spool 搬進真正的 gate log。
		{spool, GateLogPath(m.Root), "rw"},
		{m.Opts.ClaudeBinary, "/usr/local/bin/claude", "ro"},
		{m.Opts.CronBinary, "/usr/local/bin/claude-cron", "ro"},
	}
	for _, mt := range mounts {
		args = append(args, "-v", mt[0]+":"+mt[1]+":"+mt[2])
	}

	// 環境變數。CLAUDE_CONFIG_DIR 指向沙盒自己的目錄，附帶三個收穫：
	// 沙盒不繼承 operator 的 CLAUDE.md / skills / plugin；沒有任何 MCP
	// 設定（full 的 mcp__* 放行因此是空集合）；EnsureFolderTrusted 不再
	// 寫全機共用的 ~/.claude.json。
	env := []string{
		"CC_REGISTRY_ROOT=" + sandboxRoot,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(sandboxRoot, "claude"),
		"HOME=" + filepath.Join(sandboxRoot, "home"),
		"HTTPS_PROXY=" + m.Opts.ProxyURL,
		"HTTP_PROXY=" + m.Opts.ProxyURL,
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	// 只給「名字」，值由 dockerRunner.Run 的 env 參數帶進子行程環境。
	// 寫成 -e NAME=value 會讓 token 出現在 ps 裡。
	args = append(args, "-e", "CLAUDE_CODE_OAUTH_TOKEN")

	args = append(args, m.Opts.Image,
		"tmux", "new-session", "-s", pol.Session,
		"env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN", "claude")
	return args, nil
}
```

同時在本檔先放下 `GateSpoolPath`（Task 8 會補 `DrainGateSpool`）：

```go
// GateSpoolDir / GateSpoolPath —— 每個沙盒一個 spool 檔，以 rw 掛在容器內
// 的 GateLogPath 位置。host 的 A2A cycle 每輪把它 drain 進正本。
func GateSpoolDir(root string) string { return filepath.Join(root, "a2a-gate-spool") }

func GateSpoolPath(root, session string) (string, error) {
	if !sandboxSessionRe.MatchString(session) {
		return "", fmt.Errorf("invalid sandbox session name %q", session)
	}
	return filepath.Join(GateSpoolDir(root), session+".jsonl"), nil
}
```

- [ ] **Step 4: 跑測試確認它通過**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestGitCommonDir|TestContainerName|TestRunArgs' -race -v`
Expected: PASS（11 個測試）

- [ ] **Step 5: 跑全套 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok（本 task 沒有任何呼叫端，純新增）

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_container.go internal/channelagent/a2a_container_test.go
git commit -m "feat(a2a): container mount table as the enumerated security boundary"
```

---

### Task 6: `ContainerSessionManager` 的六個生命週期方法（含 docker 的三分法）

**Files:**
- Modify: `internal/channelagent/a2a_container.go`
- Modify: `internal/channelagent/a2a_container_test.go`

**Interfaces:**
- Consumes: Task 5 的 `runArgs` / `ContainerName`；Task 1 的 `dockerRunner` / `dockerSaysAbsent`；既有的 `EnsureWorktree`、`RemoveWorktree`、`EnsureSandboxSettings`、`EnsureFolderTrusted`、`IngestMessages`、`LoadSandboxPolicy`
- Produces：`ContainerSessionManager` 完整實作 `SessionManager`（`a2a_session.go:14`）的六個方法，外加
  - `func ContainerAlive(ctx context.Context, dr dockerRunner, container string) (bool, error)`
  - `func RemoveContainer(ctx context.Context, dr dockerRunner, container string) error`
  - `func sandboxClaudeConfigPath(sandboxRoot string) string`
  - `func sessionFromSandboxWorktree(worktree string) (string, error)`

**三分法為什麼是這個 task 的驗收核心：** `SweepTimeouts`（`a2a_lifecycle.go:1140-1145`）在 `sm.Stop` 回非 nil 時**什麼都不刪**，把整個 candidate 留給下一輪。這條保護在容器路線下更重要——一個還在跑的容器，它的 bind mount 目標被刪掉，行為未定義。所以 `Stop` 必須嚴格區分「docker 明確說沒有這個容器」（= 成功）與「問不到答案」（daemon 沒起來、`docker` 找不到、ctx 取消 → 必須回 error）。`Alive` 同理，`VanishedConfirmStrikes = 2` 的既有機制不動。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestContainerAliveThreeWay(t *testing.T) {
	// 活著
	dr := &fakeDocker{out: []string{"true\n"}, errs: []error{nil}}
	ok, err := ContainerAlive(context.Background(), dr, "cc-a2a-aa-x")
	if err != nil || !ok {
		t.Fatalf("running: ok=%v err=%v", ok, err)
	}
	want := []string{"inspect", "-f", "{{.State.Running}}", "cc-a2a-aa-x"}
	if !equalStrings(dr.calls[0], want) {
		t.Fatalf("argv = %v, want %v", dr.calls[0], want)
	}
	// 存在但已停止
	dr = &fakeDocker{out: []string{"false\n"}, errs: []error{nil}}
	if ok, err := ContainerAlive(context.Background(), dr, "c"); err != nil || ok {
		t.Fatalf("stopped: ok=%v err=%v, want false/nil", ok, err)
	}
	// 明確不存在
	dr = &fakeDocker{errs: []error{exitErr("Error: No such object: c")}}
	if ok, err := ContainerAlive(context.Background(), dr, "c"); err != nil || ok {
		t.Fatalf("absent: ok=%v err=%v, want false/nil", ok, err)
	}
	// 問不到答案 —— 絕不可以回 (false, nil)，那會讓 sweep 在容器還活著時拆它
	dr = &fakeDocker{errs: []error{exitErr("Cannot connect to the Docker daemon")}}
	if _, err := ContainerAlive(context.Background(), dr, "c"); err == nil {
		t.Fatal("daemon down must be an error, not a confident false")
	}
	// 看不懂的輸出也是「問不到答案」
	dr = &fakeDocker{out: []string{"<no value>\n"}, errs: []error{nil}}
	if _, err := ContainerAlive(context.Background(), dr, "c"); err == nil {
		t.Fatal("unparseable output must be an error")
	}
}

func TestRemoveContainerThreeWay(t *testing.T) {
	dr := &fakeDocker{errs: []error{nil}}
	if err := RemoveContainer(context.Background(), dr, "cc-a2a-aa-x"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	want := []string{"rm", "-f", "cc-a2a-aa-x"}
	if !equalStrings(dr.calls[0], want) {
		t.Fatalf("argv = %v, want %v", dr.calls[0], want)
	}
	// 已經不在 = 成功
	dr = &fakeDocker{errs: []error{exitErr("Error response from daemon: No such container: c")}}
	if err := RemoveContainer(context.Background(), dr, "c"); err != nil {
		t.Fatalf("absent container must be success, got %v", err)
	}
	// 問不到答案 = 失敗，sweep 因此本輪不拆任何東西
	dr = &fakeDocker{errs: []error{exitErr("Cannot connect to the Docker daemon")}}
	if err := RemoveContainer(context.Background(), dr, "c"); err == nil {
		t.Fatal("daemon down must fail so sweep leaves the worktree alone")
	}
}

func TestContainerManagerStopUsesDerivedName(t *testing.T) {
	m, _ := testManager(t)
	dr := &fakeDocker{errs: []error{nil}}
	m.Docker = dr
	if err := m.Stop(context.Background(), "aa-demo-ctx"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !equalStrings(dr.calls[0], []string{"rm", "-f", "cc-a2a-aa-demo-ctx"}) {
		t.Fatalf("argv = %v", dr.calls[0])
	}
	// 非法 session 名不得產生任何 docker 呼叫
	dr2 := &fakeDocker{}
	m.Docker = dr2
	if err := m.Stop(context.Background(), "cc-worker"); err == nil {
		t.Fatal("a non-aa- session must be refused")
	}
	if len(dr2.calls) != 0 {
		t.Fatalf("refused session must not shell out, got %v", dr2.calls)
	}
}

func TestContainerManagerStartWritesHostSideLayoutThenRuns(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	sandboxRoot := SandboxRoot(root, s)
	if err := os.MkdirAll(sandboxRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteSandboxPolicy(root, policyFor(t, root, s, GrantDevelop, worktree)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// 三次呼叫：rm -f（清掉同名殘留）、run、以及開機就緒檢查的 exec。
	dr := &fakeDocker{
		out:  []string{"", "cid\n", ""},
		errs: []error{exitErr("No such container: cc-a2a-aa-demo-ctx"), nil, nil},
	}
	m.Docker = dr

	if err := m.Start(context.Background(), s, worktree, sandboxRoot); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(dr.calls) < 2 || dr.calls[0][0] != "rm" || dr.calls[1][0] != "run" {
		t.Fatalf("expected rm then run, got %v", dr.calls)
	}
	// host 端佈局必須在 docker run 之前就位，否則 docker 會把不存在的
	// bind mount 來源自動建成「目錄」——gate spool 一旦變成目錄，容器內的
	// gate 每次寫入都會失敗。
	for _, p := range []string{
		filepath.Join(sandboxRoot, "claude"),
		filepath.Join(sandboxRoot, "home"),
	} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			t.Errorf("%s must exist as a directory before docker run", p)
		}
	}
	spool, _ := GateSpoolPath(root, s)
	if fi, err := os.Stat(spool); err != nil || fi.IsDir() {
		t.Errorf("gate spool %s must exist as a regular file before docker run", spool)
	}
	// token 的值只能進子行程環境，不能進 argv。
	var sawTokenEnv bool
	for _, e := range dr.envs[1] {
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			sawTokenEnv = true
		}
	}
	if !sawTokenEnv {
		t.Error("docker run child env must carry CLAUDE_CODE_OAUTH_TOKEN")
	}
}

func TestContainerManagerStartRefusesWithoutPolicy(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	dr := &fakeDocker{}
	m.Docker = dr
	err := m.Start(context.Background(), s, worktree, SandboxRoot(root, s))
	if err == nil {
		t.Fatal("no policy on disk must refuse to start (fail closed)")
	}
	for _, c := range dr.calls {
		if c[0] == "run" {
			t.Fatal("must not docker run without a policy")
		}
	}
}

func TestContainerManagerStartCleansUpAfterAFailedBoot(t *testing.T) {
	m, root := testManager(t)
	worktree, _ := seedWorktree(t)
	const s = "aa-demo-ctx"
	sandboxRoot := SandboxRoot(root, s)
	_ = os.MkdirAll(sandboxRoot, 0o755)
	_ = WriteSandboxPolicy(root, policyFor(t, root, s, GrantReadOnly, worktree))
	// rm（沒有殘留）→ run 成功 → 就緒檢查一直失敗
	dr := &fakeDocker{
		out: []string{"", "cid\n", "", "", ""},
		errs: []error{
			exitErr("No such container: x"), nil,
			exitErr("session not found"), exitErr("session not found"), nil,
		},
	}
	m.Docker = dr
	origAttempts := containerReadyAttempts
	containerReadyAttempts = 2
	defer func() { containerReadyAttempts = origAttempts }()

	if err := m.Start(context.Background(), s, worktree, sandboxRoot); err == nil {
		t.Fatal("a container whose tmux session never appears must fail Start")
	}
	last := dr.calls[len(dr.calls)-1]
	if last[0] != "rm" {
		t.Fatalf("a failed boot must remove the container it created, last call = %v", last)
	}
}

func TestContainerManagerTrustFolderWritesSandboxConfigNotOperatorConfig(t *testing.T) {
	m, root := testManager(t)
	const s = "aa-demo-ctx"
	sandboxRoot := SandboxRoot(root, s)
	worktree := filepath.Join(t.TempDir(), s)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.TrustFolder(context.Background(), worktree); err != nil {
		t.Fatalf("TrustFolder: %v", err)
	}
	cfg := sandboxClaudeConfigPath(sandboxRoot)
	blob, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("sandbox config must exist at %s: %v", cfg, err)
	}
	if !strings.Contains(string(blob), cleanAbs(worktree)) {
		t.Fatalf("sandbox config must record the trusted worktree, got %s", blob)
	}
}

func TestSessionFromSandboxWorktree(t *testing.T) {
	if s, err := sessionFromSandboxWorktree("/home/x/aa-demo-ctx"); err != nil || s != "aa-demo-ctx" {
		t.Fatalf("got %q/%v", s, err)
	}
	for _, bad := range []string{"/home/x/cc-worker", "/home/x", "/"} {
		if _, err := sessionFromSandboxWorktree(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestContainer|TestRemoveContainer|TestSessionFromSandbox' -race -v`
Expected: FAIL —— `undefined: ContainerAlive` 等

- [ ] **Step 3: 寫實作（續 `a2a_container.go`）**

```go
// containerReadyAttempts 是「容器起來之後，等它裡面的 tmux session 出現」
// 的重試次數，每次間隔 containerReadyInterval。可在測試中調小。
var (
	containerReadyAttempts = 60
	containerReadyInterval = 500 * time.Millisecond
)

// ContainerAlive 三分法，與 TmuxSessionAlive 完全一致的語意：
// (true, nil) 在跑；(false, nil) 明確沒在跑（含「沒有這個容器」）；
// (_, err) 問不到答案 —— 呼叫方必須先當它還活著，之後再查。
func ContainerAlive(ctx context.Context, dr dockerRunner, container string) (bool, error) {
	out, err := dr.Run(ctx, nil, "inspect", "-f", "{{.State.Running}}", container)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if dockerSaysAbsent(err) {
			return false, nil
		}
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	// 看不懂 = 沒問到答案。絕不猜。
	return false, fmt.Errorf("docker inspect %s: unparseable state %q", container, strings.TrimSpace(out))
}

// RemoveContainer 是 tmux 路線 StopTmuxSession 的對應物。「已經不在」是
// 成功；「問不到答案」必須是失敗 —— sweep 靠這個區分決定要不要拆 worktree。
func RemoveContainer(ctx context.Context, dr dockerRunner, container string) error {
	if _, err := dr.Run(ctx, nil, "rm", "-f", container); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if dockerSaysAbsent(err) {
			return nil
		}
		return err
	}
	return nil
}

// --- SessionManager 的六個方法 ---

// EnsureWorkspace / RemoveWorkspace 完全走 host 的 git：共用 repo 在 host 上，
// worktree 建好之後才掛進容器。與 TmuxSessionManager 逐字相同。
func (m ContainerSessionManager) EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error {
	return EnsureWorktree(ctx, projectDir, branch, worktree)
}

func (m ContainerSessionManager) RemoveWorkspace(ctx context.Context, projectDir, worktree string) error {
	return RemoveWorktree(ctx, projectDir, worktree)
}

// Inject 也完全不跨容器邊界：它在 host 寫 <sandboxRoot>/inbox/pending/，
// 而那個目錄剛好也掛在容器裡。與 TmuxSessionManager 逐字相同。
func (m ContainerSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	return TmuxSessionManager{}.Inject(ctx, root, msg)
}

// sandboxClaudeConfigPath 是這個沙盒自己的 ~/.claude.json —— 容器裡
// CLAUDE_CONFIG_DIR 指向 <sandboxRoot>/claude，Claude Code 在那裡找設定。
func sandboxClaudeConfigPath(sandboxRoot string) string {
	return filepath.Join(sandboxRoot, "claude", ".claude.json")
}

// sessionFromSandboxWorktree 從 worktree 路徑反推 session 名。
// SandboxWorktree(projectDir, session) 把 worktree 放在 projectDir 隔壁、
// 以 session 命名，所以 basename 就是 session；仍然要通過 sandboxSessionRe
// 才接受，避免任何非沙盒路徑走進這條路。
func sessionFromSandboxWorktree(worktree string) (string, error) {
	base := filepath.Base(cleanAbs(worktree))
	if !sandboxSessionRe.MatchString(base) {
		return "", fmt.Errorf("%q is not a sandbox worktree", worktree)
	}
	return base, nil
}

// TrustFolder 寫的是這個沙盒自己的設定檔，不是 operator 的 ~/.claude.json。
// 這正是容器路線讓 confinement F2 那條「必須走介面否則測試會改到線上設定」
// 失去存在理由的地方 —— 但介面本身保留（tmux 模式仍然寫共用檔）。
func (m ContainerSessionManager) TrustFolder(_ context.Context, worktree string) error {
	session, err := sessionFromSandboxWorktree(worktree)
	if err != nil {
		return err
	}
	cfg := sandboxClaudeConfigPath(SandboxRoot(m.Root, session))
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		return err
	}
	// EnsureFolderTrusted 要求檔案已存在（它 Stat 之後才讀）。第一次開機
	// 種一個空 JSON。
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
			return err
		}
	}
	return EnsureFolderTrusted(cfg, cleanAbs(worktree))
}

func (m ContainerSessionManager) Stop(ctx context.Context, session string) error {
	name, err := ContainerName(session)
	if err != nil {
		return err
	}
	return RemoveContainer(ctx, m.Docker, name)
}

func (m ContainerSessionManager) Alive(ctx context.Context, session string) (bool, error) {
	name, err := ContainerName(session)
	if err != nil {
		return false, err
	}
	return ContainerAlive(ctx, m.Docker, name)
}

// Start 起一個沙盒容器。順序有意義，逐條：
//  1. 讀政策檔 —— 掛載模式（worktree ro/rw）必須與 gate 判定用的等級來自
//     同一份檔案，結構上就不可能分岔。讀不到就拒絕（fail closed）。
//  2. 在 host 上把所有 bind mount 的來源準備好。docker 對不存在的來源會
//     「自動建成目錄」：gate spool 一旦變成目錄，容器內每次判定的寫入都會
//     失敗，而那是靜默的。
//  3. 寫沙盒版 settings.local.json（不含 SessionStart hook）—— 與 tmux
//     路線同一個 EnsureSandboxSettings，在 host 上做，readonly 的 :ro 掛載
//     因此不受影響。
//  4. 先 rm -f 同名殘留：serve 重啟之後可能留著一個上一輪的容器。
//  5. docker run，token 只走子行程環境。
//  6. 等容器內的 tmux session 真的出現。等不到就把自己建的容器收掉，不留孤兒。
func (m ContainerSessionManager) Start(ctx context.Context, session, cwd, registryRoot string) error {
	pol, err := LoadSandboxPolicy(m.Root, session)
	if err != nil {
		return fmt.Errorf("load sandbox policy for %s: %w", session, err)
	}
	name, err := ContainerName(session)
	if err != nil {
		return err
	}
	sandboxRoot := cleanAbs(registryRoot)
	for _, d := range []string{
		filepath.Join(sandboxRoot, "claude"),
		filepath.Join(sandboxRoot, "home"),
		GateSpoolDir(m.Root),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	spool, err := GateSpoolPath(m.Root, session)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(spool, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create gate spool: %w", err)
	}
	_ = f.Close()
	if err := EnsureSandboxSettings(cwd); err != nil {
		return err
	}

	args, err := m.runArgs(pol, sandboxRoot, time.Now())
	if err != nil {
		return err
	}
	// 同名殘留先清掉。這裡刻意用 RemoveContainer 的三分法：daemon 問不到
	// 就直接失敗，不要在狀態不明的情況下 run 一個同名容器。
	if err := RemoveContainer(ctx, m.Docker, name); err != nil {
		return fmt.Errorf("clear stale container %s: %w", name, err)
	}
	if _, err := m.Docker.Run(ctx, m.childEnv(), args...); err != nil {
		return fmt.Errorf("docker run %s: %w", name, err)
	}
	if err := m.waitSessionReady(ctx, name, session); err != nil {
		// 自己建的容器，起不來就自己收掉 —— 不要留給 ReapOrphanContainers，
		// 它有 5 分鐘的 grace，而這裡我們現在就確定它是廢的。
		if rmErr := RemoveContainer(ctx, m.Docker, name); rmErr != nil {
			log.Printf("a2a: 容器 %s 開機失敗且清不掉（留給 ReapOrphanContainers）: %v", name, rmErr)
		}
		return err
	}
	return nil
}

// childEnv 組出 docker CLI 子行程的環境：serve 自己的環境（去掉 TMUX），
// 外加把 A2A 專屬 token 改名成 claude 認得的變數名。值只存在於這個環境，
// 不進 argv（ps 看不到）。cc- session 完全不受影響：它們讀的是
// CLAUDE_CODE_OAUTH_TOKEN，而 serve 的環境裡那個變數是空的。
func (m ContainerSessionManager) childEnv() []string {
	env := envWithout(os.Environ(), "TMUX", "TMUX_PANE", "CLAUDE_CODE_OAUTH_TOKEN")
	if v := os.Getenv(m.Opts.TokenEnvVar); v != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+v)
	}
	return env
}

// waitSessionReady 等容器內的 tmux session 出現。容器 run 成功只代表
// docker 收下了指令；tmux 起不來（映像壞了、掛載被拒）要在這裡被抓到，
// 而不是等到 driver 每拍失敗、直到兩小時硬逾時。
func (m ContainerSessionManager) waitSessionReady(ctx context.Context, container, session string) error {
	var lastErr error
	for i := 0; i < containerReadyAttempts; i++ {
		if _, err := m.Docker.Run(ctx, nil, "exec", container, "tmux", "has-session", "-t", session); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(containerReadyInterval):
		}
	}
	return fmt.Errorf("container %s: tmux session %s never came up: %w", container, session, lastErr)
}
```

（本檔需補上 `context`、`errors`、`log` 的 import。）

- [ ] **Step 4: 跑測試確認它通過**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestContainer|TestRemoveContainer|TestSessionFromSandbox|TestRunArgs|TestGitCommonDir' -race -v`
Expected: PASS

- [ ] **Step 5: 靜態確認 `ContainerSessionManager` 真的滿足介面**

在 `a2a_container.go` 加一行編譯期斷言（比任何測試都可靠）：

```go
var _ SessionManager = ContainerSessionManager{}
```

- [ ] **Step 6: 跑全套 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok。**此時 `ContainerSessionManager` 完整存在但沒有任何呼叫端** —— 分支狀態是「tmux 模式完好，容器模式尚未接上」，這是刻意的（見第四節排序原則 2）。

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_container.go internal/channelagent/a2a_container_test.go
git commit -m "feat(a2a): ContainerSessionManager lifecycle with docker three-way error classification"
```

---

### Task 7: 兩種隔離並存的顯式開關（設定、per-agent 覆寫、逐列路由、serve 前置檢查）

**這是唯一的接線點。** 做完之後 `ContainerSessionManager` 才第一次被用到；但 `a2a.isolation` 預設仍是 `"tmux"`，所以**接線本身不改變任何現況行為**。

**Files:**
- Create: `internal/channelagent/a2a_isolation.go`
- Create: `internal/channelagent/a2a_isolation_test.go`
- Modify: `internal/channelagent/config.go`（`A2AConfig` + 取值方法）
- Modify: `internal/channelagent/a2a_agents.go`（`Agent.Isolation` / `Agent.Image`）
- Modify: `internal/channelagent/a2a_tasks.go`（`A2ATask.Isolation`）
- Modify: `internal/channelagent/a2a_executor.go`（dispatch 當下把有效隔離寫進 row）
- Modify: `cmd/claude-cron/main.go`（兩處 `TmuxSessionManager{}` 改成建好的路由器）

**Interfaces:**
- Consumes: Task 5/6 的 `ContainerSessionManager` / `DefaultContainerOpts` / `SandboxImageAvailable`
- Produces:
  - `const IsolationTmux = "tmux"`、`IsolationContainer = "container"`
  - `func ValidIsolation(s string) bool`
  - `func EffectiveIsolation(agent Agent, globalDefault string) string`
  - `type RoutingSessionManager struct{ Root string; Tmux, Container SessionManager; Default string }`（實作 `SessionManager`）
  - `func taskIsolation(root, session string) string`
  - `func BuildSessionManager(ctx context.Context, root string, cfg Config) (SessionManager, error)`
  - `Config.A2AIsolation() / A2ASandboxImage() / A2ASandboxNetwork() / A2AEgressProxy() / A2AOAuthTokenEnv()`

**一個被明確否決的做法，記錄下來免得日後有人「優化」進去：** 路由的依據**必須**是 `tasks.json` 的 `A2ATask.Isolation`，**不可以**放一個標記檔在 `<sandboxRoot>/` 裡。沙盒 root 是 **rw** 掛進容器的——一個沙盒把自己的標記從 `container` 改成 `tmux`，sweep 就會去呼叫 `tmux kill-session`（對容器是 no-op、回 nil = 「停掉了」），然後**在容器還活著的情況下刪掉它的 worktree**。`tasks.json` 從不掛進任何容器，是這個決定唯一安全的來源。

- [ ] **Step 1: 寫失敗的測試**

```go
package channelagent

import (
	"context"
	"testing"
)

func TestEffectiveIsolationPrefersAgentThenGlobalThenTmux(t *testing.T) {
	cases := []struct {
		agent  Agent
		global string
		want   string
	}{
		{Agent{}, "", IsolationTmux},                                       // 全空 → 今天的行為
		{Agent{}, IsolationContainer, IsolationContainer},                  // 全域
		{Agent{Isolation: IsolationContainer}, "", IsolationContainer},     // per-agent 覆寫全域
		{Agent{Isolation: IsolationTmux}, IsolationContainer, IsolationTmux}, // per-agent 也能往回蓋
		{Agent{Isolation: "garbage"}, IsolationContainer, IsolationContainer}, // 壞值一律忽略，不當成新模式
		{Agent{}, "garbage", IsolationTmux},
	}
	for i, c := range cases {
		if got := EffectiveIsolation(c.agent, c.global); got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

// recordingSM 記錄自己被呼叫了哪些方法，用來斷言路由選對了實作。
type recordingSM struct {
	name    string
	stopped *[]string
}

func (r recordingSM) EnsureWorkspace(context.Context, string, string, string) error { return nil }
func (r recordingSM) Start(context.Context, string, string, string) error           { return nil }
func (r recordingSM) Inject(context.Context, string, SourceMessage) error           { return nil }
func (r recordingSM) RemoveWorkspace(context.Context, string, string) error         { return nil }
func (r recordingSM) TrustFolder(context.Context, string) error                     { return nil }
func (r recordingSM) Stop(_ context.Context, s string) error {
	*r.stopped = append(*r.stopped, r.name+":"+s)
	return nil
}
func (r recordingSM) Alive(context.Context, string) (bool, error) { return false, nil }

func TestRoutingSessionManagerPicksByTaskRow(t *testing.T) {
	root := t.TempDir()
	var stopped []string
	rt := RoutingSessionManager{
		Root:      root,
		Tmux:      recordingSM{name: "tmux", stopped: &stopped},
		Container: recordingSM{name: "container", stopped: &stopped},
		Default:   IsolationTmux,
	}
	if err := SaveTasks(root, TaskStore{Tasks: []A2ATask{
		{ContextID: "c1", Session: "aa-a-c1", State: TaskWorking, Isolation: IsolationContainer},
		{ContextID: "c2", Session: "aa-a-c2", State: TaskWorking, Isolation: IsolationTmux},
		{ContextID: "c3", Session: "aa-a-c3", State: TaskWorking}, // 舊 row，沒有欄位
	}}); err != nil {
		t.Fatal(err)
	}
	_ = rt.Stop(context.Background(), "aa-a-c1")
	_ = rt.Stop(context.Background(), "aa-a-c2")
	if len(stopped) != 2 || stopped[0] != "container:aa-a-c1" || stopped[1] != "tmux:aa-a-c2" {
		t.Fatalf("routing = %v", stopped)
	}
}

// 認不出來的 session（row 已被 prune、或是舊 row 沒有欄位）不可以猜：
// 兩邊都停一次。tmux kill-session 對不存在的 session 是 nil，
// docker rm -f 對不存在的容器也是 nil，所以「都停」是安全且冪等的。
func TestRoutingSessionManagerStopsBothWhenIsolationIsUnknown(t *testing.T) {
	root := t.TempDir()
	var stopped []string
	rt := RoutingSessionManager{
		Root:      root,
		Tmux:      recordingSM{name: "tmux", stopped: &stopped},
		Container: recordingSM{name: "container", stopped: &stopped},
		Default:   IsolationTmux,
	}
	_ = SaveTasks(root, TaskStore{})
	if err := rt.Stop(context.Background(), "aa-gone-c9"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(stopped) != 2 {
		t.Fatalf("unknown isolation must stop both, got %v", stopped)
	}
}

func TestRoutingSessionManagerAliveIsTrueIfEitherIsAlive(t *testing.T) {
	root := t.TempDir()
	_ = SaveTasks(root, TaskStore{})
	rt := RoutingSessionManager{
		Root:      root,
		Tmux:      &FakeSessionManager{AliveSessions: map[string]bool{}},
		Container: &FakeSessionManager{AliveSessions: map[string]bool{"aa-x-1": true}},
		Default:   IsolationTmux,
	}
	ok, err := rt.Alive(context.Background(), "aa-x-1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want true/nil", ok, err)
	}
}

// 「問不到答案」必須往上傳，不可以被另一邊的 false 蓋掉 —— 否則 sweep 會
// 在一個其實還活著的沙盒上動手。
func TestRoutingSessionManagerAlivePropagatesHardErrors(t *testing.T) {
	root := t.TempDir()
	_ = SaveTasks(root, TaskStore{})
	rt := RoutingSessionManager{
		Root:      root,
		Tmux:      &FakeSessionManager{AliveSessions: map[string]bool{}},
		Container: &FakeSessionManager{FailOn: "alive"},
		Default:   IsolationTmux,
	}
	if _, err := rt.Alive(context.Background(), "aa-x-1"); err == nil {
		t.Fatal("a hard error from either side must surface")
	}
}

func TestA2AIsolationDefaultsToTmux(t *testing.T) {
	var c Config
	if got := c.A2AIsolation(); got != IsolationTmux {
		t.Fatalf("default isolation = %q, want tmux", got)
	}
	c.A2A.Isolation = "garbage"
	if got := c.A2AIsolation(); got != IsolationTmux {
		t.Fatalf("garbage isolation must fall back to tmux, got %q", got)
	}
	c.A2A.Isolation = IsolationContainer
	if got := c.A2AIsolation(); got != IsolationContainer {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestEffectiveIsolation|TestRoutingSessionManager|TestA2AIsolation' -race -v`
Expected: FAIL —— `undefined: EffectiveIsolation` 等

- [ ] **Step 3: 加設定與資料欄位**

`config.go` 的 `A2AConfig`：

```go
type A2AConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// CycleSeconds 是 A2A 生命週期迴圈的間隔；0 表示採用預設值（10 秒）。
	CycleSeconds int `json:"cycle_seconds,omitempty"`
	// Isolation 是沙盒的隔離模式："tmux"（預設，今天的行為）或 "container"。
	// 這是 a2a.enabled 之外的第二層 kill switch：改回 "tmux" 並重啟 serve
	// 就完全回到既有行為，已經在跑的容器由 ReapOrphanContainers 依 label 清掉。
	Isolation string `json:"isolation,omitempty"`
	// SandboxImage / SandboxNetwork / EgressProxy / OAuthTokenEnv 只在
	// container 模式下有意義；空值一律採用 a2a_container.go 的預設常數。
	SandboxImage   string `json:"sandbox_image,omitempty"`
	SandboxNetwork string `json:"sandbox_network,omitempty"`
	EgressProxy    string `json:"egress_proxy,omitempty"`
	OAuthTokenEnv  string `json:"oauth_token_env,omitempty"`
}

// A2AIsolation 回傳全域預設隔離模式。無法辨識的值一律當成 tmux —— 一個
// 打錯字的設定不該把系統推進一個沒人預期的模式。
func (c Config) A2AIsolation() string {
	if ValidIsolation(c.A2A.Isolation) {
		return c.A2A.Isolation
	}
	return IsolationTmux
}

func (c Config) A2ASandboxImage() string {
	if c.A2A.SandboxImage != "" {
		return c.A2A.SandboxImage
	}
	return DefaultSandboxImage
}

func (c Config) A2ASandboxNetwork() string {
	if c.A2A.SandboxNetwork != "" {
		return c.A2A.SandboxNetwork
	}
	return DefaultSandboxNetwork
}

func (c Config) A2AEgressProxy() string {
	if c.A2A.EgressProxy != "" {
		return c.A2A.EgressProxy
	}
	return DefaultEgressProxy
}

func (c Config) A2AOAuthTokenEnv() string {
	if c.A2A.OAuthTokenEnv != "" {
		return c.A2A.OAuthTokenEnv
	}
	return DefaultTokenEnvVar
}
```

`a2a_agents.go` 的 `Agent` 加兩個欄位：

```go
	// Isolation 覆寫全域的 a2a.isolation（空 = 跟全域）。讓 operator 可以
	// 先只把一個測試 agent 切成容器，其餘維持 tmux，同一個 serve 內並行。
	Isolation string `json:"isolation,omitempty"`
	// Image 覆寫沙盒映像（空 = 用全域設定）。共用映像不裝 python/node/go，
	// 需要跑 pytest / npm test / go test 的 agent 必須有自己的映像。
	Image string `json:"image,omitempty"`
```

`a2a_tasks.go` 的 `A2ATask` 加一個欄位：

```go
	// Isolation 記錄這一列的沙盒是用哪一種模式起的（"tmux" / "container"）。
	// 兩種模式並存期間，sweep 必須知道該 tmux kill-session 還是 docker rm -f
	// —— 這是共存唯一的真實成本。**只由 host 寫入、從不掛進任何容器**：
	// 用沙盒 root 裡的標記檔會讓沙盒能把自己改成另一種模式，騙 sweep 在容器
	// 還活著時刪掉它的 worktree。空字串代表升級前建立的舊 row。
	Isolation string `json:"isolation,omitempty"`
```

- [ ] **Step 4: 寫 `a2a_isolation.go`**

```go
package channelagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	IsolationTmux      = "tmux"
	IsolationContainer = "container"
)

func ValidIsolation(s string) bool { return s == IsolationTmux || s == IsolationContainer }

// EffectiveIsolation：per-agent 覆寫 > 全域預設 > tmux。任一層的值無法辨識
// 就當它不存在，往下一層取——絕不把打錯的字當成新模式。
func EffectiveIsolation(agent Agent, globalDefault string) string {
	if ValidIsolation(agent.Isolation) {
		return agent.Isolation
	}
	if ValidIsolation(globalDefault) {
		return globalDefault
	}
	return IsolationTmux
}

// taskIsolation 查這個 session 屬於哪一種隔離。唯一來源是 tasks.json
// （host-only，從不掛進任何容器）。查不到回空字串，呼叫端必須把空字串
// 當成「不知道」，不可以當成預設值。
func taskIsolation(root, session string) string {
	tasks, err := LoadTasks(root)
	if err != nil {
		return ""
	}
	for _, t := range tasks.Tasks {
		if t.Session == session && ValidIsolation(t.Isolation) {
			return t.Isolation
		}
	}
	return ""
}

// RoutingSessionManager 讓兩種沙盒在同一個 serve 內並存。它本身不做任何
// 副作用，只決定「這個 session 該交給哪一個實作」。
type RoutingSessionManager struct {
	Root      string
	Tmux      SessionManager
	Container SessionManager
	Default   string
}

var _ SessionManager = RoutingSessionManager{}

// pick 回傳這個 session 的實作，以及「有沒有真的查到」。
func (r RoutingSessionManager) pick(session string) (SessionManager, bool) {
	switch taskIsolation(r.Root, session) {
	case IsolationContainer:
		return r.Container, true
	case IsolationTmux:
		return r.Tmux, true
	}
	if r.Default == IsolationContainer {
		return r.Container, false
	}
	return r.Tmux, false
}

// EnsureWorkspace / RemoveWorkspace / Inject 在兩個實作裡逐字相同（都只碰
// host 的 git 與 host 的檔案），路由到哪一個都一樣，走預設即可。
func (r RoutingSessionManager) EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error {
	sm, _ := r.pick(filepath.Base(worktree))
	return sm.EnsureWorkspace(ctx, projectDir, branch, worktree)
}

func (r RoutingSessionManager) RemoveWorkspace(ctx context.Context, projectDir, worktree string) error {
	sm, _ := r.pick(filepath.Base(worktree))
	return sm.RemoveWorkspace(ctx, projectDir, worktree)
}

func (r RoutingSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	return TmuxSessionManager{}.Inject(ctx, root, msg)
}

func (r RoutingSessionManager) Start(ctx context.Context, session, cwd, registryRoot string) error {
	sm, _ := r.pick(session)
	return sm.Start(ctx, session, cwd, registryRoot)
}

func (r RoutingSessionManager) TrustFolder(ctx context.Context, worktree string) error {
	sm, _ := r.pick(filepath.Base(worktree))
	return sm.TrustFolder(ctx, worktree)
}

// Stop：查不到隔離模式時**兩邊都停一次**，不猜。兩邊的「本來就沒有」都是
// nil（StopTmuxSession 對不存在的 session、RemoveContainer 對不存在的容器），
// 所以這是安全且冪等的；猜錯的後果則是 sweep 在一個還活著的執行體上刪
// worktree。任一邊回錯就整體回錯 —— sweep 因此本輪什麼都不拆。
func (r RoutingSessionManager) Stop(ctx context.Context, session string) error {
	sm, known := r.pick(session)
	if known {
		return sm.Stop(ctx, session)
	}
	errT := r.Tmux.Stop(ctx, session)
	errC := r.Container.Stop(ctx, session)
	if errT != nil {
		return errT
	}
	return errC
}

// Alive：同樣的道理。任一邊說「活著」就是活著；任一邊「問不到答案」就必須
// 往上傳，不可以被另一邊的 false 蓋掉。
func (r RoutingSessionManager) Alive(ctx context.Context, session string) (bool, error) {
	sm, known := r.pick(session)
	if known {
		return sm.Alive(ctx, session)
	}
	okT, errT := r.Tmux.Alive(ctx, session)
	okC, errC := r.Container.Alive(ctx, session)
	if errT != nil {
		return false, errT
	}
	if errC != nil {
		return false, errC
	}
	return okT || okC, nil
}

// resolveHostBinary 解析 host 上某個執行檔的真實路徑（跟隨 symlink）。
// claude 與 claude-cron 都以 :ro 掛進容器，掛的必須是 symlink 解開之後的
// 實體檔案 —— 掛 symlink 進容器只會得到一條指向容器內不存在路徑的死鏈。
func resolveHostBinary(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", name, err)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	return real, nil
}

// BuildSessionManager 依設定組出 serve 要用的 SessionManager。
//
// 前置檢查的失敗策略是規格第十節寫明的：容器模式而映像或網路不存在時，
// **拒絕啟用整個 A2A**，不是靜默 fall back 到 tmux —— 後者會把系統退回一個
// 有已知提權洞（develop 可改寫自己政策檔）的模式，而 operator 以為自己
// 已經在容器裡了。
func BuildSessionManager(ctx context.Context, root string, cfg Config) (SessionManager, error) {
	global := cfg.A2AIsolation()
	needContainer := global == IsolationContainer
	if !needContainer {
		if agents, err := LoadAgents(root); err == nil {
			for _, a := range agents.Agents {
				if EffectiveIsolation(a, global) == IsolationContainer {
					needContainer = true
					break
				}
			}
		}
	}
	if !needContainer {
		return TmuxSessionManager{}, nil
	}

	opts := DefaultContainerOpts()
	opts.Image = cfg.A2ASandboxImage()
	opts.Network = cfg.A2ASandboxNetwork()
	opts.ProxyURL = cfg.A2AEgressProxy()
	opts.TokenEnvVar = cfg.A2AOAuthTokenEnv()
	var err error
	if opts.ClaudeBinary, err = resolveHostBinary("claude"); err != nil {
		return nil, err
	}
	if opts.CronBinary, err = resolveHostBinary("claude-cron"); err != nil {
		return nil, err
	}
	if os.Getenv(opts.TokenEnvVar) == "" {
		return nil, fmt.Errorf("container isolation needs %s in serve's environment (see .env)", opts.TokenEnvVar)
	}

	dr := execDockerRunner{}
	ok, err := SandboxImageAvailable(ctx, dr, opts.Image)
	if err != nil {
		return nil, fmt.Errorf("check sandbox image %s: %w", opts.Image, err)
	}
	if !ok {
		return nil, fmt.Errorf("sandbox image %s does not exist; run `make a2a-image`", opts.Image)
	}
	if _, err := dr.Run(ctx, nil, "network", "inspect", opts.Network); err != nil {
		return nil, fmt.Errorf("sandbox network %s is missing; run scripts/a2a-net-up.sh: %w", opts.Network, err)
	}

	return RoutingSessionManager{
		Root:      root,
		Tmux:      TmuxSessionManager{},
		Container: ContainerSessionManager{Root: root, Opts: opts, Docker: dr},
		Default:   global,
	}, nil
}
```

- [ ] **Step 5: 讓 dispatch 把有效隔離寫進 row**

`a2a_executor.go` 的 `Start`，在 `task.Worktree = SandboxWorktree(...)` 那一段旁邊（**必須在 `e.persist(task)` 之前**，否則崩潰恢復時 row 上沒有這個資訊）：

```go
	task.Worktree = SandboxWorktree(agent.ProjectDir, task.Session)
	task.Branch = BranchFor(task.Session)
	// 這一列用哪一種隔離，在派送當下就定案並寫進 row。之後 sweep 與
	// RoutingSessionManager 都以這個欄位為準：agents.json 之後被改成別的
	// 模式，不可以讓一個已經用容器起來的沙盒被當成 tmux session 去拆。
	task.Isolation = EffectiveIsolation(agent, e.DefaultIsolation)
	sandboxRoot := SandboxRoot(e.Root, task.Session)
```

`SandboxExecutor` 加一個欄位與建構參數：

```go
type SandboxExecutor struct {
	Root     string
	Sessions SessionManager
	// DefaultIsolation 是全域設定（cfg.A2AIsolation()）。空字串等同 "tmux"。
	DefaultIsolation string
}

func NewSandboxExecutor(root string, sm SessionManager) *SandboxExecutor {
	return &SandboxExecutor{Root: root, Sessions: sm, DefaultIsolation: IsolationTmux}
}

// NewSandboxExecutorWithIsolation 是 serve 用的建構子；既有的
// NewSandboxExecutor 維持原簽章，所有既有測試不受影響。
func NewSandboxExecutorWithIsolation(root string, sm SessionManager, defaultIsolation string) *SandboxExecutor {
	return &SandboxExecutor{Root: root, Sessions: sm, DefaultIsolation: defaultIsolation}
}
```

- [ ] **Step 6: 接進 `main.go`**

`cmd/claude-cron/main.go` 的 `if cfg.A2A.Enabled` 區塊（`:218`），在建 `A2AServer` **之前**先建 session manager；失敗就**整個 A2A 不啟用**：

```go
		if cfg.A2A.Enabled {
			sm, smErr := agent.BuildSessionManager(supCtx, *root, cfg)
			if smErr != nil {
				fmt.Fprintf(stderr, "a2a disabled: %v\n", smErr)
			} else {
				a2a := &agent.A2AServer{
					...
					Executor: agent.NewSandboxExecutorWithIsolation(*root, sm, cfg.A2AIsolation()),
					...
				}
				... // 原本的內容原封不動
			}
		}
```

`:302` 的 cycle 區塊同樣改用同一個 `sm`（**不要各自呼叫 `BuildSessionManager` 兩次** —— 那會做兩次前置檢查、也可能得到兩份不同的 opts）。把 `sm` 提到兩個區塊之外建立一次，兩處共用；`sm` 為 nil（建立失敗）時兩個區塊都不進去。

`agent.RunA2ACycleOnce(supCtx, *root, time.Now(), sm, ex, driver, cb, stdout)` —— 原本傳的 `agent.TmuxSessionManager{}` 換成 `sm`。

- [ ] **Step 7: 跑測試**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./internal/channelagent/ -run 'TestEffectiveIsolation|TestRouting|TestA2AIsolation|TestSandboxExecutor|TestDrainQueue|TestSweep' -race -v`
Expected: PASS

- [ ] **Step 8: 確認關掉時逐位元不變**

Run: `cd /home/conray/project/claude_cron && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok

然後**讀 diff 並逐行確認**：`main.go` 新增的每一行都在 `if cfg.A2A.Enabled` 之內；`cfg.A2A.Isolation` 為空（預設）時 `BuildSessionManager` 直接回 `TmuxSessionManager{}`，**一次 docker 呼叫都沒有**（`TestBuildSessionManagerDefaultMakesNoDockerCalls` 應該補上一條）。

- [ ] **Step 9: 【OP-7】operator 決定上線節奏**

**停下來等人。** 兩個選項：
- (a) 全域 `a2a.isolation = "container"`：一次全切。
- (b) 全域維持 `"tmux"`，只把一個測試 agent 的 `isolation` 設成 `"container"`（`claude-cron a2a agent update <name> --isolation=container`，Task 12 之後可用；在那之前只能透過 admin API `POST /api/a2a/agents/<name>/update`）。

**建議 (b)**：兩種模式並存本來就是本計畫的設計目標，先切一個 agent 讓 Task 13/14 的端到端有東西可跑，而其餘 agent 完全不受影響。

- [ ] **Step 10: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_isolation.go internal/channelagent/a2a_isolation_test.go \
        internal/channelagent/config.go internal/channelagent/a2a_agents.go \
        internal/channelagent/a2a_tasks.go internal/channelagent/a2a_executor.go \
        cmd/claude-cron/main.go
git commit -m "feat(a2a): per-task isolation routing so tmux and container sandboxes coexist"
```

---

### Task 8: gate spool —— 容器內的判定紀錄怎麼回到 operator 的 gate log

**Files:**
- Create: `internal/channelagent/a2a_gatespool.go`
- Create: `internal/channelagent/a2a_gatespool_test.go`
- Modify: `internal/channelagent/a2a_cycle.go`（新增一個階段）
- Modify: `internal/channelagent/a2a_lifecycle.go`（`removeCandidate` 拆除時清 spool）

**Interfaces:**
- Consumes: `GateSpoolDir` / `GateSpoolPath`（Task 5 已定義）、`GateLogPath`、`appendRotatingLine`、`GateLogEntry`
- Produces:
  - `func GateSpoolOffsetPath(root, session string) (string, error)`
  - `func DrainGateSpool(root string) (int, error)`
  - `func RemoveGateSpool(root, session string) error`
  - `const gateSpoolWarnBytes = 16 << 20`

**為什麼是「offset 前進」而不是「truncate」：** 規格第五節提的做法是「append 進正本，然後 truncate spool」。**truncate 有一個競態**：drain 讀完到 truncate 之間，容器內的 gate 可能又 append 了一行——那一行會被 truncate 吃掉，永久消失。而**改用 rename 換檔更糟**：單檔 bind mount 綁的是 inode，rename 會讓容器內繼續寫到一個 host 已經不看的舊 inode。

所以本計畫改成：**spool 只增不減，host 端記一個 offset（在 host-only 的 `.offset` 檔裡，不掛進容器）。** 沒有競態，沒有 inode 問題。代價與界限，明確寫出來：

- **spool 會長大。** 沙盒壽命上限是 `HardTimeout = 2h`；一行 gate log 約 200 B，即使每秒 10 次工具呼叫也只有約 14 MB，`AuditMaxBytes = 32 MiB` 打不到。`DrainGateSpool` 在超過 16 MiB 時記一行警告。
- **若真的打到 32 MiB**：容器內的 `appendRotatingLine` 會嘗試 rename 成 `<path>.1`，而容器內 `R/` 這個目錄是 docker 為了掛載點自動建的、沙盒的 uid 寫不進去 → rename 失敗 → `AppendGateLog` 回錯。**判定不受影響**：`runSandboxGate`（`a2a_gate.go:44-49`）明確在寫 log 失敗時「判定照舊生效，另寫一行 stderr」。所以最壞情況是**可見度損失，不是 DoS，也不是放行**。這一點必須寫進程式碼註解。
- **同一個沙盒仍可竄改自己那一個 cycle（10 秒）內的 spool 內容**，因為它是 rw 掛載。規格第五節第 3 點已判斷「先不做」sticky drop-box 強化，本計畫沿用。但比現況**更安全**：今天 `develop` 的 Bash 允許 `rm` 且完全不做路徑侷限（`a2a_gate.go:228-240` 的註解自己寫明），一個 `develop` 沙盒現在就能把整份 `a2a-gate.jsonl` 刪掉。

- [ ] **Step 1: 寫失敗的測試**

```go
package channelagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedSpool(t *testing.T, root, session string, entries ...GateLogEntry) string {
	t.Helper()
	p, err := GateSpoolPath(root, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		b, _ := json.Marshal(e)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestDrainGateSpoolMovesLinesIntoTheRealLog(t *testing.T) {
	root := t.TempDir()
	seedSpool(t, root, "aa-a-c1",
		GateLogEntry{Session: "aa-a-c1", Tool: "Bash", Outcome: "allowed"},
		GateLogEntry{Session: "aa-a-c1", Tool: "Write", Outcome: "denied_scope"})
	seedSpool(t, root, "aa-b-c2", GateLogEntry{Session: "aa-b-c2", Tool: "Read", Outcome: "allowed"})

	n, err := DrainGateSpool(root)
	if err != nil {
		t.Fatalf("DrainGateSpool: %v", err)
	}
	if n != 3 {
		t.Fatalf("drained %d lines, want 3", n)
	}
	got, err := ReadGateLog(root, "", 0)
	if err != nil {
		t.Fatalf("ReadGateLog: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("gate log has %d entries, want 3", len(got))
	}
	// 原內容必須原封不動地保留（GateLogEntry 本來就含 session，不需要重標）。
	if got[0].Session != "aa-a-c1" || got[0].Tool != "Bash" {
		t.Fatalf("entry mangled: %+v", got[0])
	}
}

func TestDrainGateSpoolIsIdempotentAndResumesFromOffset(t *testing.T) {
	root := t.TempDir()
	seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Bash"})
	if n, _ := DrainGateSpool(root); n != 1 {
		t.Fatalf("first drain n=%d", n)
	}
	// 沒有新內容 → 什麼都不該再搬一次
	if n, _ := DrainGateSpool(root); n != 0 {
		t.Fatalf("second drain must be a no-op, n=%d", n)
	}
	// 追加一行 → 只搬新的那一行
	seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Read"})
	if n, _ := DrainGateSpool(root); n != 1 {
		t.Fatalf("third drain n=%d, want 1", n)
	}
	got, _ := ReadGateLog(root, "", 0)
	if len(got) != 2 {
		t.Fatalf("gate log has %d entries, want 2 (no duplicates)", len(got))
	}
}

// spool 檔的 inode 絕不可以被 drain 換掉：單檔 bind mount 綁的是 inode，
// 換掉之後容器內的 gate 會繼續寫到一個 host 再也不看的檔案。
func TestDrainGateSpoolNeverReplacesTheSpoolInode(t *testing.T) {
	root := t.TempDir()
	p := seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Bash"})
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DrainGateSpool(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatalf("spool file must still exist after drain: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("spool inode changed; the container's bind mount would go stale")
	}
}

// 半行（容器正在寫的那一行）必須留到下一輪，不可以搬一半。
func TestDrainGateSpoolLeavesAPartialLine(t *testing.T) {
	root := t.TempDir()
	p := seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Bash"})
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"session":"aa-a-c1","tool":"Wr`)
	_ = f.Close()

	if n, _ := DrainGateSpool(root); n != 1 {
		t.Fatalf("drained %d, want 1 (the partial line must wait)", n)
	}
	// 補完那一行之後，下一輪才搬它
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("ite\"}\n")
	_ = f.Close()
	if n, _ := DrainGateSpool(root); n != 1 {
		t.Fatalf("second drain n=%d, want 1", n)
	}
}

// 沙盒把自己的 spool 截短（它是 rw 掛載，做得到）不可以讓 drain 亂搬。
func TestDrainGateSpoolRecoversFromTruncationByTheSandbox(t *testing.T) {
	root := t.TempDir()
	p := seedSpool(t, root, "aa-a-c1",
		GateLogEntry{Session: "aa-a-c1", Tool: "Bash"},
		GateLogEntry{Session: "aa-a-c1", Tool: "Read"})
	if _, err := DrainGateSpool(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Write"})
	n, err := DrainGateSpool(root)
	if err != nil {
		t.Fatalf("DrainGateSpool after truncation: %v", err)
	}
	if n != 1 {
		t.Fatalf("n=%d, want 1 (offset must reset, not skip)", n)
	}
}

func TestDrainGateSpoolIgnoresNonSandboxFiles(t *testing.T) {
	root := t.TempDir()
	dir := GateSpoolDir(root)
	_ = os.MkdirAll(dir, 0o700)
	// 非 aa- 檔名、以及 .offset 本身，都不得被當成 spool 處理。
	_ = os.WriteFile(filepath.Join(dir, "evil.jsonl"), []byte("{}\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "aa-a-c1.offset"), []byte("0"), 0o600)
	n, err := DrainGateSpool(root)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil", n, err)
	}
	if _, err := os.Stat(GateLogPath(root)); err == nil {
		t.Fatal("nothing should have been written to the gate log")
	}
}

func TestRemoveGateSpoolDrainsThenDeletes(t *testing.T) {
	root := t.TempDir()
	p := seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Bash"})
	if err := RemoveGateSpool(root, "aa-a-c1"); err != nil {
		t.Fatalf("RemoveGateSpool: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("spool file must be gone")
	}
	off, _ := GateSpoolOffsetPath(root, "aa-a-c1")
	if _, err := os.Stat(off); err == nil {
		t.Fatal("offset file must be gone")
	}
	// 拆除前的最後一行判定不可以遺失。
	got, _ := ReadGateLog(root, "aa-a-c1", 0)
	if len(got) != 1 {
		t.Fatalf("teardown must drain first, gate log has %d entries", len(got))
	}
	// 重複呼叫必須是 no-op（sweep 會重試）。
	if err := RemoveGateSpool(root, "aa-a-c1"); err != nil {
		t.Fatalf("second RemoveGateSpool must be nil, got %v", err)
	}
}

func TestA2ACycleDrainsGateSpoolBeforeCollectingResults(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	_ = SaveTasks(root, TaskStore{})
	seedSpool(t, root, "aa-a-c1", GateLogEntry{Session: "aa-a-c1", Tool: "Bash"})
	var buf strings.Builder
	RunA2ACycleOnce(t.Context(), root, timeNowForTest(), &FakeSessionManager{}, nil, nil, nil, &buf)
	got, _ := ReadGateLog(root, "", 0)
	if len(got) != 1 {
		t.Fatalf("cycle must drain the gate spool, gate log has %d entries", len(got))
	}
}
```

（`timeNowForTest()` 若既有測試檔沒有這個 helper，直接用 `time.Now()`。）

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestDrainGateSpool|TestRemoveGateSpool|TestA2ACycleDrains' -race -v`
Expected: FAIL —— `undefined: DrainGateSpool`

- [ ] **Step 3: 寫 `a2a_gatespool.go`**

```go
package channelagent

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// gateSpoolWarnBytes：spool 只增不減（見下），所以要有一個「異常成長」的
// 告警點。AuditMaxBytes（32 MiB）是容器內 appendRotatingLine 會嘗試輪替的
// 門檻，而那次輪替在容器內必定失敗（rename 的目標目錄是 docker 為掛載點
// 自動建的，沙盒的 uid 寫不進去）。失敗的後果只是「這個沙盒之後的判定不再
// 留下紀錄」——runSandboxGate（a2a_gate.go:44-49）明確在寫 log 失敗時讓
// 判定照舊生效，所以最壞情況是可見度損失，不是放行、也不是 DoS。
const gateSpoolWarnBytes = 16 << 20

func GateSpoolOffsetPath(root, session string) (string, error) {
	if !sandboxSessionRe.MatchString(session) {
		return "", fmt.Errorf("invalid sandbox session name %q", session)
	}
	return filepath.Join(GateSpoolDir(root), session+".offset"), nil
}

// DrainGateSpool 把每個沙盒 spool 裡「還沒搬過」的完整行 append 進正本
// gate log，並前進該 spool 的 offset。回傳這一趟搬了幾行。
//
// 為什麼是 offset 前進，不是 truncate、也不是 rename：
//   - truncate 有競態：讀完到 truncate 之間容器可能又寫了一行，那一行會被
//     吃掉，永久消失。
//   - rename 更糟：單檔 bind mount 綁的是 inode，rename 之後容器內的 gate
//     會繼續寫到一個 host 再也不看的舊 inode。
//   - offset 檔放在 spool 目錄裡，而**只有 <session>.jsonl 這一個檔案**掛進
//     容器（目錄本身沒有），所以沙盒碰不到自己的 offset。
//
// 錯誤策略：單一 spool 出錯只 log 並繼續處理其他 spool——一個壞掉的沙盒
// 不該讓其他沙盒的判定紀錄也回不來。
func DrainGateSpool(root string) (int, error) {
	entries, err := os.ReadDir(GateSpoolDir(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		session := strings.TrimSuffix(e.Name(), ".jsonl")
		if !sandboxSessionRe.MatchString(session) {
			continue // 不是這個系統建立的檔案，不碰
		}
		n, derr := drainOneGateSpool(root, session)
		total += n
		if derr != nil {
			log.Printf("a2a: drain gate spool %s: %v", session, derr)
		}
	}
	return total, nil
}

func drainOneGateSpool(root, session string) (int, error) {
	spool, err := GateSpoolPath(root, session)
	if err != nil {
		return 0, err
	}
	offPath, err := GateSpoolOffsetPath(root, session)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(spool)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if fi.Size() >= gateSpoolWarnBytes {
		log.Printf("a2a: 沙盒 %s 的 gate spool 已達 %d bytes；接近 AuditMaxBytes 之後容器內的輪替會失敗，之後的判定將不再留下紀錄（判定本身不受影響）", session, fi.Size())
	}

	off := readGateSpoolOffset(offPath)
	if off > fi.Size() {
		// 沙盒把自己的 spool 截短了（它是 rw 掛載，做得到）。從頭重讀是
		// 唯一不會漏行的選擇；重複的行由 offset 之後的推進避免。
		log.Printf("a2a: 沙盒 %s 的 gate spool 變短了（offset %d > size %d），從頭重讀", session, off, fi.Size())
		off = 0
	}
	if off == fi.Size() {
		return 0, nil
	}

	f, err := os.Open(spool)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, fi.Size()-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && n == 0 {
		return 0, err
	}
	buf = buf[:n]
	// 只吃到最後一個完整換行為止；半行留給下一輪。
	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		return 0, nil
	}
	region := buf[:last+1]

	count := 0
	for _, line := range bytes.Split(region, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// 原內容原封不動地 append：GateLogEntry 已含 session，不需要重標。
		if werr := appendRotatingLine(GateLogPath(root), append(line, '\n'), 0o600); werr != nil {
			// 寫不進正本就不推進 offset —— 下一輪整段重試，寧可重複也不遺失。
			return count, werr
		}
		count++
	}
	if err := writeGateSpoolOffset(offPath, off+int64(len(region))); err != nil {
		return count, err
	}
	return count, nil
}

func readGateSpoolOffset(path string) int64 {
	blob, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(blob)), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func writeGateSpoolOffset(path string, off int64) error {
	return AtomicWriteFile(path, []byte(strconv.FormatInt(off, 10)+"\n"), 0o600)
}

// RemoveGateSpool 在 sweep 拆除沙盒時呼叫：先 drain 一次（拆除前那一刻的
// 判定紀錄不可以遺失），再刪掉 spool 與 offset。找不到檔案視為已清乾淨。
func RemoveGateSpool(root, session string) error {
	if _, err := drainOneGateSpool(root, session); err != nil {
		log.Printf("a2a: 拆除 %s 前 drain gate spool 失敗（仍會刪除）: %v", session, err)
	}
	spool, err := GateSpoolPath(root, session)
	if err != nil {
		return err
	}
	offPath, err := GateSpoolOffsetPath(root, session)
	if err != nil {
		return err
	}
	for _, p := range []string{spool, offPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: 接進 cycle 與拆除路徑**

`a2a_cycle.go` 的 `RunA2ACycleOnce`，**排在 `CollectResults` 之前**（規格第五節）：

```go
	// gate spool 先 drain：容器內的 gate 寫的是自己那一份 spool，operator
	// 的正本 gate log 要在這一輪的其他階段（尤其是可能把沙盒拆掉的 sweep）
	// 之前就拿到內容。
	if _, err := DrainGateSpool(root); err != nil {
		fmt.Fprintf(stderr, "a2a gate spool: %v\n", err)
	}
	if _, err := CollectResults(root, now); err != nil {
```

`a2a_lifecycle.go` 的 `removeCandidate`（拆 worktree / sandbox root / 政策檔的那一段），在 `RemoveSandboxPolicy` 旁邊加：

```go
	if err := RemoveGateSpool(root, c.session); err != nil {
		log.Printf("a2a: sweep: 清除 %s 的 gate spool 失敗（下一趟重試）: %v", c.session, err)
	}
```

**清不掉只 log、不影響回收判定**——與既有的 `RemoveSandboxPolicy` 一致（下一趟會重試）。

- [ ] **Step 5: 跑測試 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_gatespool.go internal/channelagent/a2a_gatespool_test.go \
        internal/channelagent/a2a_cycle.go internal/channelagent/a2a_lifecycle.go
git commit -m "feat(a2a): gate spool so a container's decisions reach the operator's gate log"
```

---

### Task 9: transcript 路徑與活動鏡像在容器模式下的修復

**Files:**
- Modify: `internal/channelagent/activity.go`（新增 `CollectActivityIn` / `latestTranscriptIn`；`CollectActivity` 變成零值包裝）
- Modify: `internal/channelagent/a2a_driver.go`（傳 transcript 目錄；`capturePaneVia` / `dockerTmux` 接線）
- Modify: `internal/channelagent/activity_test.go`、`a2a_driver_test.go`

**Interfaces:**
- Produces:
  - `func sandboxTranscriptDir(sandboxRoot, worktree string) string`
  - `func latestTranscriptIn(dir string) string`
  - `func CollectActivityIn(bRoot, worktree, transcriptDir string) []string`
  - `func sandboxPaneDriver(task A2ATask) paneDriver`

**為什麼一定會壞（規格第八節）：** `CollectActivity` tail 的是 Claude 的 transcript JSONL，路徑由 `sessionTranscriptPath(bRoot, worktree)`（`session_hook.go:51`）決定：先看 `<bRoot>/state/session.json`（SessionStart hook 記的），沒有就退回 `transcriptPath(worktree)` = `os.UserHomeDir()/.claude/projects/<encodeProjectDir(worktree)>/<id>.jsonl`。**沙盒的 `sandboxAgentSettings`（`worktree.go:184`）刻意拿掉了 SessionStart hook，所以一定走退回路徑；而容器內 `CLAUDE_CONFIG_DIR` 已經指到 `<sandboxRoot>/claude`，transcript 根本不在 host 的 `~/.claude/projects/`。** 活動鏡像會全面失效——agent 頻道從此只剩 driver 自己的 🟢/🔴 兩行。

**同時記錄一個行為變化：** `latestTranscript`（`worktree.go:444`）也用 `os.UserHomeDir()`，它服務的是 `claudeArgs` 的 `--resume`。容器模式下它對沙盒 worktree 一律回空字串 → **沙盒永遠是全新 session、不 `--resume`**。這是可接受的（沙盒本來就是一次性），但容器若被重建（不在正常流程內），對話歷史不會續上。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestSandboxTranscriptDirIsUnderTheSandboxConfigDir(t *testing.T) {
	got := sandboxTranscriptDir("/r/sandboxes/aa-a-c1", "/proj/aa-a-c1")
	want := filepath.Join("/r/sandboxes/aa-a-c1", "claude", "projects", encodeProjectDir("/proj/aa-a-c1"))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCollectActivityInReadsFromTheOverrideDirectory(t *testing.T) {
	bRoot := t.TempDir()
	if err := Init(bRoot); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(bRoot, "claude", "projects", "encoded")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tp := filepath.Join(dir, "sess.jsonl")
	// 第一次呼叫只是「認識」這個 transcript 並 seek 到結尾，不回放歷史。
	if err := os.WriteFile(tp, []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"old"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lines := CollectActivityIn(bRoot, "/proj/aa-a-c1", dir); len(lines) != 0 {
		t.Fatalf("first sight must not replay backlog, got %v", lines)
	}
	f, _ := os.OpenFile(tp, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"新的想法"}]}}` + "\n")
	_ = f.Close()

	lines := CollectActivityIn(bRoot, "/proj/aa-a-c1", dir)
	if len(lines) != 1 || !strings.Contains(lines[0], "新的想法") {
		t.Fatalf("lines = %v, want the appended thinking line", lines)
	}
}

// cc- 呼叫端一個字都不改：CollectActivity 必須與加這個參數之前完全相同。
func TestCollectActivityEqualsCollectActivityInWithEmptyOverride(t *testing.T) {
	bRoot := t.TempDir()
	_ = Init(bRoot)
	if got := CollectActivity(bRoot, "/nonexistent"); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if got := CollectActivityIn(bRoot, "/nonexistent", ""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestSandboxPaneDriverOnlyForContainerTasks(t *testing.T) {
	if p := sandboxPaneDriver(A2ATask{Session: "aa-a-c1", Isolation: IsolationTmux}); p != nil {
		t.Fatalf("tmux task must get a nil paneDriver, got %#v", p)
	}
	if p := sandboxPaneDriver(A2ATask{Session: "aa-a-c1"}); p != nil {
		t.Fatalf("legacy row without isolation must get nil, got %#v", p)
	}
	p := sandboxPaneDriver(A2ATask{Session: "aa-a-c1", Isolation: IsolationContainer})
	d, ok := p.(dockerTmux)
	if !ok || d.container != "cc-a2a-aa-a-c1" {
		t.Fatalf("container task must get dockerTmux{cc-a2a-aa-a-c1}, got %#v", p)
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestSandboxTranscriptDir|TestCollectActivityIn|TestCollectActivityEquals|TestSandboxPaneDriver' -race -v`
Expected: FAIL —— `undefined: sandboxTranscriptDir` 等

- [ ] **Step 3: 改 `activity.go`**

```go
// sandboxTranscriptDir 回傳容器沙盒的 transcript 所在目錄。容器內
// CLAUDE_CONFIG_DIR 指向 <sandboxRoot>/claude，不是 operator 的 ~/.claude，
// 所以 transcript 不會出現在 host 的 ~/.claude/projects/ 底下 —— 沒有這個
// 覆寫，agent 頻道的活動鏡像會全面失效。
func sandboxTranscriptDir(sandboxRoot, worktree string) string {
	return filepath.Join(sandboxRoot, "claude", "projects", encodeProjectDir(cleanAbs(worktree)))
}

// latestTranscriptIn 回傳 dir 底下最新的 *.jsonl 完整路徑，沒有就回空字串。
// 與 latestTranscript 的差別只有「目錄從哪裡來」：後者寫死 os.UserHomeDir()。
func latestTranscriptIn(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestT time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestT) {
			newest, newestT = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	return newest
}

// CollectActivity 的行為與這個覆寫參數存在之前逐字相同（cc- 的每一個呼叫
// 端都走這條，一個字都不用改）。
func CollectActivity(bRoot, worktree string) []string {
	return CollectActivityIn(bRoot, worktree, "")
}

// CollectActivityIn 與 CollectActivity 相同，但可以指定 transcript 目錄。
// transcriptDir == "" 時走原本的解析路徑（SessionStart hook 記的路徑，
// 或 host 家目錄底下的猜測）。
func CollectActivityIn(bRoot, worktree, transcriptDir string) []string {
	tp := ""
	if transcriptDir != "" {
		tp = latestTranscriptIn(transcriptDir)
	} else {
		tp = sessionTranscriptPath(bRoot, worktree)
	}
	if tp == "" {
		return nil
	}
	// …（以下與既有 CollectActivity 從 statePath 開始的內容逐字相同）
}
```

**做法上的要求：** 把既有 `CollectActivity` 的函式體整段搬進 `CollectActivityIn`，只換掉最前面決定 `tp` 的那三行。不要複製一份——那正是兩份邏輯遲早分岔的起點。

- [ ] **Step 4: 改 `a2a_driver.go`**

新增：

```go
// sandboxPaneDriver 回傳這個任務的 tmux 執行位置。tmux 模式（以及升級前
// 建立、沒有這個欄位的舊 row）一律 nil = host tmux，行為與這個函式存在
// 之前完全相同。
func sandboxPaneDriver(task A2ATask) paneDriver {
	if task.Isolation != IsolationContainer {
		return nil
	}
	name, err := ContainerName(task.Session)
	if err != nil {
		return nil
	}
	return dockerTmux{container: name}
}
```

`loop` 內三處接線：

1. 每拍的單次擷取：`pane := capturePane(ctx, session)` → `pane := capturePaneVia(ctx, pd, session)`（`pd := sandboxPaneDriver(task)` 在 loop 開頭算一次）。
2. 兩個閘的自動回答：`TmuxInjector{Session: session}.SelectTrustSettings(ctx)` → `TmuxInjector{Session: session, Pane: pd}.SelectTrustSettings(ctx)`；`PressEnter` 同樣。
3. 活動鏡像：

```go
		for _, line := range CollectActivityIn(sandbox, task.Worktree, transcriptDirFor(task, sandbox)) {
			channel.SendLine(task.ContextID, line)
		}
```

搭配：

```go
// transcriptDirFor：容器沙盒的 transcript 在自己的 CLAUDE_CONFIG_DIR 底下；
// tmux 沙盒維持原本的解析路徑（空字串）。
func transcriptDirFor(task A2ATask, sandboxRoot string) string {
	if task.Isolation != IsolationContainer {
		return ""
	}
	return sandboxTranscriptDir(sandboxRoot, task.Worktree)
}
```

`autoAnswerSandboxConfirm` 也要能把選擇送進容器：加一個 `paneDriver` 參數，內部把 `sendConfirmChoice` 換成 `sendConfirmChoiceVia`。呼叫端 `autoAnswerSandboxConfirm(ctx, pd, session, pane, lastAnsweredHash)`。

`EnsureSandboxDrivers`（`a2a_driver.go:694`）建 injector 的那一行加上 `Pane`：

```go
		d.Ensure(ctx, t, TmuxInjector{
			Session: t.Session,
			Root:    SandboxRoot(root, t.Session),
			Pane:    sandboxPaneDriver(t),
		})
```

`d.alive` 的預設值（`NewSandboxDriver` 裡的 `alive: TmuxSessionAlive`）**不動** —— driver 的存活檢查只是「連續 3 次失敗之後停止驅動」的省電機制，真正的判定在 sweep（唯一該改任務狀態的地方）。但要補一行註解說明：容器模式下 `TmuxSessionAlive` 對 `aa-` session 一律回 `(false, nil)`，於是 driver 會更早停止驅動一個已經失敗的容器沙盒——方向正確（提早退出），不會造成誤判成 failed。

- [ ] **Step 5: 跑測試 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok。特別確認 `activity_test.go` 與 `a2a_driver_test.go` 的既有測試全綠。

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/activity.go internal/channelagent/activity_test.go \
        internal/channelagent/a2a_driver.go internal/channelagent/a2a_driver_test.go
git commit -m "fix(a2a): stream a container sandbox's transcript to its agent channel"
```

---

### Task 10: `ReapOrphanContainers` —— 孤兒容器回收，且不繞過單一拆除路徑

**Files:**
- Create: `internal/channelagent/a2a_reap_container.go`
- Create: `internal/channelagent/a2a_reap_container_test.go`
- Modify: `internal/channelagent/a2a_cycle.go`

（檔名刻意不叫 `a2a_reap.go`：`reap.go` 已經存在且是 cc- 專用，兩者放在一起會讓人誤以為有關。）

**Interfaces:**
- Consumes: `tryLockSandboxSessionForTeardown`（`a2a_sessionlock.go:165`）、`LoadTasks`、`RemoveContainer`、`dockerRunner`
- Produces:
  - `const OrphanGrace = 5 * time.Minute`
  - `func ReapOrphanContainers(ctx context.Context, root string, dr dockerRunner, now time.Time) (int, error)`
  - `type orphanReaper interface{ ReapOrphans(ctx context.Context, now time.Time) (int, error) }`
  - `ContainerSessionManager.ReapOrphans` / `RoutingSessionManager.ReapOrphans`

**這是本計畫唯一新增的破壞性路徑，因此三條約束必須同時成立：**

1. **走同一把拆除鎖。** `tryLockSandboxSessionForTeardown` 拿不到就跳過本輪（`TryLock`-and-skip），與 `SweepTimeouts` 完全相同的形狀。沒有這條，reap 會在 `SandboxExecutor.Start` 正在建立同名沙盒的中途把容器砍掉。
2. **拿到鎖之後重新確認一次。** 判定「是孤兒」與「動手刪」之間有窗口，必須在鎖內重讀 `tasks.json` 再確認——這正是 `candidateStillMatches` 存在的理由，同一個道理。
3. **grace window。** `cc.a2a.started` 距今 < `OrphanGrace` 的一律跳過。理由與 `DispatchStaleAfter` 相同：`Start` 在容器起來之後、`persist(TaskWorking)` 之前有一段窗口，這段時間內容器**合法地**沒有對應的 working row。

**這一步同時是「serve 重啟後」的收斂機制**：容器是 host 上的持久物件，serve 掛掉不會帶走它們；重啟後這一掃就把不再有主的容器清乾淨。今天的 tmux session 沒有這個性質（`EnsureSandboxDrivers` 只停 goroutine，不停 tmux）。

**失敗沙盒的容器一律刪除，不保留。** forensics 規則不受影響——worktree 是 host bind mount，容器刪掉之後它原封不動地留在磁碟上（`MaxRetainedFailedSandboxes = 20` 照舊）。保留一個死掉的容器只是佔記憶體，證據本來就在 worktree 裡。

- [ ] **Step 1: 寫失敗的測試**

```go
package channelagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func psOutput(rows ...string) string { return strings.Join(rows, "\n") + "\n" }

func row(name, session, started string) string {
	return name + "\t" + session + "\t" + started
}

func TestReapOrphanContainersRemovesContainersWithNoLiveRow(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour).Format(time.RFC3339)
	_ = SaveTasks(root, TaskStore{Tasks: []A2ATask{
		{ContextID: "c1", Session: "aa-a-c1", State: TaskWorking, Isolation: IsolationContainer},
		{ContextID: "c2", Session: "aa-a-c2", State: TaskCompleted, Isolation: IsolationContainer},
	}})
	dr := &fakeDocker{
		out: []string{psOutput(
			row("cc-a2a-aa-a-c1", "aa-a-c1", old), // 還在跑 → 留
			row("cc-a2a-aa-a-c2", "aa-a-c2", old), // 已完成 → 收
			row("cc-a2a-aa-a-c9", "aa-a-c9", old), // 根本沒有 row → 收
		), "", ""},
		errs: []error{nil, nil, nil},
	}
	n, err := ReapOrphanContainers(context.Background(), root, dr, now)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped %d, want 2", n)
	}
	var removed []string
	for _, c := range dr.calls {
		if c[0] == "rm" {
			removed = append(removed, c[2])
		}
	}
	if len(removed) != 2 || removed[0] != "cc-a2a-aa-a-c2" || removed[1] != "cc-a2a-aa-a-c9" {
		t.Fatalf("removed = %v", removed)
	}
}

func TestReapOrphanContainersHonoursTheGraceWindow(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	_ = SaveTasks(root, TaskStore{})
	dr := &fakeDocker{out: []string{psOutput(row("cc-a2a-aa-a-c1", "aa-a-c1", fresh))}, errs: []error{nil}}
	n, err := ReapOrphanContainers(context.Background(), root, dr, now)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil — a container younger than OrphanGrace is legitimately row-less", n, err)
	}
	for _, c := range dr.calls {
		if c[0] == "rm" {
			t.Fatalf("must not remove within the grace window: %v", c)
		}
	}
}

// started label 壞掉/缺席 → 保守地當成「還在 grace 內」，不刪。
func TestReapOrphanContainersSkipsUnparseableStartedLabel(t *testing.T) {
	root := t.TempDir()
	_ = SaveTasks(root, TaskStore{})
	dr := &fakeDocker{out: []string{psOutput(row("cc-a2a-aa-a-c1", "aa-a-c1", "not-a-time"))}, errs: []error{nil}}
	n, _ := ReapOrphanContainers(context.Background(), root, dr, time.Now())
	if n != 0 {
		t.Fatalf("n=%d, want 0 (an unreadable label must not authorise a delete)", n)
	}
}

// 拆除鎖忙 = 有人正在建立或投遞這個 session，本輪跳過。
func TestReapOrphanContainersSkipsLockedSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-time.Hour).Format(time.RFC3339)
	_ = SaveTasks(root, TaskStore{})
	unlock := lockSandboxSession("aa-a-c1")
	defer unlock()
	dr := &fakeDocker{out: []string{psOutput(row("cc-a2a-aa-a-c1", "aa-a-c1", old))}, errs: []error{nil}}
	n, err := ReapOrphanContainers(context.Background(), root, dr, now)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil while the session lock is held", n, err)
	}
}

// docker ps 本身失敗 = 問不到答案 → 回錯，什麼都不刪。
func TestReapOrphanContainersFailsClosedWhenListingFails(t *testing.T) {
	root := t.TempDir()
	_ = SaveTasks(root, TaskStore{})
	dr := &fakeDocker{errs: []error{exitErr("Cannot connect to the Docker daemon")}}
	if _, err := ReapOrphanContainers(context.Background(), root, dr, time.Now()); err == nil {
		t.Fatal("a failed listing must be an error, not an empty orphan set")
	}
	for _, c := range dr.calls {
		if c[0] == "rm" {
			t.Fatalf("must not remove anything: %v", c)
		}
	}
}

// tasks.json 讀不出來時絕不可以把所有容器都當成孤兒。
func TestReapOrphanContainersRefusesWhenTasksAreUnreadable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(TasksPath(root), []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Format(time.RFC3339)
	dr := &fakeDocker{out: []string{psOutput(row("cc-a2a-aa-a-c1", "aa-a-c1", old))}, errs: []error{nil}}
	if _, err := ReapOrphanContainers(context.Background(), root, dr, time.Now()); err == nil {
		t.Fatal("unreadable tasks.json must abort the reap, not authorise mass deletion")
	}
}
```

（`TasksPath` 若不存在就用 `filepath.Join(root, "tasks.json")`。）

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestReapOrphanContainers -race -v`
Expected: FAIL —— `undefined: ReapOrphanContainers`

- [ ] **Step 3: 寫實作**

```go
package channelagent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// OrphanGrace：cc.a2a.started 距今小於這個時間的容器一律跳過。理由與
// DispatchStaleAfter 相同 —— SandboxExecutor.Start 在容器起來之後、
// persist(TaskWorking) 之前有一段窗口，這段時間內容器「合法地」沒有對應
// 的 working row。
const OrphanGrace = 5 * time.Minute

// orphanReaper 是可選能力：只有容器模式的 SessionManager 提供。cycle 用
// 型別斷言取用，於是 RunA2ACycleOnce 的簽章一個字都不用改，tmux 模式也
// 完全不會多跑任何 docker 指令。
type orphanReaper interface {
	ReapOrphans(ctx context.Context, now time.Time) (int, error)
}

func (m ContainerSessionManager) ReapOrphans(ctx context.Context, now time.Time) (int, error) {
	return ReapOrphanContainers(ctx, m.Root, m.Docker, now)
}

func (r RoutingSessionManager) ReapOrphans(ctx context.Context, now time.Time) (int, error) {
	if rp, ok := r.Container.(orphanReaper); ok {
		return rp.ReapOrphans(ctx, now)
	}
	return 0, nil
}

// ReapOrphanContainers 刪掉「本系統起的、但已經沒有任何非終止 row 認領」
// 的容器。這是 serve 重啟後的收斂機制：容器是 host 上的持久物件，serve
// 掛掉不會帶走它們。
//
// 三條約束，缺一不可：
//  1. 走 tryLockSandboxSessionForTeardown（TryLock-and-skip），與 sweep
//     同一把鎖 —— 這是全系統唯一的破壞性拆除路徑，不得繞過。
//  2. 拿到鎖之後**重新確認一次**（重讀 tasks.json）：判定與動手之間的窗口
//     裡，同一個 session 可能已經被合法地重新建起來。
//  3. grace window（見 OrphanGrace）。
//
// 任何「問不到答案」（docker ps 失敗、tasks.json 讀不出來、started label
// 看不懂）一律不刪 —— fail closed 的方向是「留著」，不是「刪掉」。
func ReapOrphanContainers(ctx context.Context, root string, dr dockerRunner, now time.Time) (int, error) {
	out, err := dr.Run(ctx, nil,
		"ps", "-a", "--filter", "label=cc.a2a.session",
		"--format", `{{.Names}}	{{.Label "cc.a2a.session"}}	{{.Label "cc.a2a.started"}}`)
	if err != nil {
		return 0, fmt.Errorf("list a2a containers: %w", err)
	}
	type candidate struct{ name, session string }
	var cands []candidate
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 || f[0] == "" || f[1] == "" {
			continue
		}
		name, session, started := f[0], f[1], f[2]
		if !sandboxSessionRe.MatchString(session) {
			continue // 不是本系統的命名，不碰
		}
		st, ok := parseRFC3339(started)
		if !ok {
			// 看不懂的 label 不構成刪除授權。
			log.Printf("a2a: reap: 容器 %s 的 cc.a2a.started 讀不懂（%q），本輪跳過", name, started)
			continue
		}
		if now.Sub(st) < OrphanGrace {
			continue
		}
		cands = append(cands, candidate{name: name, session: session})
	}
	if len(cands) == 0 {
		return 0, nil
	}
	// tasks.json 讀不出來時**什麼都不刪**：把讀取失敗解讀成「沒有任何 row」
	// 等於一次把所有活著的沙盒全殺掉。
	if _, err := LoadTasks(root); err != nil {
		return 0, fmt.Errorf("reap: load tasks: %w", err)
	}

	reaped := 0
	for _, c := range cands {
		unlock, ok := tryLockSandboxSessionForTeardown(c.session)
		if !ok {
			log.Printf("a2a: reap: session %s 正在使用中，本輪跳過", c.session)
			continue
		}
		// 拿到鎖之後重新確認：判定與動手之間，同一個 contextId 可能已被
		// 合法重送、沙盒重新建起來。
		still, err := containerIsOrphan(root, c.session)
		if err != nil {
			unlock()
			log.Printf("a2a: reap: 重新確認 %s 失敗，本輪跳過: %v", c.session, err)
			continue
		}
		if !still {
			unlock()
			continue
		}
		if err := RemoveContainer(ctx, dr, c.name); err != nil {
			unlock()
			log.Printf("a2a: reap: 刪除孤兒容器 %s 失敗（下一輪重試）: %v", c.name, err)
			continue
		}
		unlock()
		reaped++
		log.Printf("a2a: reap: 已刪除孤兒容器 %s（session %s 沒有非終止的任務列）", c.name, c.session)
	}
	return reaped, nil
}

// containerIsOrphan：這個 session 有沒有一列非終止狀態的任務。讀不出來就
// 回 error（呼叫端因此跳過），絕不回 true。
func containerIsOrphan(root, session string) (bool, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return false, err
	}
	for _, t := range tasks.Tasks {
		if t.Session != session {
			continue
		}
		if !isTerminal(t.State) {
			return false, nil
		}
	}
	return true, nil
}
```

（`isTerminal` 是 `a2a_executor.go:68` 既有的未匯出函式，同一個 package 直接用即可；`TasksPath` 是 `a2a_tasks.go:127`。）

- [ ] **Step 4: 接進 cycle（排在 `SweepTimeouts` 之後）**

`a2a_cycle.go`：

```go
	if _, _, err := SweepTimeouts(ctx, root, sm, now, driver); err != nil {
		fmt.Fprintf(stderr, "a2a sweep: %v\n", err)
	}
	// 孤兒容器回收：只有容器模式的 SessionManager 實作 orphanReaper，
	// tmux 模式在這裡一次 docker 都不會呼叫。排在 sweep 之後 —— sweep
	// 剛剛可能才把一批 row 轉成終止狀態，它們的容器這一輪就能收掉。
	if rp, ok := sm.(orphanReaper); ok {
		if _, err := rp.ReapOrphans(ctx, now); err != nil {
			fmt.Fprintf(stderr, "a2a reap: %v\n", err)
		}
	}
```

在 `a2a_cycle_test.go` 的 `TestA2ACycleRunsAllStages` 補一條：傳入一個實作 `orphanReaper` 的 fake，斷言它被呼叫；以及傳入 `&FakeSessionManager{}`（**不**實作 `orphanReaper`）時完全不被呼叫。

- [ ] **Step 5: 跑測試 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_reap_container.go internal/channelagent/a2a_reap_container_test.go \
        internal/channelagent/a2a_cycle.go internal/channelagent/a2a_cycle_test.go \
        internal/channelagent/a2a_container.go internal/channelagent/a2a_isolation.go
git commit -m "feat(a2a): reap orphan sandbox containers through the one guarded teardown path"
```

---

### Task 11: 容器模式下的併發上限（8 → 4）與記憶體實測

**Files:**
- Modify: `internal/channelagent/a2a_lifecycle.go`（`MaxConcurrentSandboxes` 變成可設定的有效值）
- Modify: `internal/channelagent/a2a_lifecycle_test.go`
- Modify: `cmd/claude-cron/main.go`（容器模式時設定上限）

**Interfaces:**
- Produces:
  - `const MaxConcurrentContainerSandboxes = 4`
  - `func SetMaxConcurrentSandboxes(n int)`
  - `func EffectiveMaxSandboxes() int`
- 既有 `const MaxConcurrentSandboxes = 8` **保留為預設值**，`HasCapacity`（`:34`）與 `DrainQueue` 的 `free :=`（`:67`）改讀 `EffectiveMaxSandboxes()`。

**為什麼是 4：** 可用記憶體 5.2 GiB，單一 `claude` 行程實測 RSS 430–765 MB。`--memory=2g` × 8 = 16 GiB，遠超可用量。容器至少讓超量的沙盒被 OOM killer **針對性**殺掉，而不是像 2026-07-29 那次一樣讓整台機器 OOM 重開——但那是最後一道防線，不是可以依賴的容量規劃。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestEffectiveMaxSandboxesDefaultsToEight(t *testing.T) {
	if got := EffectiveMaxSandboxes(); got != MaxConcurrentSandboxes {
		t.Fatalf("default = %d, want %d", got, MaxConcurrentSandboxes)
	}
}

func TestSetMaxConcurrentSandboxesBoundsCapacity(t *testing.T) {
	orig := EffectiveMaxSandboxes()
	defer SetMaxConcurrentSandboxes(orig)

	SetMaxConcurrentSandboxes(MaxConcurrentContainerSandboxes)
	if got := EffectiveMaxSandboxes(); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
	var s TaskStore
	for i := 0; i < 4; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), Session: "aa-x", State: TaskWorking})
	}
	if HasCapacity(s) {
		t.Fatal("4 running sandboxes must exhaust a cap of 4")
	}
	SetMaxConcurrentSandboxes(8)
	if !HasCapacity(s) {
		t.Fatal("raising the cap must free capacity again")
	}
}

// 非法值一律忽略：0 或負數會讓系統再也派不出任何任務。
func TestSetMaxConcurrentSandboxesIgnoresNonsense(t *testing.T) {
	orig := EffectiveMaxSandboxes()
	defer SetMaxConcurrentSandboxes(orig)
	SetMaxConcurrentSandboxes(4)
	for _, n := range []int{0, -1, -100} {
		SetMaxConcurrentSandboxes(n)
		if got := EffectiveMaxSandboxes(); got != 4 {
			t.Fatalf("SetMaxConcurrentSandboxes(%d) changed the cap to %d", n, got)
		}
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestEffectiveMaxSandboxes|TestSetMaxConcurrentSandboxes' -race -v`
Expected: FAIL

- [ ] **Step 3: 寫實作**

`a2a_lifecycle.go`：

```go
// MaxConcurrentContainerSandboxes 是容器模式的上限。可用記憶體只有 5.2
// GiB，單一 claude 行程實測 RSS 430–765 MB，而每個容器 --memory=2g；
// 8 × 2 GiB 遠超可用量。這個值只往下調，不往上：容器的 OOM 上限是最後
// 一道防線，不是容量規劃。
const MaxConcurrentContainerSandboxes = 4

// maxConcurrentSandboxes 是「目前生效」的上限。用 atomic 而不是普通變數：
// serve 啟動時設定一次，之後由 handler goroutine 與 cycle goroutine 併發
// 讀取；測試也會臨時調整它。
var maxConcurrentSandboxes atomic.Int64

func init() { maxConcurrentSandboxes.Store(MaxConcurrentSandboxes) }

// SetMaxConcurrentSandboxes 設定生效上限。n <= 0 一律忽略 —— 一個 0 會讓
// 系統再也派不出任何任務，而那是一個看起來像「沒事」的完全停擺。
func SetMaxConcurrentSandboxes(n int) {
	if n <= 0 {
		return
	}
	maxConcurrentSandboxes.Store(int64(n))
}

func EffectiveMaxSandboxes() int { return int(maxConcurrentSandboxes.Load()) }

func HasCapacity(s TaskStore) bool {
	return s.RunningCount() < EffectiveMaxSandboxes()
}
```

`DrainQueue` 裡的 `free := MaxConcurrentSandboxes - tasks.RunningCount()` 改成 `free := EffectiveMaxSandboxes() - tasks.RunningCount()`。

`cmd/claude-cron/main.go`，在 `BuildSessionManager` 成功之後：

```go
			if cfg.A2AIsolation() == agent.IsolationContainer {
				// 容器模式的記憶體上限：見 MaxConcurrentContainerSandboxes。
				agent.SetMaxConcurrentSandboxes(agent.MaxConcurrentContainerSandboxes)
				fmt.Fprintf(stdout, "a2a: container isolation, max concurrent sandboxes = %d\n", agent.EffectiveMaxSandboxes())
			}
```

**注意 per-agent 覆寫的邊界情形：** 全域是 `tmux`、只有一個 agent 切成 `container` 時，上限維持 8。這是刻意的——那種情況下最多只有少數容器，其餘是 tmux 沙盒（沒有 `--memory` 硬上限，但記憶體用量與今天相同）。這一點寫進 `main.go` 的註解。

- [ ] **Step 4: 跑測試**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok。既有的容量測試（`a2a_concurrency_round14_test.go`、`a2a_lifecycle_test.go`）全綠——它們用的是預設 8。

- [ ] **Step 5: 【OP】記憶體實測**

**停下來等人執行。** 在 Task 13 的端到端跑起來之後（或先手動起 4 個容器），量：

```bash
free -h
docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}\t{{.MemPerc}}' | grep cc-a2a
ps -o rss=,comm= -C claude | sort -rn | head
```

判讀標準：
- 4 個容器同時跑時，`free -h` 的 available **必須 > 1 GiB**，否則 cc- 的 47 個 binding 會開始被擠。
- 任何一個容器逼近 2 GiB 上限（`MemPerc` > 90%）要記錄下來——那代表 `--memory=2g` 可能太緊，`claude` 會被 OOM killer 殺掉而看起來像「沙盒莫名死掉」。
- 若 available 撐不住 4 個，**允許再往下調到 3 或 2**（改 `MaxConcurrentContainerSandboxes`），**不允許往上調**。

- [ ] **Step 6: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_lifecycle.go internal/channelagent/a2a_lifecycle_test.go cmd/claude-cron/main.go
git commit -m "feat(a2a): lower the sandbox concurrency cap under container isolation"
```

---

### Task 12: CLI、admin API 與 admin UI 的隔離模式欄位

**Files:**
- Modify: `internal/channelagent/a2a_admin.go`（agent add / update 收 `isolation` / `image`；task 列表帶容器名）
- Modify: `internal/channelagent/a2a_admin_test.go`
- Modify: `cmd/claude-cron/a2a_cmd.go`（`--isolation` / `--image`）
- Modify: `web/admin/src/Agents.svelte`

**Interfaces:**
- Consumes: `Agent.Isolation` / `Agent.Image`（Task 7）、`A2ATask.Isolation`（Task 7）、`ContainerName`（Task 5）
- Produces：admin API 的 agent JSON 多兩個欄位；task JSON 多 `isolation` 與 `container`

**約束重申：admin API 仍是 `agents.json` 的唯一寫入者。** CLI 只是 HTTP 客戶端，UI 也是。這個 task 不新增任何第二個寫入點。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestAdminAgentAddAcceptsIsolationAndImage(t *testing.T) {
	root := t.TempDir()
	srv := newTestAdminA2A(t, root) // 沿用既有 a2a_admin_test.go 的 helper
	body := `{"name":"demo","project_dir":"/tmp/p","capabilities":["x"],"enabled":true,` +
		`"isolation":"container","image":"cc-a2a-sandbox:custom"}`
	rec := srv.post(t, "/api/a2a/agents", body)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	agents, err := LoadAgents(root)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := agents.Get("demo")
	if !ok {
		t.Fatal("agent not stored")
	}
	if a.Isolation != IsolationContainer || a.Image != "cc-a2a-sandbox:custom" {
		t.Fatalf("stored %+v", a)
	}
}

func TestAdminAgentAddRejectsUnknownIsolation(t *testing.T) {
	root := t.TempDir()
	srv := newTestAdminA2A(t, root)
	rec := srv.post(t, "/api/a2a/agents",
		`{"name":"demo","project_dir":"/tmp/p","capabilities":["x"],"isolation":"vm"}`)
	if rec.Code == 200 {
		t.Fatal("an unknown isolation must be rejected, not silently stored")
	}
	if agents, _ := LoadAgents(root); len(agents.Agents) != 0 {
		t.Fatal("nothing must be written on a rejected request")
	}
}

// update 用 pointer 語意：沒帶的欄位不得被清空。
func TestAdminAgentUpdateLeavesIsolationAloneWhenAbsent(t *testing.T) {
	root := t.TempDir()
	srv := newTestAdminA2A(t, root)
	_ = srv.post(t, "/api/a2a/agents",
		`{"name":"demo","project_dir":"/tmp/p","capabilities":["x"],"isolation":"container"}`)
	rec := srv.post(t, "/api/a2a/agents/demo/update", `{"description":"changed"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	agents, _ := LoadAgents(root)
	a, _ := agents.Get("demo")
	if a.Isolation != IsolationContainer {
		t.Fatalf("isolation was cleared by an unrelated update: %+v", a)
	}
	if a.Description != "changed" {
		t.Fatalf("description not applied: %+v", a)
	}
}

func TestAdminTaskListReportsIsolationAndContainerName(t *testing.T) {
	root := t.TempDir()
	srv := newTestAdminA2A(t, root)
	_ = SaveTasks(root, TaskStore{Tasks: []A2ATask{
		{ContextID: "c1", Session: "aa-demo-c1", State: TaskWorking, Isolation: IsolationContainer},
		{ContextID: "c2", Session: "aa-demo-c2", State: TaskWorking, Isolation: IsolationTmux},
	}})
	rec := srv.get(t, "/api/a2a/tasks")
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if rows[0]["isolation"] != IsolationContainer || rows[0]["container"] != "cc-a2a-aa-demo-c1" {
		t.Fatalf("row0 = %+v", rows[0])
	}
	// tmux 的列不得憑空長出一個容器名。
	if rows[1]["container"] != nil && rows[1]["container"] != "" {
		t.Fatalf("row1 must have no container: %+v", rows[1])
	}
}
```

- [ ] **Step 2: 跑測試確認它失敗**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestAdminAgent(Add|Update)|TestAdminTaskList' -race -v`
Expected: FAIL

- [ ] **Step 3: 改 admin API**

`a2a_admin.go` 的 agent 新增請求結構加兩個欄位並驗證：

```go
	Isolation string `json:"isolation,omitempty"`
	Image     string `json:"image,omitempty"`
```

驗證（與 `a2aNameRe` 那一組驗證放在一起）：

```go
	// 空字串 = 跟全域，合法。除此之外只接受兩個已知值 —— 打錯字的隔離模式
	// 若被存進 agents.json，EffectiveIsolation 會安靜地退回全域，operator
	// 以為自己設好了、其實沒有。在寫入這一層擋住比較誠實。
	if req.Isolation != "" && !ValidIsolation(req.Isolation) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown isolation %q (want %q or %q)", req.Isolation, IsolationTmux, IsolationContainer))
		return
	}
```

update 路由沿用既有的 pointer 語意（`*string`），只在 key 出現時才寫入。

task 列表的序列化（既有 `taskSnapshotPayload` 或 admin 專用的 row 組裝）加：

```go
	row["isolation"] = t.Isolation
	if t.Isolation == IsolationContainer {
		if name, err := ContainerName(t.Session); err == nil {
			row["container"] = name
		}
	}
```

- [ ] **Step 4: 改 CLI**

`cmd/claude-cron/a2a_cmd.go` 的 `case "agent add"` 增加兩個 opts：

```go
	case "agent add":
		out, err = c.do(http.MethodPost, "/api/a2a/agents", map[string]any{
			"name": arg, "project_dir": opts["project"], "description": opts["description"],
			"capabilities": splitCSV(opts["capabilities"]), "channel_id": opts["channel"],
			"enabled": flags["enabled"],
			// 空字串 = 跟全域設定走。
			"isolation": opts["isolation"], "image": opts["image"],
		})
```

`case "agent update"` 用既有的「只送有出現的 flag」模式：

```go
		if v, ok := opts["isolation"]; ok {
			body["isolation"] = v
		}
		if v, ok := opts["image"]; ok {
			body["image"] = v
		}
```

`a2aUsage` 的說明文字補上這兩個旗標，並寫明「`--isolation=container` 需要映像與網路已存在，否則 `serve` 啟動時會拒絕啟用整個 A2A」。

- [ ] **Step 5: 改 admin UI**

`web/admin/src/Agents.svelte`：
- agents 表格加一欄「隔離」，顯示 `agent.isolation || '（全域）'`；`container` 用一個明顯的標記（例如 🧱）。
- 新增/編輯表單加一個 select：`（跟全域）` / `tmux` / `container`，以及一個 image 文字欄位（只在選 `container` 時顯示）。
- tasks 表格加一欄顯示 `task.container`（沒有就留空）。
- i18n 字串加進 `lib/i18n.svelte.js`（與既有 `agents.*` 同一組）。

**不做確認對話框**：切換隔離模式**不是擴權**（`confirmRaiseLevel` 那條規則是給 `readonly → full` 用的），而且切成 `container` 是收緊。

- [ ] **Step 6: 跑測試、建 UI、commit**

Run:
```bash
cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5
cd web/admin && npm run build && cd ../..
go test ./internal/channelagent/ -run TestAdminSPA -race -v
```
Expected: 全部 ok（`admin_spa_smoke_test.go` 會驗證 `admin_dist` 的內容）

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_admin.go internal/channelagent/a2a_admin_test.go \
        cmd/claude-cron/a2a_cmd.go web/admin/src internal/channelagent/admin_dist
git commit -m "feat(a2a): expose isolation mode and sandbox image through the admin API, CLI and UI"
```

---

### Task 13: 【需 operator】端到端實跑（一）—— `readonly` 委派

**到這一步為止，容器模式從來沒有真的跑過一次完整的委派。** 前面 12 個 task 的測試全部是單元層級（依 Global Constraints，它們永遠不得起容器）。這個 task 是第一次真的把開關打開。

**Files:**
- Modify: `docs/superpowers/notes/2026-08-07-a2a-container-probe.md`（追加端到端結果）
- 可能 Modify：任何實跑抓到的缺陷（見下）

**前置：** Task 2 的 probe 全綠、Task 12 已合併、`scripts/a2a-net-up.sh` 已跑過。

- [ ] **Step 1: 準備一個 scratch repo 與一個容器模式的 agent**

**不要用線上專案。** 共用 `.git` 是 rw 掛載（風險 1），第一次實跑不該拿 47 個 binding 共用的 repo 當白老鼠。

```bash
P=/home/conray/project/a2a-scratch
rm -rf "$P" && mkdir -p "$P" && cd "$P"
git init -q && git config user.email a2a@local && git config user.name a2a
printf 'hello\n' > README.md && git add -A && git commit -qm init
git checkout --detach   # 避免之後 worktree add 撞到同一個分支
```

```bash
claude-cron a2a agent add scratch \
  --project="$P" --description="容器隔離端到端測試" \
  --capabilities=demo --isolation=container --enabled
claude-cron a2a agent list
```
Expected: `scratch` 出現，`isolation` 是 `container`。

- [ ] **Step 2: 【OP】設定並重啟 serve**

**停下來等人。** 在 `config.json` 的 `a2a` 區塊確認：`enabled: true`；`isolation` 依 Task 7 的 OP-7 決定（建議維持 `"tmux"`，靠 agent 覆寫）。然後重啟：

```bash
systemctl --user restart claude-cron-serve.service
sleep 5
systemctl --user status claude-cron-serve.service --no-pager | head -20
journalctl --user -u claude-cron-serve.service --since '1 min ago' --no-pager | tail -30
```

判讀標準：
- **不可以**看到 `a2a disabled: …`。看到就代表前置檢查失敗（映像、網路、或 `A2A_CLAUDE_CODE_OAUTH_TOKEN` 沒設），先修好。
- 47 個 cc- binding 的 session 必須全部還在：`tmux ls | grep -c '^cc-'` 應與重啟前相同。

- [ ] **Step 3: 【OP】派一個 `readonly` 委派**

```bash
claude-cron a2a caller register e2e-1 --credential=<自選>       # 若尚未註冊
claude-cron a2a caller approve e2e-1
claude-cron a2a caller set-level e2e-1 readonly
```

用 A2A JSON-RPC 送一個 `message/send`（照 `docs/superpowers/specs/2026-08-05-a2a-integration-design.md` 的格式），prompt 用一個**只需要讀**的任務，例如：「讀 README.md，用一句話說它的內容」。

- [ ] **Step 4: 【OP】逐條驗證**

```bash
# 1. 容器真的起來了，而且掛載表就是 Task 5 那一張
docker ps --filter label=cc.a2a.session --format '{{.Names}}\t{{.Status}}'
C=$(docker ps -q --filter label=cc.a2a.session | head -1)
docker inspect "$C" -f '{{range .Mounts}}{{.Source}} -> {{.Destination}} ({{if .RW}}rw{{else}}ro{{end}})
{{end}}'
```
判讀：worktree 是 **ro**；政策目錄是 **ro**；`R` 本身、`bindings.json`、`callers.json`、`.env`、`~/.claude/`、`docker.sock` **一個都不在**。

```bash
# 2. 核心真的擋住寫入（readonly 的第一次「不可寫」由核心保證）
docker exec "$C" bash -c 'echo x > README.md'; echo "exit=$?"
# 3. 政策檔改不動 —— 這是整條路線存在的理由
docker exec "$C" bash -c 'echo "{}" > '"$(docker inspect "$C" -f '{{range .Mounts}}{{if eq .Destination .Source}}{{end}}{{end}}')"'/policy.json' 2>&1 | tail -2
```
判讀：兩者都必須是 `Read-only file system`。

```bash
# 4. 判定紀錄真的回到正本 gate log
claude-cron a2a audit --gate --session=<session> | tail -20
# 5. 活動鏡像真的有東西（agent 頻道）
```
判讀：gate log 有這次委派的判定行；agent 的 Discord 頻道有 `🟢 driver started` **以及**至少一行 `💭` 或工具行——只有 🟢/🔴 而沒有活動行，代表 Task 9 的 transcript 覆寫沒生效。

```bash
# 6. 結果真的回到呼叫方
claude-cron a2a task list --state=completed
```

- [ ] **Step 5: 【OP】驗證拆除**

等 `RetainAfterComplete`（10 分鐘）過去，或手動 `claude-cron a2a task cancel <ctx>`，然後：

```bash
docker ps -a --filter label=cc.a2a.session   # 該 session 的容器必須消失
ls /home/conray/project/claude_cron/.channel-agent/a2a-policies/   # 該 session 的目錄必須消失
ls /home/conray/project/claude_cron/.channel-agent/a2a-gate-spool/ # spool 與 offset 必須消失
ls /home/conray/project/a2a-scratch/../ | grep aa-   # worktree 必須消失（completed 不保留）
```

- [ ] **Step 6: 把結果追加進 probe 筆記，並修掉抓到的缺陷**

實跑幾乎必定會抓到東西（前三輪 A2A 每一輪都被 review 判過 DO NOT SHIP，而這套東西到 Task 13 為止**一次都沒有真的端到端跑過**）。處理原則：
- 小的、明確的缺陷 → 直接修，補一條會失敗的單元測試，跟著這個 task 一起 commit。
- 需要設計決定的 → **不要在這裡臨時決定**，寫進 probe 筆記回報，另開 task。

- [ ] **Step 7: Commit**

```bash
cd /home/conray/project/claude_cron
git add docs/superpowers/notes/2026-08-07-a2a-container-probe.md internal/ cmd/
git commit -m "test(a2a): first end-to-end readonly delegation under container isolation"
```

---

### Task 14: 【需 operator】端到端實跑（二）—— `develop` 委派與「碰不到」清單逐條驗證

**Files:**
- Modify: `docs/superpowers/notes/2026-08-07-a2a-container-probe.md`

- [ ] **Step 1: 【OP】派一個 `develop` 委派，驗證交付模型**

```bash
claude-cron a2a caller set-level e2e-1 develop
```

prompt 用一個**需要改檔並 commit** 的任務，例如：「在 README.md 加一行 `container-isolation ok`，然後 commit」。

驗證：

```bash
# 沙盒的 commit 真的落在共用 repo 的 aa/<session> 分支上
git -C /home/conray/project/a2a-scratch log --oneline -3 aa/<session>
git -C /home/conray/project/a2a-scratch show aa/<session>:README.md
```
判讀：commit 存在、內容正確。**這同時再一次確認了風險 1 是真的**：容器內的行程寫得動共用 repo 的 ref。

- [ ] **Step 2: 【OP】驗證 egress allowlist 的兩個已知行為改變**

```bash
C=$(docker ps -q --filter label=cc.a2a.session | head -1)
# git push 到外部 remote 應該被擋（除非 OP-6 決定放行）
docker exec "$C" bash -c 'cd <worktree> && timeout 20 git ls-remote https://gitlab.jvdtech.dev/ 2>&1 | tail -2'
```
Expected: 連線失敗或 403（代理拒絕）。

`WebFetch` 的行為：在委派 prompt 裡要求沙盒 `WebFetch` 一個外部網址，觀察它是否失敗；`WebSearch` 走上游 API，**應該**還能用——但這是規格自己說「應該」而沒有驗證的一項，這裡必須實測並記錄，**不要當成事實寫進文件**。

- [ ] **Step 3: 【OP】逐條核對規格第六節的「碰不到」清單**

在容器內逐條嘗試，**每一條都要記錄實際輸出**：

| 應該碰不到 | 驗證指令（在容器內） | 期望 |
|---|---|---|
| `~/.claude/.credentials.json` | `ls /home/conray/.claude/.credentials.json` | No such file |
| `.env` | `ls /home/conray/project/claude_cron/.env` | No such file |
| `callers.json` / `agents.json` / `tasks.json` / `bindings.json` | `ls /home/conray/project/claude_cron/.channel-agent/` | No such file（整個 `R` 都不存在） |
| 其他 binding 的 worktree | `ls /home/conray/project/` | 只看得到自己的（或不存在） |
| 其他沙盒 | `ls <R>/sandboxes/` | No such file |
| 自己的政策檔（寫入） | `echo x >> <政策目錄>/policy.json` | Read-only file system |
| admin API `127.0.0.1:8787` | `timeout 3 bash -c '</dev/tcp/127.0.0.1/8787'` | 失敗（loopback 不共用） |
| A2A `127.0.0.1:8790` | 同上換埠 | 失敗 |
| `fatgame-mysql` | `timeout 5 bash -c "</dev/tcp/<bridge-gw>/3306"` | 失敗 |
| MCP `8001` / `8003`、SiYuan `6806` | 同上 | 失敗 |
| `/var/run/docker.sock` | `ls -la /var/run/docker.sock` | No such file |
| MCP server | `claude mcp list`（在容器內） | 空清單 |

**任何一條不符合預期，就是一個必須修掉的缺陷，不是「已知限制」。** 掛載表是這份設計唯一的邊界，它列錯了就沒有第二道防線。

- [ ] **Step 4: 【OP】驗證 `develop` 的自我提權路徑真的消失**

這是整條路線存在的理由，必須明確地試一次：

```bash
C=$(docker ps -q --filter label=cc.a2a.session | head -1)
# 1. 政策檔寫不動
docker exec "$C" bash -c 'echo "{\"level\":\"full\"}" > <政策目錄>/policy.json' 2>&1
# 2. 映像裡沒有 python / node / go 可以拿來改 JSON
docker exec "$C" bash -c 'which python3 node go; echo "exit=$?"'
```
Expected：(1) `Read-only file system`；(2) 三個都找不到。

- [ ] **Step 5: 把全部結果追加進 probe 筆記並 commit**

```bash
cd /home/conray/project/claude_cron
git add docs/superpowers/notes/2026-08-07-a2a-container-probe.md internal/ cmd/
git commit -m "test(a2a): end-to-end develop delegation and the full isolation checklist"
```

---

### Task 15: 【需 operator】cc- 無影響驗證、kill switch 演練，與收尾決策記錄

**Files:**
- Modify: `docs/superpowers/notes/2026-08-07-a2a-container-probe.md`
- Modify: `internal/channelagent/a2a_gate.go`（**只加註解，不改邏輯**）
- Create: `docs/superpowers/notes/2026-08-07-a2a-container-rollout.md`

- [ ] **Step 1: 【OP】確認 47 個 cc- binding 完全無感**

在 Task 13/14 的容器沙盒跑過之後，對照 Task 13 Step 2 之前記下的基準：

```bash
tmux ls | grep -c '^cc-'                 # session 數量不變
claude-cron list                          # 每個 binding 的狀態不變
journalctl --user -u claude-cron-serve.service --since '1 hour ago' --no-pager | grep -i 'error\|panic' | head -20
free -h                                   # available 沒有被容器吃光
```

再實際在一個 cc- 頻道發一則訊息，確認正常回覆。

**判讀標準：任何一個 cc- binding 的行為改變都是必須立刻回退的缺陷。** 這是 Global Constraints 的第一條，不接受「應該沒關係」。

- [ ] **Step 2: 【OP-9】kill switch 演練**

```bash
# 1. 把那個 agent 切回 tmux
claude-cron a2a agent update scratch --isolation=tmux
# 2. 或整個全域切回（若 OP-7 選了 (a)）：改 config.json 的 a2a.isolation = "tmux"
systemctl --user restart claude-cron-serve.service
```

驗證：
- 新派的委派用 tmux session（`tmux ls | grep '^aa-'`），不起容器。
- **已經在跑的容器不會被新設定認領**——它們由 `ReapOrphanContainers` 依 label 清掉。等 `OrphanGrace`（5 分鐘）過後再看 `docker ps -a --filter label=cc.a2a.session`，應該為空。
- 最極端的 kill switch 仍然有效：`a2a.enabled = false` + 重啟 → `tmux ls | grep -c '^aa-'` 為 0，且 serve 完全不跑 A2A cycle。

- [ ] **Step 3: 在 gate 的旗標允許清單上加註解（唯一的程式碼改動）**

`a2a_gate.go` 的 `readonlyHeadFlags` 上方加：

```go
// 【容器隔離之後的狀態，2026-08-07】這一整組旗標允許清單（flagPolicy、
// flagTokenAllowed、firstDeniedFlag、readonlyHeadFlags、readonlyFindTokens、
// findDecision、readonlyGitSubFlags）存在的原始理由是「Bash 判定做不到路徑
// 侷限」。在 isolation == "container" 的沙盒裡，readonly 的 worktree 是 :ro
// 掛載，路徑侷限由核心負責，這一組因此變成縱深防禦而不是唯一防線。
//
// **但它一個字都不能刪**，兩個理由：
//  1. tmux 模式仍然存在且仍然是預設值（a2a.isolation 預設 "tmux"）。刪掉
//     會讓 tmux 沙盒比今天更弱。
//  2. 底下的 bashDecision 首 token 允許清單、gitDecision 的子命令與
//     pushArgsAllowed **在容器模式下也完全不多餘**：共用 repo 的 .git 是
//     rw 掛載，它們仍然是唯一擋住「動到同專案所有 cc- binding 共用分支」
//     的東西。容器沒有取代它們。
//
// 真正可以刪的時機是 tmux 模式正式 deprecated 之後，屆時另開計畫處理。
```

**這是本 task 唯一的程式碼改動。不刪任何東西。**

- [ ] **Step 4: 寫 rollout 決策記錄**

`docs/superpowers/notes/2026-08-07-a2a-container-rollout.md` 必須包含：

1. **目前生效的設定**：全域 `a2a.isolation` 是什麼、哪些 agent 有覆寫、egress allowlist 的內容、併發上限的實際值。
2. **三條殘留風險的最終狀態**（照本計畫第三節逐條回答，用實跑的證據而不是預期）：
   - 共用 `.git` rw：Task 14 Step 1 證實沙盒寫得動共用分支。gate 的 git 規則仍是唯一防線。要不要改 per-sandbox `git clone --local`（+2～3 task）——待決。
   - 憑證 scope 無法縮小：選項 B 已上線，**operator 手上有一份可獨立撤銷的 A2A token**。撤銷方式必須寫在這裡（怎麼作廢、作廢後怎麼換新的）。選項 C（+3～4 task）——待決。
   - task 數與併發：實際做了 15 個 task；併發實測值與最終設定。
3. **第二節那張表的最終狀態**：哪些 confinement 元件現在是縱深防禦、哪些仍是唯一防線、什麼時候才可以刪。
4. **已知的功能倒退清單**（每一條都要有實跑證據）：`develop` 在共用映像裡跑不動 `pytest` / `npm test` / `go test`；`WebFetch` 在 egress allowlist 下的實際行為；`git push` 到外部 remote 的實際行為；沙盒不繼承 operator 的 skills / CLAUDE.md / MCP；容器重建後對話歷史不續上。
5. **運維物件清單**：映像（怎麼重建）、`cc-a2a` 網路與 `cc-a2a-egress` 代理（`scripts/a2a-net-up.sh`，**必須確認開機時會被建立**——加進 `scripts/boot-claude-cron.sh` 或一個 systemd `--user` unit，這一項若還沒做要明確標為 TODO）。
6. **規格開放問題 7 與 10 的狀態**：gate spool 的 drop-box 強化（本計畫不做）；`cc-a2a-egress` 掛掉時的行為（本計畫沿用「沙盒 API 呼叫失敗 → driver 連續 3 次失敗 → 存活檢查 → 標 failed」，**沒有**加「代理不健康時暫停 `DrainQueue`」的閘——這會把一次代理重啟變成一批任務失敗，是已知且未處理的）。

- [ ] **Step 5: 全套測試 + commit**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... -race 2>&1 | tail -5`
Expected: 全部 ok

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_gate.go \
        docs/superpowers/notes/2026-08-07-a2a-container-probe.md \
        docs/superpowers/notes/2026-08-07-a2a-container-rollout.md
git commit -m "docs(a2a): container rollout state, residual risks, and why no confinement code was deleted"
```

---

## Self-Review

### 1. 規格涵蓋度

| 規格章節／要求 | 對應 task |
|---|---|
| 零、既有事實（glibc 基底、不烘 claude） | 1 |
| 一、邊界（什麼在容器內／外） | 5（掛載表）、6（只有 Start 進容器） |
| 二、掛載表 + 路徑同構 + 容器啟動參數 | 5 |
| 二、`CLAUDE_CONFIG_DIR` 的三個附帶收穫 | 5（env）、6（`TrustFolder`）、9（transcript） |
| 三、prompt 進出不跨界；`paneDriver` 抽象 | 3（抽象）、9（driver 接線） |
| 四、政策檔 `:ro` 與 rename/inode 問題 | 4 |
| 五、gate spool + `DrainGateSpool` + 三個性質 | 8 |
| 六、認證（選項 B）、攻擊者拿得到什麼 | 2（OP-2/OP-3/OP-4a）、5（token 不進 argv）、14（清單逐條驗證） |
| 七、網路與出口、`--internal` 實測 | 0.1（已關閉）、2（腳本 + 實測） |
| 八、活動鏡像修復、`latestTranscript` 的行為變化 | 9 |
| 九、sweep 拆什麼、三分法、孤兒容器回收 + grace | 6（三分法）、8（spool 清理）、10（reap） |
| 十、映像內容、建置、與 host 版本同步、映像存在性檢查 | 1（映像 + 檢查）、7（前置檢查與「拒絕啟用整個 A2A」） |
| 十一、必改（新檔／既有檔）逐項 | 1,3,4,5,6,7,8,9,10,12 |
| 十一、可刪的 confinement 元件 | **第二節 + Task 15：明確保留，不刪**，理由寫在兩處 |
| 十二、47 個 cc- binding 的七條硬性約束 | Global Constraints + 3（argv 逐字不變）+ 11（記憶體）+ 15（實跑驗證） |
| 十三、遷移路徑、per-agent 覆寫、`A2ATask.Isolation`、kill switch | 7、15 |
| 十四、task 數與成本 | 第三節（15 個，落在 13–16，逐項說明差異） |
| 十五、明確不做與已接受的限制 | 第三節 + Task 15 Step 4 的 rollout 記錄 |
| 十六、開放問題 1–10 | 1（無）、2/3/4/5/6/8（Task 2 的 OP-2/OP-5/OP-6 決策點 + Task 15 記錄）、7（不做，記錄）、9（第二節：不刪）、10（不做，記錄） |

**沒有找到未被涵蓋的規格要求。** 唯一刻意不做的是規格第六節的選項 C（host 端注入認證的代理）與開放問題 1 的 per-sandbox `git clone --local`——兩者在第三節都寫明了「不做、代價、以及什麼情況下必須做」。

### 2. Placeholder 掃描

逐條檢查「No Placeholders」的紅旗：
- 沒有 TBD / TODO / 「之後補」——唯一出現「TODO」字樣的是 Task 15 Step 4 要求 rollout 筆記標註 `a2a-net-up.sh` 的開機接線狀態，那是**產出文件的內容要求**，不是計畫本身的缺口。
- 沒有「加上適當的錯誤處理」這類空話——每一處錯誤處理都寫明了要走哪一條分支（三分法、fail-closed、只 log 不中止），並附測試。
- 沒有「參照 Task N」代替程式碼——`RemoveContainer` / `ContainerAlive` / `paneArgv` 等在被引用的每一個 task 都有完整簽章重述。
- 每一個「寫實作」步驟都有實際可貼上的程式碼；每一個「寫測試」步驟都有完整可跑的測試函式。
- 【OP】步驟都有實際指令、預期輸出、判讀標準，以及失敗時的處理方式。

### 3. 型別／命名一致性

| 名稱 | 定義於 | 被引用於 | 一致 |
|---|---|---|---|
| `dockerRunner.Run(ctx, env, args...)` | 1 | 5,6,10 | ✓（`env` 參數的唯一用途是 token，在 5 與 6 都說明了） |
| `dockerSaysAbsent` | 1 | 6 | ✓ |
| `SandboxImageAvailable` | 1 | 7 | ✓ |
| `paneDriver` / `paneArgv` / `paneRun` / `paneOutput` | 3 | 3,9 | ✓ |
| `dockerTmux{container}` | 3 | 9（`sandboxPaneDriver`） | ✓ |
| `capturePaneVia` / `sendConfirmChoiceVia` | 3 | 9 | ✓ |
| `PolicySessionDir` / `PolicyPath` | 4 | 5（掛載表）、6（`LoadSandboxPolicy`） | ✓ |
| `GateSpoolPath` / `GateSpoolDir` | 5（先定義） | 6（Start 建檔）、8（drain） | ✓ **刻意在 5 定義，因為掛載表要引用它** |
| `GateSpoolOffsetPath` / `DrainGateSpool` / `RemoveGateSpool` | 8 | 8（cycle）、8（`removeCandidate`） | ✓ |
| `ContainerName` | 5 | 6,9,10,12 | ✓（一律回 `(string, error)`） |
| `ContainerOpts` / `DefaultContainerOpts` | 5 | 6,7 | ✓ |
| `ContainerSessionManager{Root, Opts, Docker}` | 5 | 6,7,10 | ✓ |
| `IsolationTmux` / `IsolationContainer` / `ValidIsolation` / `EffectiveIsolation` | 7 | 7,9,10,11,12 | ✓ |
| `RoutingSessionManager` | 7 | 7（main.go）、10（`ReapOrphans`） | ✓ |
| `A2ATask.Isolation` | 7 | 7,9,10,12 | ✓ |
| `Agent.Isolation` / `Agent.Image` | 7 | 7,12 | ✓ |
| `orphanReaper` / `ReapOrphans` | 10 | 10（cycle） | ✓ |
| `EffectiveMaxSandboxes` / `SetMaxConcurrentSandboxes` | 11 | 11（`HasCapacity`、`DrainQueue`、main.go） | ✓ |
| `CollectActivityIn` / `sandboxTranscriptDir` / `latestTranscriptIn` | 9 | 9（driver） | ✓ |

**已核對過的既有名稱**（動工時可直接使用，不需要再猜）：`isTerminal`（`a2a_executor.go:68`，未匯出，同 package 可用）、`TasksPath`（`a2a_tasks.go:127`）、`AtomicWriteFile`（`fileutil.go:87`）、`parseRFC3339`（`a2a_lifecycle.go:412`）、`lockSandboxSession` / `tryLockSandboxSessionForTeardown`（`a2a_sessionlock.go:96,165`）、`removeCandidate`（`a2a_lifecycle.go:1312`）、`appendRotatingLine`（`a2a_audit.go:115`）、`encodeProjectDir`（`worktree.go:430`）、`sessionTranscriptPath`（`session_hook.go:51`）。

**唯二仍需現場確認的名稱**：Task 8 測試裡的 `timeNowForTest()`（沒有就直接用 `time.Now()`）與 Task 12 測試裡的 `newTestAdminA2A` / `srv.post` / `srv.get`（沿用 `a2a_admin_test.go` 裡實際存在的 helper 名稱）。兩者都只影響測試碼，不影響產品程式碼的介面。

### 4. 排序自檢

- **半成品不會看起來像做完了：** Task 1–6 之後 `ContainerSessionManager` 完整存在但**零呼叫端**；Task 7 是唯一接線點，且預設值讓行為不變。任何一個 task 停下來，分支都處於「tmux 模式完好」的狀態。
- **兩條路徑始終並存：** Task 7 之後每一個 task 的驗收條件都包含「tmux 模式的既有測試全綠」；Task 15 演練切回。
- **風險前置：** 認證與共用 `.git` 在 Task 2 就用實跑證明，不是假設。Task 2 之後若任一項是紅的，後面 13 個 task 不該開工——這一條寫在 Task 2 結尾的檢查點。
