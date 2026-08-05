# A2A（Agent-to-Agent）整合設計

日期：2026-08-05
狀態：規格已定案，尚未實作

## 目的

讓外部 agent 能透過 [A2A 協定](https://a2a-protocol.org/)（Google 開放標準，JSON-RPC 2.0 over HTTP）委派任務給這台機器上的代理。A2A 與 MCP 互補：MCP 管一個 agent 內部怎麼呼叫工具，A2A 管 agent 之間怎麼發現彼此、互相委派。

## 核心概念：兩套前綴的分工

| 前綴 | 意義 | 對外 |
|---|---|---|
| `cc-<name>` | **你的任務**。既有的工作頻道（Discord/Telegram binding） | 永不對外 |
| `aa-<agent>` | **你的代理**。刻意建立的 agent 身分 | Agent Card 公告 |
| `aa-<agent>-<ctx>` | 代理底下的**任務實例**，每個 contextId 一個 | 不公告 |

`aa-<agent>` 是**身分**，本身不執行任務；實際執行的是 `aa-<agent>-<ctx>`。同一個代理可同時有多個 ctx 實例，彼此不共用 context。

既有的 `cc-` 機制（`bindings.json`、supervisor 迴圈、`reap.go`）完全不改動。

## 狀態檔

三份獨立 JSON，不碰既有的 `bindings.json`：

### `agents.json`
| 欄位 | 說明 |
|---|---|
| `name` | 代理名（組成 `aa-<name>`） |
| `project_dir` | 對應的專案目錄 |
| `description` | 對外描述，Agent Card 用 |
| `capabilities` | 能力清單 |
| `enabled` | 是否啟用 |

### `callers.json`
| 欄位 | 說明 |
|---|---|
| `caller_id` | 呼叫方識別 |
| `credential` | 憑證 |
| `status` | `pending` / `approved` / `revoked` |
| `granted_capabilities` | 授權能力清單 |

### `tasks.json`
| 欄位 | 說明 |
|---|---|
| `context_id` | A2A contextId |
| `task_id` | A2A taskId |
| `agent` | 對應代理 |
| `session` | tmux session 名 |
| `worktree` | worktree 路徑 |
| `state` | 見狀態機 |
| `started_at` | 起始時間 |

## 安全模型

1. **對外開放，但要核准**：任何人可申請註冊，狀態為 `pending`，需人工核准後才生效。核准動作走既有的 permission gate 模式（Discord 按鈕）。
2. **per-caller 能力授權**：核准時決定該 caller 能用哪些能力，於後台（`admin.go` 的 `/app/` Svelte UI）管理。
3. **執行當下不再詢問**：授權清單就是全部政策。範圍內直接放行；範圍外自動拒絕，回 A2A `failed` 附原因。
   - 這也繞開一個架構死結：`aa-*-<ctx>` 沙盒不隸屬任何頻道，permission gate 的 `bindingByWorktree(cwd)` 找不到可發問的頻道。
4. **Agent Card 逐一 opt-in**：預設不公告。Agent Card 等同公開，若全部曝光會洩漏專案名、客戶名、Jira 票號。
5. **獨立監聽埠**：A2A server 跑在 `serve` 行程內，但**必須用與 admin API 不同的埠**。admin API（預設 `127.0.0.1:8787`）能建立可跑 shell 的 binding，等同機器的 root 入口，絕不可對外。

## 執行流程

```
外部 agent ──POST /a2a──▶ A2A server（serve 內，獨立 port）
                              │ 1. 驗 caller 憑證、查授權能力
                              │ 2. 查 contextId 有無既有實例
                              │ 3. 無則 git worktree add + 開 tmux session
                              │ 4. inject 任務內容
                              ▼
                    aa-<agent>-<ctx> 執行
                              │ 寫結果 JSON 到 outbox
                              ▼
              A2A server 偵測到檔案 ──▶ 回 A2A 回應（含分支名）
```

**完成判定**沿用既有 outbox 檔案約定：沙盒寫出結果 JSON 即視為完成。不刮 tmux 畫面猜狀態。

## 工作區隔離

每個 contextId 一個 git worktree，沿用既有的 `EnsureWorktree()`。

- 成本實測：一個 worktree 約 80 MB；fatgame 現有 25 個；磁碟餘裕 61 GB。8 個併發約 640 MB，可接受。真正成本是 `git worktree add` 的建立時間，不是空間。
- **已知限制**：worktree 只隔離檔案，**不隔離外部狀態**——本機資料庫（fatgame MySQL、planetscale）、Docker、cache 仍為共用。若日後委派任務需要 DB 隔離，須改採「worktree + 每個 worktree 一個輕量容器」的混合模式。此版不處理。

## 產出交還

沙盒把改動 commit 到自己的分支並 push，A2A 回應帶分支名。與既有的 worktree → 分支 → MR 流程一致，reviewer 用平常方式檢視，不需為 A2A 另立一套流程。

## 生命週期

### 狀態機

```
submitted ──▶ working ──▶ completed   結果 JSON 出現，回分支名
                  │
                  ├──▶ failed        沙盒回報錯誤、或能力不足
                  └──▶ canceled      硬上限到、或呼叫方主動取消
```

### 時間門檻

| 事件 | 時間 | 動作 |
|---|---|---|
| 軟上限 | 30 分 | 維持 `working`，回報進度，不砍 |
| 硬上限 | 2 小時 | 砍 session，轉 `canceled`，回報逾時 |
| 閒置回收 | 完成後 10 分 | 收 session 與 worktree（**保留分支**） |

完成後保留 10 分鐘，讓呼叫方能在同一 contextId 追問而不必重建沙盒。

參考點：既有 permission gate 逾時為 1800 秒；實測日報任務曾跑逾 11 分鐘。長時間執行屬常態。

### 併發

- 全域同時最多 **8 個** `aa-*-<ctx>` 實例（業界建議 8-10，取保守值）
- 額滿時新任務**排隊**（狀態 `submitted`），不直接拒絕
- 同時作為記憶體保護

### 失敗處理

- **不自動重試**。任務有副作用（已改檔、可能已 push），盲目重跑會產生重複 commit 或半套狀態。失敗即回報，由呼叫方決定是否重新委派。
- **能力不足**：直接回 `failed` 附原因。
- **沙盒崩潰**：session 不存在但任務未完成 → 標 `failed`，**worktree 保留**供事後查驗，不自動清除。

### 稽核

所有委派寫入 append-only log：時間、caller、代理、contextId、任務摘要、結果狀態、分支名。

## 測試策略

不得以真實 session 做測試（消耗訂閱額度、慢、不穩）。分三層：

| 層 | 測什麼 | 方法 |
|---|---|---|
| 純函式 | 憑證驗證、能力比對、contextId → session 名推導、狀態機轉換、併發計算 | 一般單元測試 |
| 檔案協定 | 三份 JSON 讀寫、outbox 結果檔偵測、稽核 log 追加 | `t.TempDir()`，比照既有 `permission_test.go` |
| A2A 協定 | JSON-RPC 解析、Agent Card 產生、錯誤碼、未授權拒絕 | 測試用 HTTP server，**不開任何 tmux session** |

session 開關以介面（如 `SessionManager`）抽離，測試時替換為假實作，驗證呼叫與參數。

## 交付順序

四階段，每階段可獨立驗證：

1. **資料層** — 三份 JSON 的 schema 與讀寫、agents/callers CRUD。無網路、無 session。
2. **A2A 協定層** — HTTP server、Agent Card、JSON-RPC、認證授權。**任務執行接假實作**，回固定假結果。此階段結束時，外部已能發現代理、發任務、取得回應，協定層問題在碰到 session 前即暴露。
3. **執行層** — 接上真實 worktree 建立、session 開關、inject、結果偵測。
4. **生命週期** — 三個時間門檻、併發上限、排隊、稽核 log。

## 明確不做

- 不改動既有 `cc-` 機制
- 不做自動重試
- 不做容器層隔離（DB 共用列為已知限制）
- 不將任何 `cc-` binding 曝光於 Agent Card
