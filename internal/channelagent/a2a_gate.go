package channelagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gateDecision 是沙盒分支的判定結果。outcome 是寫進 a2a-gate.jsonl 的機器可讀
// 代碼，reason 是回給 Claude Code 的人類可讀理由。
type gateDecision struct {
	allow   bool
	outcome string
	reason  string
}

func gateAllow(reason string) gateDecision         { return gateDecision{true, "allowed", reason} }
func gateDeny(outcome, reason string) gateDecision { return gateDecision{false, outcome, reason} }

// runSandboxGate 是 aa- 沙盒的完整權限判定。它與 cc- 分支唯一的共同點是輸出
// 格式：沒有 pending 檔、沒有頻道詢問、沒有等待，立刻回答。沙盒沒有人可以
// 問，而「執行當下不再詢問」正是 2026-08-05 規格第 58 行的原話。
//
// 沒有任何路徑會 fail-open：判定不出「允許」就是拒絕。
func runSandboxGate(root, session string, hi hookInput, out io.Writer) error {
	pol, dec := sandboxDecision(root, session, hi)
	if err := AppendGateLog(root, GateLogEntry{
		At:        time.Now().UTC().Format(time.RFC3339),
		Session:   session,
		ContextID: pol.ContextID,
		CallerID:  pol.CallerID,
		Agent:     pol.Agent,
		Level:     string(pol.Level),
		Tool:      hi.ToolName,
		Outcome:   dec.outcome,
		Detail:    gateDetail(hi),
	}); err != nil {
		// 判定照舊生效，另寫一行 stderr。不因為記錄失敗而改判 —— 磁碟抖一下
		// 不該讓一個任務卡死。代價是磁碟滿的情況下會有未留紀錄的放行，這是
		// 規格第六節開放問題 6 明列的取捨。
		fmt.Fprintf(os.Stderr, "a2a gate: 寫入 gate log 失敗（判定照舊生效）: %v\n", err)
	}
	fmt.Fprint(out, hookDecisionJSON(dec.allow, dec.reason))
	return nil
}

// gateDetail 取一段短的、可讀的工具輸入摘要。gate log 每秒可能有數十行，
// 不能讓單行無限長（那正是 ReadAudit 整份讀不出來的成因）。
func gateDetail(hi hookInput) string {
	s := summarizeToolInput(hi.ToolName, hi.ToolInput)
	if r := []rune(s); len(r) > 512 {
		return string(r[:512]) + "…（截斷）"
	}
	return s
}

// sandboxDecision 依規格 §3.3 的順序判定。任何非預期的 panic 都被 recover 成
// 拒絕：一個判定不出結果的 gate 必須擋下工具呼叫，不是放它過去。
func sandboxDecision(root, session string, hi hookInput) (pol SandboxPolicy, dec gateDecision) {
	defer func() {
		if r := recover(); r != nil {
			dec = gateDeny("denied_internal", "a2a gate: internal error")
		}
	}()

	pol, err := LoadSandboxPolicy(root, session)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pol, gateDeny("denied_no_policy", "a2a gate: 沒有 "+session+" 的政策檔")
		}
		return pol, gateDeny("denied_bad_policy", "a2a gate: 政策檔無法解讀")
	}
	if pol.Level == GrantRevoked {
		return pol, gateDeny("denied_revoked", "a2a gate: 呼叫方已被撤銷")
	}
	// Worktree 為空時視同壞政策：inScope("", path) 的語意（永遠 false）不可以
	// 被當成「範圍檢查有做」來信任。
	if !ValidGrantLevel(pol.Level) || pol.Worktree == "" || pol.SandboxRoot == "" {
		return pol, gateDeny("denied_bad_policy", "a2a gate: 政策檔無法解讀")
	}
	// 自我一致性檢查（review Minor 1）：政策檔內的 session 與 sandbox_root
	// 必須跟被查詢的 session 算出來的值完全一致。gate 從檔名（session 參數）
	// 決定要套用哪份政策，但檔案內容本身可能寫錯——一份意外寫錯 session 名
	// 字或 sandbox_root 的政策檔，不該被信任成「範圍檢查已經做過」而悄悄把
	// 授權套用到別的沙盒身上。
	if pol.Session != session || pol.SandboxRoot != SandboxRoot(root, session) {
		return pol, gateDeny("denied_bad_policy", "a2a gate: 政策檔與 session 不一致")
	}

	switch hi.ToolName {
	case "Edit", "Write", "NotebookEdit":
		return pol, editDecision(pol, hi)
	case "Bash":
		return pol, bashDecision(pol.Level, bashCommand(hi.ToolInput))
	case "WebFetch", "WebSearch":
		if pol.Level == GrantReadOnly {
			return pol, gateDeny("denied_level", "a2a gate: readonly 不允許對外取用")
		}
		return pol, gateAllow("a2a gate: 等級 " + string(pol.Level) + " 允許 " + hi.ToolName)
	}
	if strings.HasPrefix(hi.ToolName, "mcp__") {
		// MCP server 直通 production（planetscale、openobserve、Atlassian、
		// Slack），一個被委派的開發任務不該無聲碰到它們。
		if pol.Level != GrantFull {
			return pol, gateDeny("denied_mcp", "a2a gate: 只有 full 等級可以呼叫 MCP")
		}
		return pol, gateAllow("a2a gate: full 等級允許 MCP")
	}
	// hook 只裝六個 matcher，這是縱深防禦：任何沒被上面命中的工具名一律拒絕。
	return pol, gateDeny("denied_unknown_tool", "a2a gate: 未知工具 "+hi.ToolName+"，預設拒絕")
}

// editDecision 執行那條唯一不可設定、三級皆適用的規則：沙盒只能寫在自己的
// worktree ∪ 自己 sandboxes/<session>/outbox/ 內，之外一律拒絕（含 full）。
//
// outbox 這個例外是必要的，不是放寬：注入的 prompt 指示沙盒把結果寫成 .tmp
// 再 rename 進 outbox（BuildClaudePrompt，adapters.go），CollectResults 靠它
// 判定完成。沒有這個例外，沒有任何任務能夠完成。cc- gate 有一模一樣的例外，
// 理由也一模一樣（permission.go:283-297 的註解）。
//
// session scratchpad（cc- gate 的第三個放行區）刻意不放行：沙盒沒有寫草稿給
// 人看的需求（規格第六節開放問題 2）。
func editDecision(pol SandboxPolicy, hi hookInput) gateDecision {
	path := filePathOf(hi.ToolInput)
	if path == "" {
		return gateDeny("denied_no_path", "a2a gate: 工具輸入沒有 file_path，無法判斷範圍")
	}
	// Review Critical 3：worktree 範圍內還有兩個地方寫了等於繞過閘門本身，
	// 不管等級、不管是否落在 outbox，一律先擋。
	if sandboxGateConfigPath(pol.Worktree, path) {
		return gateDeny("denied_gate_config", "a2a gate: 不允許改寫沙盒自己的閘門設定（.claude/settings.local.json 或 .git）")
	}
	inOutbox := sandboxPathInScope(filepath.Join(pol.SandboxRoot, "outbox"), path)
	if !sandboxPathInScope(pol.Worktree, path) && !inOutbox {
		return gateDeny("denied_out_of_scope", "a2a gate: 寫入超出 worktree 範圍")
	}
	if pol.Level == GrantReadOnly && !inOutbox {
		return gateDeny("denied_level", "a2a gate: readonly 不允許寫入")
	}
	return gateAllow("a2a gate: worktree/outbox 內的寫入")
}

// resolveExistingPrefix 盡量把 path 換成檔案系統實際落地的絕對路徑：從最深
// 的「已存在」祖先開始解析 symlink，還不存在的尾段原樣接回去——Write 建新
// 檔時最後一段幾乎必然不存在，EvalSymlinks 對不存在的路徑會直接回錯。任何
// 一層解析失敗（權限、循環連結、中間節點根本不是目錄）就把那一層當終點，
// 不再往上找：寧可少解析一層讓比對偏嚴格（更容易判成「不在範圍」），也不要
// 偏寬鬆。
func resolveExistingPrefix(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveExistingPrefix(parent), filepath.Base(p))
}

// sandboxPathInScope 是沙盒專用的範圍比對：在 inScope 的字面前綴比對之上，
// 多解析一次 symlink（review「Important」發現：<worktree>/esc -> / 之後
// Write <worktree>/esc/etc/... 會通過純字面比對，實際落地卻在 worktree 之
// 外）。這個函式只給沙盒分支用，刻意不改 inScope 本身——cc- 的每一個判定都
// 被 TestPermissionGateBindingPathUnchanged 逐字鎖住，共用 helper 的任何行
// 為變動都會被那條測試打回來，也不應該打回來：cc- 的既有 binding 不是這輪
// 要修的風險面。
//
// root 或 path 是空字串、或解析不出實際路徑時一律回 false：範圍比對不出來
// 就是不在範圍內，不是預設放行。
func sandboxPathInScope(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rr := resolveExistingPrefix(cleanAbs(root))
	rp := resolveExistingPrefix(cleanAbs(path))
	return rp == rr || strings.HasPrefix(rp, rr+string(filepath.Separator))
}

// sandboxGateConfigPath 認出「沙盒自己的閘門機關」：改寫它們不是一般的
// worktree 內寫入，是繞過閘門本身（review Critical 3）。
//
//   - <worktree>/.claude/settings.local.json：EnsureAgentSettings
//     （worktree.go）寫入的就是這次 tmux session 的 PreToolUse hook 設定；
//     writeAgentSettings 只在檔案不存在時才寫（worktree.go:176），所以沙盒
//     把內容改掉之後不會被復原，破壞會撐過 session 重啟。
//   - <worktree>/.git：EnsureWorktree 建立的是 `git worktree add` checkout，
//     這裡的 .git 不是目錄，是一個指向真正 git dir 的指標檔
//     （內容是 "gitdir: <路徑>"，已用真的 worktree 驗證過）。改寫這個檔案
//     可以讓後續任何被允許的 git 指令（含 readonly 的 git status、develop
//     的 git commit/push）改去讀一個攻擊者控制的 git dir——自己的 config、
//     自己的 hooks，效果等同真的能寫 .git/hooks，只是路徑長得不一樣。這裡
//     同時擋 .git 本身與任何 .git/... 前綴，不論解析出來是檔案還是目錄，
//     避免依賴「.git 目前是檔案」這個實作細節。
func sandboxGateConfigPath(worktree, path string) bool {
	if worktree == "" || path == "" {
		return false
	}
	rp := resolveExistingPrefix(cleanAbs(path))
	wt := cleanAbs(worktree)

	settings := resolveExistingPrefix(filepath.Join(wt, ".claude", "settings.local.json"))
	if rp == settings {
		return true
	}
	dotGit := resolveExistingPrefix(filepath.Join(wt, ".git"))
	return rp == dotGit || strings.HasPrefix(rp, dotGit+string(filepath.Separator))
}

// bashMetaChars 是 readonly / develop 一律拒絕的 shell metacharacter。
//
// Bash 的判定只能做到「首個 token 的允許清單 + 禁止 metacharacter」。這
// **不保證路徑侷限在 worktree 內** —— `rm -rf /home/conray/project/x` 的首
// token 是允許的 rm。真正的路徑侷限需要容器層隔離，本輪不做（規格第五節）。
// 引號也不解析：禁掉 metacharacter 之後，一個能繞過首 token 檢查的引號組合
// 就不存在了，這是這條規則全部的保證。
//
// review Critical 1：原始清單漏了單獨的 "&"（background 運算子）——
// `ls & rm -rf /home/conray/project` 的首 token 是允許的 ls，指令裡沒有
// `&&`，就這樣繞過了整條規則。"&" 是 "&&" 的超集（Contains 對 "&&" 一樣會
// 命中），加了它之後 "&&" 這條可以留著當文件用，不影響判定。
var bashMetaChars = []string{";", "&", "&&", "||", "|", "`", "$(", ">", ">>", "<", "\n"}

var readonlyBashHeads = map[string]bool{
	"git": true, "ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"find": true, "rg": true, "grep": true, "file": true, "stat": true,
	"du": true, "tree": true,
}

var developBashHeads = map[string]bool{
	"go": true, "make": true, "npm": true, "pnpm": true, "yarn": true, "node": true,
	"bundle": true, "rake": true, "rspec": true, "pytest": true, "python": true,
	"python3": true, "cargo": true, "mkdir": true, "cp": true, "mv": true,
	"rm": true, "touch": true, "chmod": true, "sed": true, "awk": true,
	"sort": true, "uniq": true, "diff": true, "patch": true, "test": true,
}

var readonlyGitSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "branch": true,
	"remote": true, "describe": true, "blame": true, "ls-files": true, "rev-parse": true,
}

var developGitSubs = map[string]bool{
	"add": true, "commit": true, "checkout": true, "switch": true, "restore": true,
	"stash": true, "fetch": true, "merge": true, "rebase": true, "push": true,
}

func bashDecision(level GrantLevel, cmd string) gateDecision {
	if level == GrantFull {
		// full 等同把主機交出去（Bash 無限制 = 能讀憑證、能 curl | sh）。
		// 只授予信任程度等同 operator 本人的呼叫方。
		return gateAllow("a2a gate: full 等級允許任意 Bash")
	}
	if strings.TrimSpace(cmd) == "" {
		return gateDeny("denied_bash_rule", "a2a gate: 空的 Bash 指令")
	}
	for _, m := range bashMetaChars {
		if strings.Contains(cmd, m) {
			return gateDeny("denied_bash_rule", "a2a gate: 指令含 shell metacharacter "+m)
		}
	}
	fields := strings.Fields(cmd)
	head := fields[0]
	if head == "sudo" {
		return gateDeny("denied_bash_rule", "a2a gate: 一律不允許 sudo")
	}
	allowed := readonlyBashHeads[head]
	if !allowed && level == GrantDevelop {
		allowed = developBashHeads[head]
	}
	if !allowed {
		return gateDeny("denied_bash_rule", "a2a gate: 等級 "+string(level)+" 不允許指令 "+head)
	}
	if head == "git" {
		return gitDecision(level, fields)
	}
	// review Critical 2：find 在允許清單內，但 -exec/-execdir/-ok/-okdir/
	// -fprintf 可以讓 find 自己啟動任意程式，且用 `+` 收尾（不是 `;`）就不
	// 含在 bashMetaChars 裡，能繞過整條規則。find 本身留在允許清單（列目錄
	// 是 readonly 的正常需求），只擋帶執行旗標的用法。
	if head == "find" && hasFindExecFlag(fields) {
		return gateDeny("denied_bash_rule", "a2a gate: find 不允許帶 -exec/-execdir/-ok/-okdir/-fprintf")
	}
	return gateAllow("a2a gate: 等級 " + string(level) + " 允許指令 " + head)
}

// findExecFlags 是 find 會用來啟動另一個程式（或把輸出格式化成別的用途）的
// 旗標。`+` 收尾的 -exec（如 `find x -exec rm -rf {} +`）不含分號，逃過
// bashMetaChars 對 ";" 的檢查，因此改用旗標本身擋。
var findExecFlags = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-fprintf": true,
}

func hasFindExecFlag(fields []string) bool {
	for _, f := range fields[1:] {
		if findExecFlags[f] {
			return true
		}
	}
	return false
}

// gitDecision 檢查 git 的子命令。子命令必須是 fields[1]：允許 `git -C <dir>
// push` 這種形式等於允許沙盒對任意 repo 動手，而 -C 的值本身無法用首 token
// 規則約束。
func gitDecision(level GrantLevel, fields []string) gateDecision {
	if len(fields) < 2 {
		return gateDeny("denied_bash_rule", "a2a gate: git 缺少子命令")
	}
	sub := fields[1]
	if strings.HasPrefix(sub, "-") {
		return gateDeny("denied_bash_rule", "a2a gate: git 不允許在子命令前加旗標")
	}
	ok := readonlyGitSubs[sub]
	if !ok && level == GrantDevelop {
		ok = developGitSubs[sub]
	}
	if !ok {
		return gateDeny("denied_bash_rule", "a2a gate: 等級 "+string(level)+" 不允許 git "+sub)
	}
	if sub == "push" {
		// 保護分支命名空間 aa/<session>（BranchFor，a2a_executor.go:39）：
		// 沙盒可以推自己的分支，但不能強推、不能刪遠端分支、不能用 refspec
		// 把 commit 推到別的 ref 上。
		for _, f := range fields[2:] {
			if f == "--force" || f == "-f" || f == "--delete" || f == "-d" ||
				strings.HasPrefix(f, "--force-with-lease") || strings.Contains(f, ":") {
				return gateDeny("denied_bash_rule", "a2a gate: git push 不允許參數 "+f)
			}
		}
	}
	if sub == "remote" {
		// review Minor 2：readonly/develop 都能到 "remote"（readonlyGitSubs
		// 允許），但 remote 有自己的子命令，原本沒檢查 fields[2]——
		// `git remote set-url origin <attacker>` 可以把 origin 換掉，develop
		// 允許的 git push 接著就會把東西送到攻擊者的遠端。只放行明顯是讀取
		// 的用法，其餘（add/remove/rename/set-url/set-head/...）一律拒絕。
		rsub := ""
		if len(fields) > 2 {
			rsub = fields[2]
		}
		if !readonlyGitRemoteSubs[rsub] {
			return gateDeny("denied_bash_rule", "a2a gate: git remote 不允許子命令 "+rsub)
		}
	}
	return gateAllow("a2a gate: 等級 " + string(level) + " 允許 git " + sub)
}

// readonlyGitRemoteSubs 是 `git remote` 允許的子命令：裸的 `git remote`
// （列出）、`-v`（列出並顯示 URL）、`show <name>`、`get-url <name>`。任何會
// 改動遠端設定的子命令（add/remove/rename/set-url/set-head/set-branches/
// prune/update）都不在清單內，因為那些足以把 develop 允許的 git push 導向
// 攻擊者控制的遠端。
var readonlyGitRemoteSubs = map[string]bool{
	"": true, "-v": true, "show": true, "get-url": true,
}

// GateLogEntry 是一次執行期判定的紀錄。
type GateLogEntry struct {
	At        string `json:"at"`
	Session   string `json:"session"`
	ContextID string `json:"context_id"`
	CallerID  string `json:"caller_id"`
	Agent     string `json:"agent"`
	Level     string `json:"level"`
	Tool      string `json:"tool"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail"`
}

// GateLogPath 是獨立於 a2a-audit.jsonl 的 gate log。分開存放：後者是委派紀錄
// （誰要求了什麼），前者是執行期判定紀錄（沙盒想做什麼），量級差兩個數量級，
// 混在一起會把委派紀錄淹掉。
func GateLogPath(root string) string { return filepath.Join(root, "a2a-gate.jsonl") }

// AppendGateLog 以單次 O_APPEND write 追加一行。gate 是每次工具呼叫都被 spawn
// 的獨立行程，沒有共用鎖可用；Linux 上 < 4KB 的 append 是原子的，所以多個並發
// gate 行程不會互相截斷。0600：這份 log 含 caller id 與指令內容。
func AppendGateLog(root string, e GateLogEntry) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(GateLogPath(root), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	blob, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(blob, '\n'))
	return err
}
