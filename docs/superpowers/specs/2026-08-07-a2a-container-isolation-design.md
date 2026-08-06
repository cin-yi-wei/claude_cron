# A2A 沙盒容器隔離 設計

日期：2026-08-07
狀態：**選項評估用**。operator 尚未選定此路線；本規格的存在是為了讓這個決定有真實資訊可依據，不是已定案的工單。
前置：
- `2026-08-05-a2a-integration-design.md`（已實作）
- `2026-08-06-a2a-sandbox-driver-design.md`（已實作）
- `2026-08-06-a2a-confinement-design.md`（已實作，最終安全審查完成）

## 目的

confinement 那一輪把 gate 從 fail-open 修成 fail-closed，並以實跑證明 `readonly` 真的守得住（no exec / no write / no network）。同一份審查也證明 `develop` **會自我提權**：gate 讀的政策檔 `<root>/a2a-policies/<session>.json` 與沙盒是同一個 unix uid（`conray`），而 `develop` 依設計就能跑 `python` / `node` / `go run`——一行程式把 `level` 改成 `full` 即可（實測：`curl` 與 `mcp__*` 從 deny 變 allow）。

這不是檔案層可以修的缺陷，它是 same-uid 的直接推論。要關掉它只有兩條路：

| 路線 | 需要的權限變更 |
|---|---|
| 換 unix 使用者 | `serve` 是 systemd **`--user`** 單元（`claude-cron-serve.service`），不改 root 設定就換不了 uid。這台機器的 `sudo` 需要互動認證、沒有 NOPASSWD 規則，因此代價是一條 sudoers 或一個 setuid 執行檔——**常態性的特權擴張**。 |
| **容器（本規格）** | **零新特權**。`conray` 已在 `docker` 群組（gid 108），Docker 29.1.3 可用（apparmor + seccomp builtin + cgroupns + cgroup v2，已實跑過容器）。 |

本規格描述後者：政策檔以 `:ro` bind mount 進容器，**核心強制**沙盒改不到它；同時把「哪些路徑存在」這件事從「Bash 允許清單猜出來的近似值」換成「掛載表列舉出來的事實」。

---

## 零、這台機器的既有事實（本規格的地基）

以下每一條都在寫這份規格時實地查證過，不是推測。

| 事實 | 值 | 出處 |
|---|---|---|
| 架構 / OS | `aarch64`、Ubuntu 26.04 LTS、glibc 2.43 | `uname -m`、`/etc/os-release`、`ldd --version` |
| `claude` | 2.1.223，**原生 ELF 單檔**、275 MB、**動態連結 glibc**，裝在 `~/.local/share/claude/versions/<ver>`，`~/.local/bin/claude` 是 symlink。同時保留 2.1.221 / 2.1.222 | `file`、`ldd`、`ls` |
| `claude-cron` | Go binary 10.8 MB，**同樣動態連結 glibc**，`~/.local/bin/claude-cron` → `claude-cron-beta` | `file`、`ldd` |
| Docker | 29.1.3，**rootful**（`/var/run/docker.sock` 為 `root:docker 0660`），overlayfs + containerd snapshotter，cgroup v2 / systemd driver | `docker info`、`ls -la` |
| 已在跑的容器 | `siyuan`、`fatgame-mysql`（`mysql:8.0`，預設 bridge `172.17.0.2`，**對外發佈 `0.0.0.0:3306`**） | `docker ps`、`docker port` |
| 記憶體 | 總 17 GiB，**可用僅 5.2 GiB**；單一 `claude` 行程 RSS 實測 430–765 MB | `free -h`、`ps` |
| 磁碟 | `/` 97 GB，餘 55 GB | `df -h` |
| cc- binding | **47 個**（`bindings.json`） | 讀檔計數 |
| 憑證 | `~/.claude/.credentials.json`（0600, 523 B），`claudeAiOauth.scopes = [user:file_upload, user:inference, user:mcp_servers, user:profile, user:sessions:claude_code]`，`subscriptionType = team` | 只讀 key 結構，未讀 token 值 |
| OAuth token 模式 | `.env` 的 `CLAUDE_CODE_OAUTH_TOKEN` **目前是註解掉的**，所以現在跑的是 credentials.json 模式 | `.env` |
| `claude` 支援的環境變數 | binary 內含 `CLAUDE_CONFIG_DIR`、`CLAUDE_CODE_OAUTH_TOKEN`、`ANTHROPIC_BASE_URL`、`HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY`、`api.anthropic.com` | `strings` |

兩個立即的推論，直接決定了設計：

1. **基底映像必須是 glibc（Ubuntu 26.04 arm64），不能是 alpine。** `claude` 與 `claude-cron` 都動態連結 glibc 2.43。
2. **`fatgame-mysql` 對 `0.0.0.0:3306` 發佈。** 任何能走到 host gateway 的容器網路都碰得到它。網路設計不能只靠「不接上預設 bridge」（見第七節）。

---

## 一、邊界：什麼在容器內、什麼在容器外

**唯一在容器內的東西是「那一次委派任務的 Claude Code 行程與它的 tmux」，以及它必須就地執行的兩個工具（`claude` 本體、`claude-cron permission-gate`）。除此之外全部留在 host。**

| 元件 | 位置 | 理由 |
|---|---|---|
| `serve`（含 A2A HTTP listener、A2A cycle） | **host** | 它是控制面。放進容器等於把 47 個 cc- binding 的 supervisor 一起搬進去。 |
| `SandboxExecutor.Start`（`a2a_executor.go:185`） | **host** | 建 worktree、寫政策檔、起沙盒——全部是 host 的動作。 |
| `git worktree add` / `remove`（`worktree.go:345,364`） | **host** | 共用 repo 在 host 上，worktree 建好後才掛進容器。 |
| `RunWorkerOnce`（`worker.go:70`） | **host** | 它只操作 `<sandboxRoot>` 底下的 inbox/outbox 檔案 + 一個 Injector。這兩者跨容器邊界都成立（見第三、四節）。**不動。** |
| `SandboxDriver.loop`（`a2a_driver.go:128`） | **host** | 同上。它讀 pane、判畫面、呼叫 `RunWorkerOnce`。 |
| `CollectResults`（`a2a_result.go:142`）、`SweepTimeouts`、`DrainQueue`、`PruneTasks` | **host** | 全部只碰 host 檔案系統與 docker CLI。 |
| tmux server | **容器內**（每容器一個） | claude 需要 PTY；host 的 tmux server 不再持有任何 `aa-` session。 |
| `claude` 行程 | **容器內** | |
| `claude-cron permission-gate`（PreToolUse hook） | **容器內** | 它是 Claude Code 自己 spawn 的子行程，只能在 claude 跑的地方跑。 |
| Discord sender / activity mirror（`a2a_output.go`、`activity.go`） | **host** | 它讀 transcript JSONL 檔，不需要進容器。 |

**邊界的判準：任何需要看到 `<root>` 整體、`bindings.json`、`callers.json`、`.env`、或 host `~/.claude/` 的東西，一律留在 host。** 這條規則本身就是容器要買的東西。

---

## 二、掛載表（本規格的核心）

**設計主軸：所有跨界的路徑都用「路徑同構」bind mount——host 路徑與容器內路徑逐字相同。**

這一條不是美觀問題，它一次消掉四類改動：

- `BuildClaudePrompt`（`adapters.go:377`）把 `<sandboxRoot>/current_job.json` 與 outbox 的**絕對路徑**寫進 prompt → 不必改。
- `CollectResults` 掃 `<sandboxRoot>/outbox/pending/*.json` → 不必改。
- gate 的 `SandboxSessionFromRegistryRoot`（`a2a_policy.go:161`）從 `CC_REGISTRY_ROOT` 反推 `<root>` 與 session，靠的是「父目錄名為 `sandboxes`」→ 不必改。
- gate 的範圍判定 `sandboxPathInScope` / `filePathOf` 比對的是絕對路徑 → 不必改。

設 `S = aa-<agent>-<ctx>`、`R = /home/conray/project/claude_cron/.channel-agent`、`W = SandboxWorktree(projectDir, S)`、`P = <agent>.ProjectDir`。

| host 路徑 | 容器內路徑 | 模式 | 說明 |
|---|---|---|---|
| `W` | 同左 | `develop`/`full` → **rw**；`readonly` → **ro** | 沙盒的工作目錄。`readonly` 的「不可寫」第一次由核心保證，而不是由 Bash 允許清單近似。 |
| `P/.git` | 同左 | **rw** | `git worktree add` 產生的 `W/.git` 是指標檔（`gitdir: P/.git/worktrees/S`）。沒有這個掛載，容器內連 `git status` 都跑不動。**這是本設計最大的一個洞，見第十五節。** |
| `R/sandboxes/S` | 同左 | **rw** | inbox / outbox / state / locks / logs、`current_job.json`。prompt 進出走這裡。 |
| `R/a2a-policies/S.json` | 同左 | **ro** | 政策檔。**核心強制唯讀** = `develop` 自我提權路徑就此關閉。 |
| `R/a2a-gate-spool/S.jsonl` | `R/a2a-gate.jsonl` | **rw** | 唯一刻意不同構的掛載，見第五節。 |
| `~/.local/share/claude/versions/<ver>` | `/usr/local/bin/claude` | **ro** | 不烘進映像：275 MB × 每個版本，且必須跟 host 同步。 |
| `~/.local/bin/claude-cron-beta` | `/usr/local/bin/claude-cron` | **ro** | `sandboxAgentSettings`（`worktree.go:184`）裡的 hook 指令是裸名 `claude-cron`，靠 PATH 解析。 |

**沒有列在上表的東西，容器內看不到。** 明確不掛：`R` 本身、`bindings.json`、`callers.json`、`agents.json`、`tasks.json`、`.env`、`~/.claude/`、`~/.claude.json`、其他 46 個 binding 的 worktree、其他沙盒、`/var/run/docker.sock`。

環境變數：

```
CC_REGISTRY_ROOT=R/sandboxes/S          # gate 靠它認出自己是沙盒
CLAUDE_CONFIG_DIR=R/sandboxes/S/claude  # 每個沙盒自己的 ~/.claude，不共用
HOME=R/sandboxes/S/home
CLAUDE_CODE_OAUTH_TOKEN=<A2A 專用 token>   # 見第六節
HTTPS_PROXY=http://cc-a2a-egress:3128
HTTP_PROXY=http://cc-a2a-egress:3128
PATH=/usr/local/bin:/usr/bin:/bin
```

`CLAUDE_CONFIG_DIR` 指向沙盒自己的目錄，是一個附帶但重要的收穫：

- 沙盒**不繼承** operator 的 `~/.claude/CLAUDE.md`（全域守則）、`~/.claude/skills/`、plugin、RTK hook。它跑在一個乾淨的 Claude Code 上。
- 沙盒**沒有任何 MCP server 設定**。`full` 等級的 `mcp__*` 放行因此在容器內變成空集合——`a2a_gate.go:108-115` 的那條分支還在，但沒有東西可以呼叫。這是「容器讓一部分 confinement 工作變成多餘」的第一個具體例子。
- `EnsureFolderTrusted`（`a2a_trust.go`）不再寫全機共用的 `~/.claude.json`，改寫沙盒自己的 config dir。confinement 規格 F2 那條「走介面否則單元測試會改到 operator 線上設定」的強制要求，在容器路線下失去存在理由。

容器啟動指令的形狀：

```
docker run -d -t --name cc-a2a-<S> \
  --label cc.a2a.session=<S> --label cc.a2a.started=<RFC3339> \
  --network cc-a2a --user 1000:1000 --init \
  --cap-drop=ALL --security-opt no-new-privileges \
  --memory=2g --memory-swap=2g --pids-limit=512 --cpus=2 \
  <掛載表> <環境變數> \
  cc-a2a-sandbox:<claude 版本> \
  tmux new-session -s <S> env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN claude
```

三個決定，明確記錄：

1. **`--user 1000:1000`**：bind 進來的 worktree 與 sandbox root 屬於 `conray`。容器內用同一個 uid 才寫得動。這代表**容器逃逸之後就是 `conray`**——但逃逸需要 kernel/runc 漏洞，與「改一行 JSON」不是同一個數量級。掛載表才是真正的邊界，不是 uid。
2. **tmux 當 PID 1（`-t` 配 attached `new-session`）**：容器的壽命 = session 的壽命。`Alive` 因此變成 `docker inspect -f '{{.State.Running}}'`，比 `tmux has-session` 更明確。`--init` 負責收殭屍。
3. **不開 `--read-only` 根檔案系統**：claude 會寫快取，逐一挖 tmpfs 例外的成本大於收益；容器每次任務都是全新的，這一層沒必要。

---

## 三、prompt 怎麼進去、結果怎麼出來

**兩者都完全不跨容器邊界——它們走的是 host 檔案系統，而那個目錄剛好也掛在容器裡。**

進去：

1. `SandboxExecutor.Start` → `e.Sessions.Inject(ctx, sandboxRoot, msg)` → `IngestMessages` 在 **host** 寫 `R/sandboxes/S/inbox/pending/<id>.json`。**不變。**
2. host 上的 `SandboxDriver.loop` 呼叫 `RunWorkerOnce(ctx, R/sandboxes/S, inj, timeout)`，它在 **host** 把 job 搬進 `processing/`、寫 `current_job.json`、算出 `outputPath`、組出 prompt。**不變。**
3. 唯一改變的是最後一步：`TmuxInjector.typeAndSubmit`（`adapters.go:184`）目前執行 `tmux send-keys -t S ...`。容器化之後必須是 `docker exec cc-a2a-S tmux send-keys -t S ...`。

出來：

4. 容器內的 claude 依 prompt 把結果寫成 `R/sandboxes/S/outbox/pending/<jobid>.json.tmp` 再 rename——因為路徑同構，它寫的就是 host 上那個檔案。
5. host 的 `CollectResults`（`a2a_result.go:142`）掃到它，用 `resultBelongsToTask`（`:69`，比對 `job.JobID` 是否含 `sanitize(task.LastMessageID)`）認領，轉 `TaskCompleted`，把檔案搬到 `outbox/sent/`。**完全不變。**

**這是容器化最大的運氣：`RunWorkerOnce` 與整套 outbox 檔案約定本來就是「一個目錄 + 一個 injector」的介面，容器只換掉 injector 那一半。** 前一份規格為了別的理由確認過 `RunWorkerOnce` 不綁 binding；同一個性質讓它也不綁 host。

### 需要的抽象：`paneDriver`

`tmux` 在 A2A 路徑上被四個動詞用到，散在三個檔：

| 呼叫點 | 動詞 |
|---|---|
| `adapters.go:184-200` `typeAndSubmit` | `send-keys` |
| `adapters.go:93,139` `Inject`/`submitLine` 驗證 | `capture-pane` |
| `adapters.go:208` `paneBusy`、`:152` `LooksGlitched`、`:164` `SessionWorking` | `capture-pane` |
| `adapters.go:224-287` `PasteLoginCode`/`SendLogin`/`PressEnter`/`SelectTrustSettings` | `send-keys` |
| `supervisor.go:136` `capturePane`（driver 每拍呼叫，`a2a_driver.go:201`） | `capture-pane` |
| `confirm.go:97` `sendConfirmChoice` | `send-keys` |
| `a2a_session.go:75` `TmuxSessionAlive`、`:540` `StopTmuxSession` | `has-session` / `kill-session` |

做法：新增一個只有一個方法的介面

```go
// paneDriver 決定 tmux 指令在哪裡執行。nil = 今天的行為（host tmux），
// cc- 路徑永遠傳 nil。
type paneDriver interface{ argv(tmuxArgs []string) (name string, args []string) }
```

`hostTmux`（`nil` 時的預設）回傳 `("tmux", tmuxArgs)`，與現況逐位元相同；`dockerTmux{container}` 回傳 `("docker", append([]string{"exec", c, "tmux"}, tmuxArgs...))`。`TmuxInjector` 與 `capturePane` / `sendConfirmChoice` 各加一個可為 nil 的欄位/參數。

**這是唯一一處會碰到 cc- 也在用的程式碼的改動**，因此它的驗收條件是：`TestPermissionGateBindingPathUnchanged` 之外，再補一條「nil paneDriver 時產生的 argv 與改動前逐字相同」的測試。

---

## 四、政策檔住在哪、為什麼沙盒改不到

政策檔路徑不變：`R/a2a-policies/S.json`（`a2a_policy.go:94`），由 `SandboxExecutor.Start` 在起容器**之前**寫入（`a2a_executor.go:284`），mode 0600。

改變的只有一件事：**它以 `:ro` bind mount 進容器的同一個路徑。**

後果，逐條：

- `LoadSandboxPolicy`（`a2a_policy.go:111`）在容器內讀得到 → gate 的判定邏輯**一行都不改**。
- 容器內任何 uid、任何工具、任何等級（**含 `full`**）都寫不動它——`EROFS`，核心層拒絕，不經過任何 Go 程式碼。
- confinement 規格第六節「開放問題 5」（`full` 級沙盒的 Bash 可以改寫自己的政策檔，列為已知殘留）**直接消失**。
- 已經證實可行的 `develop` → `full` 自我提權路徑（`python -c "改一行 JSON"`）**直接消失**。
- 撤銷路徑 `RevokeSandboxPolicy`（`a2a_policy.go:126`）在 **host** 執行，寫的是 host 那一份；bind mount 是同一個 inode，容器內下一次 gate 呼叫立刻讀到 `revoked`。**語意不變、速度不變。**
  - 一個實作細節必須注意：`AtomicWriteJSONMode` 走的是 write-temp-then-rename。**rename 會換掉 inode，而單檔 bind mount 綁的是舊 inode**，容器內會繼續看到舊內容。因此撤銷路徑必須改成「掛目錄而不是掛單檔」：掛 `R/a2a-policies/` 整個目錄為 `:ro`。但那樣沙盒就看得到**所有**沙盒的政策檔（含別的 caller id）。
  - **本規格的判斷：掛一個只含這一個 session 的專屬目錄。** `SandboxExecutor.Start` 改寫到 `R/a2a-policies/S/policy.json`，掛 `R/a2a-policies/S/` 為 `:ro`，容器內路徑同構。`PolicyPath` 因此要改（`a2a_policy.go:90`）。這是政策檔唯一被迫的改動，列為第十一節的必改項。

---

## 五、gate 的判定怎麼回到 operator 的 gate log

`AppendGateLog`（`a2a_gate.go:677`）寫的是 `GateLogPath(root)` = `R/a2a-gate.jsonl`。而 `R` 本身**不掛進容器**（那是整份設計的前提）。

做法：**唯一一個刻意不同構的掛載。** host 的 `R/a2a-gate-spool/S.jsonl` 掛在容器內的 `R/a2a-gate.jsonl`。

- 容器內的 gate 以為自己在寫正本，程式碼**一行不改**。
- host 的 A2A cycle 新增一個 `DrainGateSpool(root)` 階段：把每個 spool 檔的內容 append 進真正的 `R/a2a-gate.jsonl`，然後 truncate 該 spool。放在 `CollectResults` 之前。
- 每一批 drain 的行都保留原內容（`GateLogEntry` 已含 `session`），不需要重新標記。

三個必須寫明的性質：

1. **rotation 會壞掉。** `appendRotatingLine` 超過 `AuditMaxBytes = 32 MiB` 時會 `rename` 成 `.1`——對一個單檔 bind mount 而言那個 rename 落在容器內的一個不存在的目錄裡。因為 host 每 10 秒 drain + truncate 一次，spool 永遠到不了 32 MiB，所以實務上打不到；但這是靠時序而不是靠結構撐住的，必須記錄。
2. **這比現況更安全，不是更不安全。** 今天 `develop` 的 Bash 允許 `rm` 且**完全不做路徑侷限**（`a2a_gate.go:228-240` 的註解自己寫明了），所以一個 `develop` 沙盒現在就可以 `rm /home/conray/.../a2a-gate.jsonl` 把自己的判定紀錄整份刪掉。容器化之後它最多只碰得到自己那一個 spool，且 host 每 10 秒就已經把內容搬走了。
3. **同一個沙盒仍可竄改自己那 10 秒的 spool。** 想要連這個都堵住，做法是把 spool 目錄設成 sticky drop-box（`01733`，每次判定寫一個新檔而非 append），代價是每次工具呼叫多一個 inode。**本規格判斷：先不做**，列入開放問題。

---

## 六、認證：需要什麼、代價是什麼、攻擊者拿得到什麼

**這是整份規格的關鍵一節，不迴避。**

### 事實

- 訂閱流程是互動式 PTY 登入，寫出 `~/.claude/.credentials.json`（0600, 523 B）。
- 該檔的 `claudeAiOauth.scopes` 是 **`user:file_upload, user:inference, user:mcp_servers, user:profile, user:sessions:claude_code`**，`subscriptionType = team`。
- **這組 scope 是登入流程發的，operator 沒有任何介面可以把它縮小。** Claude Code 沒有「per-agent、可降權的憑證」這種東西。任何拿到一份有效憑證的實體，就拿到這五個 scope 的全部。
- `.env` 裡的 `CLAUDE_CODE_OAUTH_TOKEN` 目前註解掉；跑的是 credentials.json 模式。`claudeArgs`（`worktree.go:484`）刻意 `env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN`，確保走訂閱而非按量計費。

### 三個選項

**A（否決）：把 host 的 `~/.claude/.credentials.json` 唯讀掛進容器。**
- 代價：沙盒直接持有 operator 的 refresh token。這是最糟的一種——它是**可重用、長效、與 operator 本人共用**的憑證。
- 另有一個實務上的致命傷：access token 會到期並被 refresh 後寫回原檔；唯讀掛載讓寫回失敗。寫回失敗之後 claude 的行為（記憶體內續用，或退回登入畫面）**我沒有實測，不確定**。
- 否決。

**B（本規格採用的基線）：一個 A2A 專用的 `claude setup-token`，以環境變數注入。**
- 產生方式：`claude setup-token`（有效期約一年），存進 `serve` 的 `EnvironmentFile`（`/home/conray/project/claude_cron/.env`）另一個變數名，只注入 `aa-` 容器，**不注入 cc- session**。
- 買到什麼：**可獨立撤銷。** 沙盒外洩的是這一份 token，operator 可以單獨作廢它而不影響自己的日常登入。這是相對於 A 唯一但實在的改善。
- 沒買到什麼：**scope 完全一樣。** 一個拿到這份 token 的攻擊者仍然能對 operator 的 team 訂閱下推論請求、讀 profile、列 sessions，直到被撤銷為止。**沒有任何辦法把它縮到「只准這個沙盒用」。**
- 一個必須標明的不確定：`worktree.go:416-425` 的註解斷言 setup-token 是訂閱 OAuth token、計費留在方案內；使用者記憶檔 `auth-token-strategy` 把「訂閱 vs API 計費」記為**未確認**。動工前應到 console 確認，不要照抄程式碼註解。
- 附帶收穫：容器內**永遠不需要跑 `/login`**。沙盒開機卡在登入畫面這整類問題（driver 的 `loginStrikes` / `markSandboxLoginFailure`，`a2a_driver.go:280-312`）在正常情況下不會再觸發。那段程式碼仍應保留當 token 失效時的偵測。

**C（唯一真正解掉「憑證外洩」的做法，本規格不列入第一階段）：容器內完全不放憑證。**
- 做法：host 上跑一個反向代理，容器設 `ANTHROPIC_BASE_URL` 指向它、`ANTHROPIC_API_KEY` 設成一個 per-sandbox 的隨機字串。代理驗證那個字串、換上真正的 OAuth bearer 送往 `api.anthropic.com`，並負責 refresh。
- 買到什麼：沙盒**永遠拿不到可重用的憑證**。它能燒 operator 的額度，但只在容器活著的期間，且 operator 隨時可以只作廢那個 per-sandbox 字串。這是唯一能回答「compromised sandbox 不會交出 operator 的帳號」的設計。
- 代價：要維護一個懂 Anthropic OAuth（bearer + 相關 beta header）的代理；`claude` 在「base URL 被換掉 + 沒有 OAuth 憑證」的組合下是否正常運作，**我沒有驗證，不確定**；而且計費是否仍算訂閱也未驗證。
- **判斷：列為第二階段的選配，不擋第一階段上線。** 但如果 operator 的接受門檻就是「沙盒不得持有可重用憑證」，那 C 是必要條件，成本要加進去（我估 +3 到 +4 個 task，且含一段不確定的實驗）。

### 一個完全擁有容器的攻擊者，還碰得到什麼

| 碰得到 | 說明 |
|---|---|
| A2A 專用 OAuth token（選項 B 之下） | operator 的 team 訂閱推論 / profile / sessions，直到被撤銷。**無法縮小 scope。** |
| 自己的 worktree | 依等級 rw 或 ro。 |
| 自己的 sandbox root | inbox/outbox/state；含自己的 gate spool。 |
| **`P/.git`（共用 repo 的 git dir）** | 可寫 ref 與 object → **可以動到同一個專案底下所有 cc- binding 共用的分支**。gate 的 `git branch` / `git push` 規則（`a2a_gate.go:465-597`）**仍然是唯一的防線**，容器沒有取代它。 |
| 上游 API（經 egress proxy） | 燒額度。 |
| kernel | 逃逸需要 kernel/runc 漏洞（`--cap-drop=ALL` + `no-new-privileges` + seccomp builtin + apparmor docker-default 之下）。 |

| 碰不到 | 為什麼 |
|---|---|
| `~/.claude/.credentials.json` | 未掛載 |
| `.env`（Discord / Telegram bot token） | 未掛載 |
| `callers.json`（明文 bearer 憑證）、`agents.json`、`tasks.json`、`bindings.json` | 未掛載 |
| 另外 46 個 binding 的 worktree、其他沙盒 | 未掛載 |
| 自己的政策檔（寫入） | `:ro` 掛載，核心拒絕 |
| admin API `127.0.0.1:8787`、A2A `127.0.0.1:8790`、MCP `8001/8003`、SiYuan `6806` | 網路隔離（第七節），且 loopback 不共用 |
| `/var/run/docker.sock` | **永遠不掛。** 掛了等於直接給 host root（rootful daemon）。 |
| MCP server | 沙盒的 `CLAUDE_CONFIG_DIR` 裡沒有任何 MCP 設定 |

---

## 七、網路與出口

需求互相拉扯：容器**必須**連得到 `api.anthropic.com`，同時**絕不能**連到 `fatgame-mysql`（`172.17.0.2:3306`，且對 `0.0.0.0:3306` 發佈）、`siyuan`、host 的 loopback 服務、以及 `192.168.0.0/24` 的辦公室網段。

做法（**不需要 root**）：

1. `docker network create --internal cc-a2a` —— 沙盒容器全部接在這個網路上，它沒有對外 NAT。
2. 一個長駐的 `cc-a2a-egress` 代理容器（tinyproxy 或 squid，只放行 `CONNECT api.anthropic.com:443`），**雙網卡**：接 `cc-a2a`（給沙盒用）+ 接預設 `bridge`（給自己出去用）。
3. 沙盒的 `HTTPS_PROXY` / `HTTP_PROXY` 指向 `http://cc-a2a-egress:3128`。`claude` binary 內含 `HTTPS_PROXY` / `NO_PROXY` 字串，支援這條路。

**一個必須實測才能宣稱的事：`--internal` 網路上的容器是否仍碰得到 host 自己 listen 在 `0.0.0.0` 的埠（尤其是 `3306`）。** Docker 的 `--internal` 加的是 FORWARD 鏈的規則，而送往 host 的流量走 INPUT 鏈——所以**我判斷它很可能仍然通**，但沒有實測，不當作事實。

- 驗證方式：起一個 `cc-a2a` 上的一次性容器，`nc -vz <bridge gateway ip> 3306`。
- 若真的通，備援是一條 `iptables -I INPUT -i br-<cc-a2a-id> -j DROP`（放行已建立連線）。**這需要一次性的 root 動作。** 這與 separate-unix-user 的差別在於：這是**一次性設定**（可寫進一個 systemd unit 或 `/etc/iptables`），不是「serve 每次派工都要提權」的常態性特權。

代價，明確寫出來：

- **`git push` 到 `gitlab.jvdtech.dev` / `git.fatcatbet.net` 會被擋掉**，除非把那些 host 也加進 proxy 的允許清單。這改變交付模型：目前 `develop` 等級允許 `git push`（`a2a_gate.go:480-504`）。**本規格判斷：預設不放行 git remote，交付走「commit 到 `aa/<S>` 分支，host 端的 sweep 之前分支已存在於共用 repo」**——這正是 `BuildClaudePrompt`（`adapters.go:408-411`）已經在教沙盒做的事。要 push 就把 remote 加進允許清單，列為 per-agent 設定。
- `WebFetch` / `WebSearch`（`develop` 與 `full` 放行）在只允許 `api.anthropic.com` 的 proxy 之下**會失效**。`WebSearch` 走的是上游 API（應該還能用），`WebFetch` 直接抓網頁則不行。這是行為改變，必須讓 operator 知道。

---

## 八、agent 頻道的活動鏡像

今天的路徑：`SandboxDriver.loop` → `CollectActivity(sandbox, task.Worktree)`（`a2a_driver.go:366`）→ `AgentChannel.SendLine` → `AgentOutputSink` 的單一 goroutine → `DiscordSender`（含 `discord.go` 的 per-channel 250 ms throttle 與 429 `retry_after` 退避）。

`CollectActivity` **不抓 tmux pane**，它 tail 的是 Claude 的 transcript JSONL（`activity.go:60`），路徑由 `sessionTranscriptPath(bRoot, worktree)` 決定：先看 `<bRoot>/state/session.json` 裡 SessionStart hook 記下的路徑，沒有就退回 `transcriptPath(worktree)` = `os.UserHomeDir()/.claude/projects/<encodeProjectDir(worktree)>/<id>.jsonl`。

**沙盒的 `sandboxAgentSettings`（`worktree.go:184`）刻意拿掉了 SessionStart hook，所以走的一定是退回路徑；而容器內 `CLAUDE_CONFIG_DIR` 已經被指到 `R/sandboxes/S/claude`，transcript 不會出現在 host 的 `~/.claude/projects/`。因此活動鏡像會全面失效。**

修法（小而明確）：新增

```go
// sandboxTranscriptDir 回傳沙盒 transcript 所在目錄。沙盒的 CLAUDE_CONFIG_DIR
// 指向 <sandboxRoot>/claude，不是 operator 的 ~/.claude。
func sandboxTranscriptDir(sandboxRoot, worktree string) string {
    return filepath.Join(sandboxRoot, "claude", "projects", encodeProjectDir(cleanAbs(worktree)))
}
```

並在 A2A 的 `CollectActivity` 呼叫點傳進去（給 `CollectActivity` 加一個「transcript 目錄覆寫」參數，cc- 傳空字串走原路徑）。`RunActivityStreamOnce`（`activity.go:371`，cc- 專用）一行不動。

其餘全部不變：sink、throttle、`SandboxOutputPrefix(contextID)` 的 `[<ctx>]` 標註、單向不 ingest 的性質。

`latestTranscript`（`worktree.go:444`）也用 `os.UserHomeDir()`，它服務的是 `claudeArgs` 的 `--resume`。容器化之後它對沙盒 worktree 一律回空字串 → 沙盒永遠是全新 session、不 `--resume`。**這是可接受的：沙盒本來就是一次性的。** 但要記錄一個行為變化：容器若被重建（不在正常流程內），對話歷史不會續上。

---

## 九、sweep 拆什麼、孤兒容器怎麼收

`SweepTimeouts`（`a2a_lifecycle.go:598`）今天的破壞性動作，在容器路線下逐條對應：

| 現在 | 容器化之後 |
|---|---|
| `stopper.Stop(session)`（停 driver goroutine，阻塞到真的結束） | **不變**（driver 在 host） |
| `sm.Stop(ctx, session)` → `StopTmuxSession` → `tmux kill-session` | → `docker rm -f cc-a2a-<S>`。**三分法必須原樣保留**：docker 明確回報「沒有這個容器」= 成功；「問不到答案」（daemon 沒起來、`docker` 找不到、ctx 取消）必須回非 nil error，否則會在容器還活著的時候刪掉它的 worktree。這正是 `TmuxSessionAlive` / `StopTmuxSession`（`a2a_session.go:75,540`）已經寫了一大段註解在防的那件事。 |
| `sm.RemoveWorkspace` → `git worktree remove --force` | **不變**（host 上的 git） |
| `os.RemoveAll(SandboxRoot(root, session))` | **不變**（host 目錄）。但**必須排在 `docker rm -f` 成功之後**——現行程式碼已經是這個順序（`a2a_lifecycle.go:1140-1145`：`sm.Stop` 失敗就整個 candidate 放棄，什麼都不刪）。這條順序保證在容器路線下更重要：一個還在跑的容器的 bind mount 目標被刪掉，行為未定義。 |
| `RemoveSandboxPolicy(root, session)` | 改成刪整個 `R/a2a-policies/S/` 目錄（第四節的改動） |
| （新增）| 刪 `R/a2a-gate-spool/S.jsonl`，drain 之後 |

`sm.Alive` → `docker inspect -f '{{.State.Running}}' cc-a2a-<S>`，同樣的三分法：`No such object` = 明確不存在；其他錯誤 = 問不到，回 error。`VanishedConfirmStrikes = 2` 的既有機制不動。

**孤兒容器的回收（現行設計完全沒有的東西）。** 今天有一條已知的孤兒路徑（confinement 規格的 C1：同 contextId 換 agent），而 `sandboxes/` 目錄沒有任何東西在掃。容器讓這件事第一次變得可以徹底解決，因為容器有 label：

sweep 新增第四步 `ReapOrphanContainers(ctx, root)`：

1. `docker ps -aq --filter label=cc.a2a.session` 列出所有本系統起的容器。
2. 對每個容器讀 `cc.a2a.session` label，查 `tasks.json` 有沒有一列 `Session == <label>` 且非終止狀態。
3. 沒有 → 這是孤兒 → `docker rm -f`。
4. **加一條 grace**：`cc.a2a.started` label 距今 < 5 分鐘的一律跳過。理由與 `DispatchStaleAfter` 相同——`Start` 在容器起來之後、`persist(TaskWorking)` 之前有一段窗口，這段時間內容器合法地「沒有對應的 working row」。

這一步同時是「serve 重啟後」的收斂機制：容器是 host 上的持久物件，serve 掛掉不會帶走它們，重啟後這一掃就能把不再有主的容器清乾淨。今天的 tmux session 沒有這個性質（`EnsureSandboxDrivers` 只停 goroutine，不停 tmux）。

**失敗沙盒的 forensics 規則不受影響**：worktree 是 host bind mount，容器刪掉之後它原封不動地留在磁碟上。`MaxRetainedFailedSandboxes = 20` 的上限照舊。**因此失敗沙盒的容器一律刪除，不保留**——保留一個死掉的容器只是佔記憶體，證據本來就在 worktree 裡。

---

## 十、映像：內容、建置、與 host 版本同步

`docker/a2a-sandbox/Dockerfile`：

```dockerfile
FROM ubuntu:26.04            # 必須 glibc 2.43+；alpine (musl) 跑不動 claude
RUN apt-get update && apt-get install -y --no-install-recommends \
      tmux git ca-certificates ripgrep jq less procps \
 && rm -rf /var/lib/apt/lists/*
RUN useradd -u 1000 -m -s /bin/bash agent   # 與 host conray 的 uid 對齊
USER 1000:1000
```

**映像裡刻意沒有 `claude`、沒有 `claude-cron`。** 兩者都以 `:ro` bind mount 從 host 帶進去（第二節）。這樣做的三個理由：

1. **版本同步變成不可能出錯的事。** host 換 claude 版本（`~/.local/bin/claude` 的 symlink 換指向）之後，下一個起來的沙盒自動用新版——不需要重建映像、不需要記得重建、不會漂移。這正是問題 6「怎麼跟 host 的 Claude Code 版本保持一致」最乾淨的答案。
2. 275 MB 的 binary 不進映像層，映像本身小（估 ~150 MB）。host 現在同時留著 2.1.221 / 2.1.222 / 2.1.223 三個版本，烘進去等於每個版本一份。
3. `claude-cron` 同理：gate 的行為必須跟 host 上跑的 `serve` 是同一份 build，掛載保證這件事。

建置：`Makefile` 新增 `make a2a-image`，跑 `docker build --platform linux/arm64 -t cc-a2a-sandbox:1 docker/a2a-sandbox`。映像 tag 是**映像自己的版本號**（作業系統 + 工具集），不是 claude 的版本號——claude 版本已經由掛載決定。

`serve` 啟動時（`cfg.A2A.Isolation == "container"`）做一次 `docker image inspect cc-a2a-sandbox:1`；不存在就記一行明確錯誤並**拒絕啟用容器隔離**（fall back 到停用 A2A，不是 fall back 到 tmux——後者會靜默地回到有已知提權洞的模式）。

一個 apt 依賴的判斷：`ripgrep` / `jq` / `less` 裝進去，因為 Claude Code 的 `Grep` 工具與分頁需要它們；`python3` / `node` / `go` **不裝**。這代表 `develop` 等級在容器內跑不動 `pytest` / `npm test` / `go test`——**這是一個真實的功能倒退**，見第十五節。要跑測試的 agent 需要一個自己的映像（`agents.json` 加一個 `image` 欄位），這是容器路線的正常做法，但也是額外工作。

---

## 十一、程式碼影響：改什麼、刪什麼、不動什麼

### 必改（新檔）

| 檔案 | 內容 |
|---|---|
| `internal/channelagent/a2a_container.go`（新） | `ContainerSessionManager`（實作既有的 `SessionManager` 介面）、掛載表組裝、`docker run/rm/inspect/exec` 的封裝與三分法錯誤處理 |
| `internal/channelagent/a2a_pane.go`（新） | `paneDriver` 介面、`hostTmux`、`dockerTmux` |
| `internal/channelagent/a2a_gatespool.go`（新） | `DrainGateSpool(root)` |
| `internal/channelagent/a2a_reap.go`（新） | `ReapOrphanContainers(ctx, root)` |
| `docker/a2a-sandbox/Dockerfile`（新） | 第十節 |
| `scripts/a2a-net-up.sh`（新） | 建 `cc-a2a` internal network + `cc-a2a-egress` 代理容器（冪等） |

### 必改（既有檔）

| 檔案:位置 | 改法 |
|---|---|
| `adapters.go:39` `TmuxInjector` | 加一個可為 nil 的 `Pane paneDriver` 欄位；七個 `runExternalCommand(ctx,"tmux",…)` 呼叫點改走它。nil 時 argv 逐字不變。 |
| `supervisor.go:136` `capturePane` | 加一個 `paneDriver` 參數（cc- 傳 nil） |
| `confirm.go:97` `sendConfirmChoice` | 同上 |
| `a2a_driver.go:201,256,271` | 傳 `dockerTmux{container}` |
| `a2a_session.go:14` `SessionManager` 介面 | **不動**（六個方法對容器一樣成立）。`FakeSessionManager` 也不動。 |
| `a2a_policy.go:90` `PolicyPath` | `R/a2a-policies/S/policy.json`（第四節的 rename/inode 問題） |
| `activity.go:60` `CollectActivity` | 加 transcript 目錄覆寫參數；cc- 傳空字串 |
| `a2a_cycle.go:32` `RunA2ACycleOnce` | 新增 `DrainGateSpool`（排在 `CollectResults` 之前）與 `ReapOrphanContainers`（排在 `SweepTimeouts` 之後） |
| `config.go:113` `A2AConfig` | 新增 `Isolation string`（`"tmux"` \| `"container"`，預設 `"tmux"`）與 `SandboxImage string` |
| `cmd/claude-cron/main.go:222,310` | 依 `cfg.A2A.Isolation` 選 `TmuxSessionManager{}` 或 `ContainerSessionManager{…}` |
| `cmd/claude-cron/a2a_cmd.go` + `admin` a2a 路由 | `agent add` 加 `--image`；task list 顯示容器名 |
| `web/admin/src/Agents.svelte` | 顯示隔離模式與容器狀態（小改） |

### 可刪（容器落地之後變成多餘的 confinement 工作）

| 對象 | 為什麼多餘 |
|---|---|
| `a2a_gate.go` 的旗標允許清單全家：`flagPolicy`、`flagTokenAllowed`、`firstDeniedFlag`、`readonlyHeadFlags`、`readonlyFindTokens`、`findDecision`、`readonlyGitSubFlags`（約 250 行） | 它們存在的唯一理由是「Bash 判定做不到路徑侷限」（`a2a_gate.go:228-240` 自己這樣寫）。掛載表把路徑侷限交給核心之後，`readonly` 只要把 worktree 掛 `:ro` 就成立，不必再逐一猜 `rg --pre` / `find -delete` / `git diff --output` 下一個沒想到的旗標。 |
| confinement 開放問題 5（`full` 可改寫自己的政策檔） | `:ro` 掛載 |
| confinement F2 的「`EnsureFolderTrusted` 必須走介面否則測試會改到 operator 的 `~/.claude.json`」 | 每個沙盒有自己的 `CLAUDE_CONFIG_DIR` |
| `a2a_gate.go:108-115` 的 `mcp__*` 分支的實際效力 | 容器內沒有 MCP 設定；分支保留當縱深防禦，但不再是唯一防線 |

**注意：`bashDecision` 的首 token 允許清單、`gitDecision` 的子命令與 `pushArgsAllowed` 全部要留著。** 共用 `.git` 是 rw 掛載（第二節），gate 的 git 規則仍是唯一擋住「動到 47 個 binding 共用分支」的東西。**容器沒有取代它。**

### 一行都不動

`bindings.json`、`registry.go`、`supervisor.go` 的 binding 迴圈、`reap.go`、`permission.go` 的 cc- 分支（`:274` 起）、`agentSettings` / `controlAgentSettings` / `EnsureAgentSettings` / `EnsureControlSettings`（`worktree.go:119,150,275,279`）、`StartTmuxClaude`（`:381`）、`StartControlSession`（`:501`）、`classifyScreen`（`screen.go:32`）、`RunActivityStreamOnce`（`activity.go:371`）、`RunWorkerOnce`（`worker.go:70`）、`CollectResults`（`a2a_result.go:142`）、`BuildClaudePrompt`（`adapters.go:377`）、`IngestMessages`、`Init`。

---

## 十二、47 個 cc- binding 必須被隔離於什麼之外（硬性約束，不是假設）

1. **cc- session 全部留在 host 的 tmux server。** 沒有任何 cc- binding 會被容器化。
2. **`paneDriver` 的 nil 路徑必須產生與今天逐字相同的 argv。** 這是唯一一處 cc- 也在跑的共用程式碼被碰到的地方，驗收條件寫在第三節。
3. **docker daemon 死掉不得影響任何 cc- binding。** cc- 完全不經過 docker；A2A 的 `Stop`/`Alive` 在「問不到答案」時回 error，sweep 因此什麼都不拆（既有的三分法保護）。
4. **記憶體必須留給 cc-。** 可用記憶體只有 5.2 GiB，單一 claude 行程 430–765 MB。`MaxConcurrentSandboxes = 8`（`a2a_lifecycle.go:27`）在容器模式下**必須降到 4**，配合 `--memory=2g` 的硬上限——容器至少讓超量的沙盒被 OOM killer 針對性殺掉，而不是像 2026-07-29 那次一樣讓整台機器 OOM 重開。
5. **`a2a.enabled` 與 `a2a.isolation` 兩層 kill switch 都必須維持「關掉時 `serve` 行為逐位元不變」。** 前者已由三份 review 各自確認過（`main.go:218` 與 `:302`）。
6. **不啟用 dockerd 的 userns-remap。** 那是 daemon 全域設定，會一併改變已在跑的 `siyuan` 與 `fatgame-mysql` 的 uid 對應，可能弄壞它們的資料目錄權限。明確非目標。
7. **`/var/run/docker.sock` 永遠不掛進沙盒。** rootful daemon 之下那等於直接給 host root。

---

## 十三、遷移路徑與 kill switch

**兩種沙盒可以共存。** `SessionManager` 是既有介面（`a2a_session.go:14`），`TmuxSessionManager` 與 `ContainerSessionManager` 是兩個實作，選擇點只有 `main.go` 兩處。

分三段：

1. **全域旗標，預設關。** `a2a.isolation = "tmux"`（預設，今天的行為）/ `"container"`。`serve` 啟動時若是 `container` 但映像或網路不存在 → 印明確錯誤並**停用整個 A2A**。
2. **per-agent 覆寫。** `agents.json` 加 `isolation` 與 `image` 欄位（空 = 跟全域）。這讓 operator 可以先只把一個測試 agent 切成容器，其餘維持 tmux，同一個 `serve` 內並行。**任務列必須記下它是用哪一種起的**（`A2ATask` 加 `Isolation` 欄位），否則 sweep 不知道該 `tmux kill-session` 還是 `docker rm -f`——這是共存唯一的真實成本。
3. **收尾。** 確認容器路線穩定後，把 `tmux` 模式標為 deprecated；`readonly` 的 worktree 改成 `:ro` 掛載並刪掉旗標允許清單（第十一節）。這一步之前不要刪任何 gate 程式碼。

**Kill switch**：把 `a2a.isolation` 改回 `"tmux"` 並重啟 `serve`。已經在跑的容器不會被新設定認領——它們由 `ReapOrphanContainers` 依 label 清掉，不需要人工介入。最極端的情況仍是既有那一個：`a2a.enabled = false`。

---

## 十四、成本估計，以及這比 separate-unix-user 買到什麼

### Task 數（誠實估計）

| # | 內容 |
|---|---|
| 1 | Dockerfile + `make a2a-image` + 映像存在性檢查 |
| 2 | `scripts/a2a-net-up.sh`：`cc-a2a` internal network + `cc-a2a-egress` 代理；**含第七節那條 host-published port 的實測** |
| 3 | `paneDriver` 抽象 + 三個既有呼叫點改造 + cc- argv 逐字不變的回歸測試 |
| 4 | `ContainerSessionManager`：`Start`/`Stop`/`Alive`/`RemoveWorkspace`/`TrustFolder`，含 docker 錯誤的三分法 |
| 5 | 掛載表組裝 + 路徑同構的不變量測試（政策檔 `:ro`、`R` 不可見） |
| 6 | `PolicyPath` 改成 per-session 目錄（rename/inode 問題）+ 撤銷路徑驗證 |
| 7 | gate spool：容器內路徑映射 + `DrainGateSpool` + cycle 接線 |
| 8 | 認證：A2A 專用 setup-token、env 注入、`CLAUDE_CONFIG_DIR` 佈局、首次開機不跳登入的實跑驗證 |
| 9 | transcript 路徑 + 活動鏡像修復 + 8 個沙盒併發到 agent 頻道的 throttle 驗證 |
| 10 | `ReapOrphanContainers` + sweep 的容器拆除 + grace window |
| 11 | `A2AConfig.Isolation` / `agents.json.isolation` / `A2ATask.Isolation` + CLI + admin + Svelte |
| 12 | 端到端實跑：一個 scratch repo、一個 `readonly` 委派 + 一個 `develop` 委派，逐條驗證第六節的「碰不到」清單 |
| 13 | 併發上限降到 4 + 記憶體實測 |

**13 個 task。** 我的誠實區間是 **13–16**：前三輪 A2A 每一輪都被 review 判過 DO NOT SHIP，而且這套東西到今天為止**一次都沒有真的端到端跑過**。容器會把第一次跑通的難度往上推（多一層網路、多一層檔案系統、多一層行程樹），我預期 task 12 自己就會分裂成 2–3 個。

不含選項 C（host 端注入認證的代理）的 +3～4 個 task。

### 這比 separate-unix-user 買到什麼

| 面向 | separate-unix-user | 容器 |
|---|---|---|
| 需要的 host 特權 | **sudoers 或 setuid**（常態性） | **零**（`docker` 群組已在手）；可能一條一次性 iptables 規則 |
| 政策檔不可寫 | 靠檔案 owner/mode（有效） | 靠 `:ro` 掛載（有效，且核心層） |
| 檔案可見性 | 靠 unix 權限逐一設對——而 47 個 binding 的 worktree、`.env`、`~/.claude/` 目前**全都是同一個 uid 可讀**，要一一收緊 | 靠掛載表**列舉**——沒列出來的就不存在，預設拒絕而非預設可見 |
| `Read`/`Glob`/`Grep` 能讀全機 | 仍然可以讀所有 `conray` 可讀的檔（confinement 規格明列的已知限制） | **關掉**：只看得到掛載表裡的東西 |
| 網路 | 不受限 | 可用 egress allowlist 收束 |
| DB / Docker / cache 共用（2026-08-05 規格第 85 行列的已知限制） | 仍共用 | 可切斷（`fatgame-mysql` 碰不到，待第七節實測確認） |
| 記憶體暴走 | 不受限 | `--memory` 硬上限，OOM 針對性 |
| 額外運維物件 | 一個 unix 使用者 | 一個映像、一個網路、一個代理容器、一套孤兒回收 |
| 憑證外洩的後果 | 一樣（選項 B 之下兩者相同） | 一樣 |

**最大的差別是特權方向**：separate-unix-user 是**加特權**（給 serve 一條切換使用者的路），容器是**減可見面**（掛載表列舉）。前者的成本是永久的攻擊面擴大，後者的成本是永久的運維複雜度。

---

## 十五、明確不做，與已接受的限制

### 明確不做

- **不改任何 cc- 機制**（第十二節列舉）。
- **不啟用 dockerd userns-remap**（會影響已在跑的 `siyuan` 與 `fatgame-mysql`）。
- **不使用 rootless docker**（現有兩個容器跑在 rootful daemon 上，遷移它們不在本規格範圍）。
- **不掛 `/var/run/docker.sock`。**
- **不做 K8s / podman / gVisor / Firecracker。** 這台機器有 Docker、operator 有權限、其他方案都要新裝東西。
- **不在第一階段做「host 端注入認證的代理」（第六節選項 C）。**
- **不為每個 agent 客製映像**（第一階段只有一個共用映像）。
- **不做自動重試**（沿用 `2026-08-05:122`）。

### 已接受的限制（每一條都是真實的功能倒退或殘留風險）

1. **共用 repo 的 `.git` 是 rw 掛載。** 沙盒因此仍碰得到同一專案下所有 cc- binding 共用的分支。gate 的 `git branch` / `git push` 規則仍是唯一防線。要根除只能改成 per-sandbox `git clone --local`（物件 hardlink，另一個 `.git`），但那會改變交付模型（分支不再直接落在共用 repo），成本我估 +2～3 個 task。**列為開放問題 1。**
2. **`develop` 在共用映像裡跑不動 `pytest` / `npm test` / `go test`**（映像不裝 python/node/go）。今天的 tmux 沙盒可以。這是明確的功能倒退，解法是 per-agent 映像。
3. **`WebFetch` 在 egress allowlist 之下失效**；`git push` 到外部 remote 需要逐一放行。
4. **沙盒不繼承 operator 的 skills / CLAUDE.md / MCP。** 多半是好事，但是行為改變。
5. **憑證的 scope 無法縮小。** 選項 B 買到的是「可獨立撤銷」，不是「權限更小」。
6. **容器逃逸 = `conray`。** `--user 1000:1000` 是 bind mount 可寫性的直接後果。
7. **gate spool 在被 drain 之前（最長一個 cycle，10 秒）可被同一個沙盒竄改。**
8. **`--internal` 網路是否擋得住 host 自己的 `0.0.0.0:3306`，尚未實測。** 若擋不住，需要一次性 root 動作。
9. **併發上限從 8 降到 4**（記憶體）。
10. **多一組運維物件**：映像要重建、網路與代理容器要在開機時存在（需要一個 systemd `--user` unit 或在 `boot-claude-cron.sh` 裡冪等建立）。

---

## 十六、開放問題

前六題我已經下了判斷並寫進上文；列在這裡是為了讓 operator 知道那是我的判斷而不是既有事實。

1. **共用 `.git` rw 掛載 vs per-sandbox `git clone --local`。** 我暫定前者（第一階段），因為後者改變交付模型且成本明確。**這是本規格最大的殘留洞**，如果 operator 的接受門檻是「沙盒不得碰到共用 repo」，那必須改成後者，成本 +2～3 task。
2. **憑證：選項 B（專用 setup-token）還是選項 C（host 端注入的代理）。** 我暫定 B 進第一階段、C 列選配。如果門檻是「沙盒不得持有可重用憑證」，C 是必要條件，+3～4 task 且含一段不確定的實驗。
3. **`setup-token` 的計費歸屬（訂閱 vs 按量）。** 程式碼註解（`worktree.go:416-425`）斷言是訂閱；記憶檔記為未確認。**動工前必須到 console 查證**，我沒有查證。
4. **egress allowlist 的內容。** 我暫定只放 `api.anthropic.com`。要不要加 `gitlab.jvdtech.dev` / `git.fatcatbet.net`（讓 `git push` 能用）、`registry.npmjs.org` / `proxy.golang.org`（讓 `develop` 能裝依賴）是 operator 的取捨——每加一個都是一條出口。
5. **共用映像 vs per-agent 映像。** 我暫定第一階段共用，代價是限制 2。
6. **併發上限 4 還是 8。** 我暫定 4（記憶體實測）。8 個 × `--memory=2g` 遠超可用的 5.2 GiB。
7. **gate spool 的 drop-box 強化**（`01733` + 每次判定一個新檔）。我暫定不做。
8. **`readonly` 的 worktree 掛 `:ro` 之後，Claude Code 本身是否還能正常運作**（它會不會嘗試在 cwd 寫暫存檔）。未實測。若不行，`readonly` 就得退回靠 gate 判定，第十一節「可刪」那一段的價值折半。
9. **要不要在容器落地後真的刪掉旗標允許清單。** 刪了會讓 `tmux` 模式（若還留著）比現在弱。我暫定「兩種模式並存期間不刪，第 3 段收尾才刪」。
10. **`cc-a2a-egress` 代理容器掛掉時的行為。** 我暫定：沙盒的 API 呼叫失敗 → driver 連續 3 次 `RunWorkerOnce` 失敗 → 存活檢查 → 任務標 failed。但這會把一次代理重啟變成一批任務失敗。是否要加一個「代理不健康時暫停 `DrainQueue`」的閘，未決定。
