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
// worktree 內寫入，是繞過閘門本身（review Critical 3；.mcp.json 是第二輪
// review 的 Minor 追加）。
//
//   - <worktree>/.claude/settings.local.json：EnsureAgentSettings
//     （worktree.go）寫入的就是這次 tmux session 的 PreToolUse hook 設定；
//     writeAgentSettings 只在檔案不存在時才寫（worktree.go:176），所以沙盒
//     把內容改掉之後不會被復原，破壞會撐過 session 重啟。
//   - <worktree>/.mcp.json：Claude Code 的 project-scope MCP server 註冊
//     檔，裡面每一段設定都是「session 啟動時要執行的指令」。改掉它等於自
//     己註冊一個會被執行的程式，跟改 settings.local.json 是同一類風險（改
//     設定檔換取之後的執行權），三個等級都不放行。<worktree>/.claude/
//     settings.json（非 local）刻意不擋：hooks 是各來源合併疊加，不是互相
//     覆蓋，改那個檔案沒辦法拿掉閘門自己在 settings.local.json 裡的
//     PreToolUse hook。
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

	for _, rel := range []string{
		filepath.Join(".claude", "settings.local.json"),
		".mcp.json",
	} {
		if rp == resolveExistingPrefix(filepath.Join(wt, rel)) {
			return true
		}
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

// flagPolicy 是一個指令（或一個 git 子命令）允許帶哪些旗標的正向清單。
//
// review 第二輪的核心教訓：黑名單擋旗標會一直輸——第一輪只擋了
// find -exec 系列，`-delete`、`-fprint`、`-fprint0`、`-fls` 全部沒擋到；
// rg 的 --pre、git diff 的 --output、tree 的 -o、file 的 -C 全都是「同一
// 個模式的另一個旗標」。黑名單只擋「想到的那幾個」，攻擊者只需要找到一個
// 沒被列進去的。這裡整個反過來：short/long 兩個集合都是允許清單，沒被列
// 進去的旗標一律拒絕，不管是 "--opt=value"、"--opt value" 還是群聚短旗標
// "-xyz" 的形式送進來——三種寫法都經過同一個 flagTokenAllowed。
type flagPolicy struct {
	short map[byte]bool   // 允許的單字元短旗標，可以群聚（-l 和 -a 都在時 -la 也允許）
	long  map[string]bool // 允許的長旗標名稱（不含 "=value" 那一段）
}

func byteSet(chars string) map[byte]bool {
	m := make(map[byte]bool, len(chars))
	for i := 0; i < len(chars); i++ {
		m[chars[i]] = true
	}
	return m
}

func strSet(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// flagTokenAllowed 判斷一個以 "-" 開頭的 token 是否落在 p 允許的集合內。
//   - "--opt=value"：只比對 "=" 前面的 "--opt"。
//   - "--opt value"：值是下一個 token，不以 "-" 開頭，走位置引數的路徑，
//     根本不會進到這個函式。
//   - 群聚短旗標（"-la"）：拆成每一個字元分別檢查 p.short，全部都在允許集
//     合裡才算允許。代價：像 "-n5" 這種旗標字元後面直接黏數值、中間沒有空
//     白或 "=" 的寫法會被擋（'5' 不是旗標字元）——這是刻意犧牲一點可用性
//     換安全；改成分開寫的 "-n 5" 一樣能用。
func flagTokenAllowed(p flagPolicy, tok string) bool {
	if strings.HasPrefix(tok, "--") {
		name := tok
		if i := strings.IndexByte(tok, '='); i >= 0 {
			name = tok[:i]
		}
		return p.long[name]
	}
	body := tok[1:]
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if !p.short[body[i]] {
			return false
		}
	}
	return true
}

// firstDeniedFlag 掃過 args 裡每一個以 "-" 開頭的 token。不以 "-" 開頭的是
// 位置引數（檔名、模式字串……），本輪不做路徑侷限（規格第五節、Bash 規則
// 本身的既有限制），一律放行。回傳第一個被拒絕的 token；全部通過則回空字
// 串。p 是零值（未定義任何允許項）時，任何旗標都會被拒絕——查不到政策就是
// 不給旗標，不是放行。
func firstDeniedFlag(p flagPolicy, args []string) string {
	for _, f := range args {
		if strings.HasPrefix(f, "-") && !flagTokenAllowed(p, f) {
			return f
		}
	}
	return ""
}

// readonlyHeadFlags 是 readonlyBashHeads 裡「一般指令」（不含 git、find——
// 兩者語法跟這裡的字元群聚模型不合，各自有專屬的判定函式）的旗標允許清單。
// 沒列在這裡的旗標，不管長短，一律拒絕。
var readonlyHeadFlags = map[string]flagPolicy{
	"ls":  {short: byteSet("laAhRtSr1dFG")},
	"cat": {short: byteSet("nbsTEAet")},
	"head": {
		short: byteSet("ncqvz"),
		long:  strSet("--lines", "--bytes", "--quiet", "--silent", "--verbose"),
	},
	"tail": {
		short: byteSet("ncqvzf"),
		long:  strSet("--lines", "--bytes", "--quiet", "--silent", "--verbose", "--follow"),
	},
	"wc": {short: byteSet("lwcmL")},
	"du": {short: byteSet("hsacx0")},
	"stat": {
		short: byteSet("cfLt"),
		long:  strSet("--format", "--printf", "--terse"),
	},
	// review Minor：-C 會編譯出一份 .mgc 檔（寫檔），是 file 唯一的危險旗
	// 標；其餘都只是「怎麼判斷/怎麼顯示」，不寫檔、不執行、不連網。-C 不在
	// 允許集合裡。
	"file": {short: byteSet("bimLzks0n")},
	// review Minor：tree 唯一的危險旗標是 -o（輸出寫進檔案）；其餘都是純顯
	// 示格式選項。-o 不在允許集合裡。
	"tree": {short: byteSet("adLfiCxhnpugDqA")},
	// review Critical：--pre / --pre-glob 會用攻擊者指定的 argv 執行任意外
	// 部程式（ripgrep 官方文件明載：「search the standard output of
	// COMMAND PATH」）；-z/--search-zip 會呼叫 gzip/bzip2/xz 等外部解壓縮
	// 程式。兩者都不在允許集合裡——用真的 rg --pre=/usr/bin/id 驗證過。
	"rg": {
		short: byteSet("inrRwvcloABCefgtTmxFPSsuUHIL"),
		long: strSet("--include", "--exclude", "--iglob", "--glob", "--fixed-strings",
			"--word-regexp", "--line-regexp", "--hidden", "--no-ignore", "--type",
			"--ignore-case", "--smart-case", "--multiline"),
	},
	"grep": {
		short: byteSet("inrRwvcloABCefxFEHILsz"),
		long: strSet("--include", "--exclude", "--ignore-case", "--word-regexp",
			"--line-regexp", "--fixed-strings", "--extended-regexp", "--color"),
	},
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
	fromReadonly := readonlyBashHeads[head]
	allowed := fromReadonly
	if !allowed && level == GrantDevelop {
		allowed = developBashHeads[head]
	}
	if !allowed {
		return gateDeny("denied_bash_rule", "a2a gate: 等級 "+string(level)+" 不允許指令 "+head)
	}
	if head == "git" {
		return gitDecision(level, fields)
	}
	if head == "find" {
		return findDecision(fields)
	}
	// fromReadonly：這個 head 只可能來自 readonlyBashHeads（develop 專屬的
	// go/make/npm/python/... 完全不查這份清單，維持「develop 如設計」不受
	// 影響——那些工具的任意執行能力是設計，不是這輪要收的口子）。
	if fromReadonly {
		if bad := firstDeniedFlag(readonlyHeadFlags[head], fields[1:]); bad != "" {
			return gateDeny("denied_bash_rule", "a2a gate: 等級 "+string(level)+" 不允許 "+head+" 帶旗標 "+bad)
		}
	}
	return gateAllow("a2a gate: 等級 " + string(level) + " 允許指令 " + head)
}

// findDecision 用允許清單判斷 find 的引數：任何以 "-" 開頭、不在
// readonlyFindTokens 裡的 token 一律拒絕。find 的每個選項/動作都是完整單
// 詞（"-name"、"-delete"……），不是可以群聚的單字元短旗標，所以這裡用整個
// token 精確比對，不是 flagTokenAllowed 的字元群聚模型。
//
// review Critical（第二輪）：第一輪只擋了 -exec/-execdir/-ok/-okdir/
// -fprintf，`-delete`（find <path> -name '*.go' -delete 真的刪了檔案）、
// -fprint、-fprint0、-fls 全部沒擋到——這就是黑名單的結構性問題。這裡整個
// 反過來，只放行明確安全（純過濾判斷式、或只印到 stdout）的動作，find 本
// 身留在允許清單，因為列目錄/找檔案是 readonly 的正常需求。
func findDecision(fields []string) gateDecision {
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") && !readonlyFindTokens[f] {
			return gateDeny("denied_bash_rule", "a2a gate: find 不允許旗標 "+f)
		}
	}
	return gateAllow("a2a gate: 允許的 find 用法")
}

var readonlyFindTokens = map[string]bool{
	"-name": true, "-iname": true, "-path": true, "-ipath": true,
	"-type": true, "-maxdepth": true, "-mindepth": true,
	"-size": true, "-mtime": true, "-mmin": true, "-atime": true, "-ctime": true,
	"-newer": true, "-newermt": true,
	// -print/-print0/-printf 只印到 stdout；-fprint/-fprint0/-fprintf/-fls
	// 都帶一個檔名引數把輸出寫進那個檔案，不在允許清單內。
	"-print": true, "-print0": true, "-printf": true, "-ls": true,
	"-not": true, "-and": true, "-or": true, "-a": true, "-o": true,
	"-regex": true, "-iregex": true, "-empty": true, "-inum": true,
	"-depth": true, "-daystart": true, "-xdev": true, "-samefile": true,
	"-perm": true, "-readable": true, "-writable": true, "-executable": true,
	"-true": true, "-false": true, "-prune": true,
	"-links": true, "-user": true, "-group": true, "-uid": true, "-gid": true,
	"-context": true,
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
		// review Minor（第一輪）：readonly/develop 都能到 "remote"
		// （readonlyGitSubs 允許），但 remote 有自己的子命令，原本沒檢查
		// fields[2]——`git remote set-url origin <attacker>` 可以把 origin
		// 換掉，develop 允許的 git push 接著就會把東西送到攻擊者的遠端。
		// 只放行明顯是讀取的用法，其餘（add/remove/rename/set-url/
		// set-head/...）一律拒絕。
		//
		// review Important（第二輪）：「show」原本也放行，但真的用
		// `git remote show origin` 驗證過——不帶 -n 的話 show 會對遠端發一
		// 次網路查詢（git 官方文件：「-n do not query remotes」，意味著沒
		// 給 -n 就會查）。readonly 規格明講「no-outbound」，這裡整個把
		// show 從允許清單移除，不試著靠檢查是否帶了 -n 來放行——少一個子
		// 命令換掉一整類「忘記檢查 -n」的重蹈覆轍。
		rsub := ""
		if len(fields) > 2 {
			rsub = fields[2]
		}
		if !readonlyGitRemoteSubs[rsub] {
			return gateDeny("denied_bash_rule", "a2a gate: git remote 不允許子命令 "+rsub)
		}
	} else if readonlyGitSubs[sub] {
		// review（第二輪）：readonlyGitSubs 裡除了 remote（有自己的子命令
		// 層 dispatch，上面已經處理）之外的每個子命令，一律再查一次旗標允
		// 許清單——不管實際呼叫的是 readonly 還是 develop，因為這幾個子命
		// 令沒有一個是 develop「設計上」要用來改動東西的：develop 真正的
		// 改動手段是 commit/push/checkout 等 developGitSubs，那些維持原
		// 樣，不受這裡影響。查不到 sub 對應的政策（readonlyGitSubFlags 沒
		// 這一筆）拿到的是零值 flagPolicy{}，等於「不給任何旗標」，是安全
		// 預設，不是漏洞。
		if bad := firstDeniedFlag(readonlyGitSubFlags[sub], fields[2:]); bad != "" {
			return gateDeny("denied_bash_rule", "a2a gate: git "+sub+" 不允許旗標 "+bad)
		}
	}
	return gateAllow("a2a gate: 等級 " + string(level) + " 允許 git " + sub)
}

// readonlyGitRemoteSubs 是 `git remote` 允許的子命令：裸的 `git remote`
// （列出）、`-v`（列出並顯示 URL）、`get-url <name>`。「show」（會連網查
// 詢，見上）與任何會改動遠端設定的子命令（add/remove/rename/set-url/
// set-head/set-branches/prune/update）都不在清單內。
var readonlyGitRemoteSubs = map[string]bool{
	"": true, "-v": true, "get-url": true,
}

// readonlyGitSubFlags 是 readonlyGitSubs 裡每個子命令（remote 除外，見上）
// 的旗標允許清單，跟 readonlyHeadFlags 用同一套字元群聚模型——git 的旗標語
// 法跟一般 GNU 工具一致，不是 find 那種整詞判斷。
var readonlyGitSubFlags = map[string]flagPolicy{
	"status": {
		short: byteSet("sbv"),
		long:  strSet("--short", "--branch", "--long", "--verbose", "--porcelain", "--ignored", "--no-renames"),
	},
	// review Critical（第二輪）：--output=FILE 會把整份輸出寫進任意檔案，
	// 真的用 `git diff --output=/path` 驗證過會把目標檔案清空重寫；log、
	// show 共用同一套 diff machinery，一樣支援這個旗標，三個子命令都不放
	// 行 --output。
	"log": {
		short: byteSet("p"),
		long: strSet("--oneline", "--graph", "--stat", "--name-only", "--name-status",
			"--patch", "--no-patch", "--since", "--until", "--author", "--grep",
			"--all", "--reverse", "--pretty", "--format", "--abbrev-commit",
			"--decorate", "--no-color", "--color"),
	},
	"diff": {
		short: byteSet("p"),
		long: strSet("--stat", "--name-only", "--name-status", "--patch",
			"--no-color", "--color", "--numstat", "--shortstat", "--summary"),
	},
	"show": {
		short: byteSet("ps"),
		long:  strSet("--stat", "--name-only", "--name-status", "--patch", "--no-color", "--color", "--pretty", "--format"),
	},
	// review Important（第二輪）：允許清單只留「列出」，不留任何建立/刪
	// 除/改名（-d/-D/-m/-M/-c/-C/-u/--set-upstream-to/--unset-upstream）——
	// 真的用 `git branch -D <name>` 驗證過會刪掉分支。分支存在主 repo 底
	// 下，是所有從它切出去的 worktree 共用的；readonly 改得動分支，就等於
	// 改得動跟這個沙盒共用同一個主 repo 的其他 ~40 個 cc- binding。
	"branch": {
		short: byteSet("arv"),
		long:  strSet("--list", "--all", "--remotes", "--verbose", "--color", "--no-color", "--contains", "--merged", "--no-merged"),
	},
	"describe": {long: strSet("--tags", "--long", "--abbrev", "--dirty", "--always", "--all", "--contains")},
	"blame":    {short: byteSet("ewp"), long: strSet("--date", "--porcelain", "--line-porcelain")},
	"ls-files": {
		short: byteSet("comdsiuz"),
		long:  strSet("--exclude-standard", "--others", "--cached", "--modified", "--deleted", "--stage"),
	},
	"rev-parse": {
		long: strSet("--verify", "--short", "--abbrev-ref", "--symbolic", "--symbolic-full-name",
			"--show-toplevel", "--is-inside-work-tree", "--is-bare-repository"),
	},
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
