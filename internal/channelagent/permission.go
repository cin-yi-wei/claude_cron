package channelagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cleanAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// Permission gate: a PreToolUse hook for bound sessions. When Claude is about to
// run a gated tool, this is invoked with the hook JSON on stdin. It posts an
// approval request to the binding's channel and blocks until the user answers
// y/n (routed in by the worker), then prints the hook's permission decision.

// hookInput is the subset of the PreToolUse hook stdin payload we use.
type hookInput struct {
	CWD       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// hookDecisionJSON builds the PreToolUse hook stdout payload.
func hookDecisionJSON(allow bool, reason string) string {
	dec := "deny"
	if allow {
		dec = "allow"
	}
	b, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       dec,
			"permissionDecisionReason": reason,
		},
	})
	return string(b)
}

// parseDecision interprets a user reply as allow/deny. ok=false if it is not a
// recognizable decision (so it can be treated as a normal message).
func parseDecision(content string) (allow, remember, ok bool) {
	switch strings.ToLower(strings.TrimSpace(content)) {
	case "ya", "y!", "yy", "always", "y always", "記住", "都允許", "永遠":
		return true, true, true // allow + remember this category
	case "y", "yes", "allow", "ok", "准", "允許", "可以", "好":
		return true, false, true
	case "n", "no", "deny", "拒絕", "不", "否":
		return false, false, true
	default:
		return false, false, false
	}
}

// bashCommand extracts the command string from a Bash tool_input.
func bashCommand(raw json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if c, ok := m["command"].(string); ok {
		return c
	}
	return ""
}

// matchedRiskyPattern returns the risky pattern a bash command matches (install,
// download, privilege, destructive), or "" if none. The pattern doubles as the
// "remember" key, so approving once can auto-allow that category later.
func matchedRiskyPattern(cmd string) string {
	c := strings.ToLower(cmd)
	for _, pat := range []string{
		"npm install", "npm i ", "npm ci", "yarn add", "pnpm add", "pnpm install",
		"pip install", "pip3 install", "gem install", "bundle install", "bundle add",
		"apt ", "apt-get", "apt-add", "dpkg", "brew install", "cargo install",
		"go install", "go get", "curl ", "wget ", "sudo ", "rm -rf", "mkfs", "dd if=",
	} {
		if strings.Contains(c, pat) {
			return pat
		}
	}
	return ""
}

// bashNeedsApproval reports whether a bash command should be escalated.
func bashNeedsApproval(cmd string) bool { return matchedRiskyPattern(cmd) != "" }

// gateKey is the "remember" key for a gated tool call: the matched risky pattern
// for Bash, else the tool name (e.g. an MCP tool).
func gateKey(toolName string, toolInput json.RawMessage) string {
	if toolName == "Bash" {
		if p := matchedRiskyPattern(bashCommand(toolInput)); p != "" {
			return "bash:" + p
		}
		return ""
	}
	return "tool:" + toolName
}

// remembered approvals are stored per binding so an approved category isn't
// re-asked. Cleared by deleting permissions/allowed.json.
func allowedKeysPath(root string) string { return pathIn(root, "permissions", "allowed.json") }

func isRemembered(root, key string) bool {
	if key == "" {
		return false
	}
	var keys []string
	if err := ReadJSON(allowedKeysPath(root), &keys); err != nil {
		return false
	}
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

func rememberKey(root, key string) error {
	if key == "" {
		return nil
	}
	var keys []string
	_ = ReadJSON(allowedKeysPath(root), &keys)
	for _, k := range keys {
		if k == key {
			return nil
		}
	}
	keys = append(keys, key)
	return AtomicWriteJSON(allowedKeysPath(root), keys)
}

// summarizeToolInput renders a short human description of what's being run.
func summarizeToolInput(toolName string, raw json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if cmd, ok := m["command"].(string); ok && cmd != "" { // Bash
		return cmd
	}
	if u, ok := m["url"].(string); ok && u != "" { // WebFetch
		return u
	}
	if q, ok := m["query"].(string); ok && q != "" { // WebSearch
		return q
	}
	// MCP / other tools: show the full input so you can judge safety. Only clamp
	// absurdly large payloads, and say how much was hidden (never silently abbreviate).
	s := string(raw)
	if n := len([]rune(s)); n > 1500 {
		s = string([]rune(s)[:1500]) + fmt.Sprintf("… (截斷，完整共 %d 字)", n)
	}
	return s
}

// permissionPaths holds the per-binding directories for the gate protocol.
func permPendingDir(root string) string  { return pathIn(root, "permissions", "pending") }
func permDecisionDir(root string) string { return pathIn(root, "permissions", "decisions") }

// RunPermissionGate is the hook entrypoint. registryRoot is the .channel-agent
// path (to resolve which binding the cwd belongs to). Reads hook JSON from in,
// writes the decision JSON to out. Blocks up to timeout for the user's reply;
// on timeout it denies (safe default).
func RunPermissionGate(ctx context.Context, registryRoot string, in io.Reader, out io.Writer, timeout time.Duration) error {
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	var hi hookInput
	if err := json.Unmarshal(data, &hi); err != nil {
		// Can't parse → fail safe: deny.
		fmt.Fprint(out, hookDecisionJSON(false, "permission gate: bad hook input"))
		return nil
	}

	reg, err := LoadRegistry(registryRoot)
	if err != nil {
		fmt.Fprint(out, hookDecisionJSON(false, "permission gate: registry error"))
		return nil
	}
	b, ok := bindingByWorktree(reg, hi.CWD)
	if !ok {
		// Unknown worktree → don't block a session we can't route for: allow.
		fmt.Fprint(out, hookDecisionJSON(true, "permission gate: no binding for cwd, allowing"))
		return nil
	}
	// Auto-approve (trusted-binding bypass): skip the channel y/n entirely.
	if b.AutoApprove {
		fmt.Fprint(out, hookDecisionJSON(true, "permission gate: auto-approved (binding bypass)"))
		return nil
	}

	// Only escalate the things worth a human decision (installs / downloads /
	// privilege / destructive, and all MCP). Ordinary Bash — file edits via
	// sed/mv/cat, git, build/test, ls — is auto-allowed so the channel isn't
	// spammed for every command.
	if hi.ToolName == "Bash" && !bashNeedsApproval(bashCommand(hi.ToolInput)) {
		fmt.Fprint(out, hookDecisionJSON(true, "permission gate: ordinary command auto-allowed"))
		return nil
	}

	// If this category was already approved with "remember", auto-allow it.
	key := gateKey(hi.ToolName, hi.ToolInput)
	if isRemembered(b.Root, key) {
		fmt.Fprint(out, hookDecisionJSON(true, "permission gate: remembered approval ("+key+")"))
		return nil
	}

	id := sanitize(hi.ToolName) + "-" + sanitize(strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000"), ".", ""))
	detail := summarizeToolInput(hi.ToolName, hi.ToolInput)

	// Record the pending request and post it to the binding's channel.
	req := map[string]string{"id": id, "tool": hi.ToolName, "detail": detail, "key": key}
	if err := AtomicWriteJSON(pathIn(permPendingDir(b.Root), id+".json"), req); err != nil {
		fmt.Fprint(out, hookDecisionJSON(false, "permission gate: cannot record request"))
		return nil
	}
	msg := fmt.Sprintf("🔐 權限請求：session 想執行 %s\n```\n%s\n```\n點下方按鈕，或回 y 允許一次 / ya 允許並記住這類 / n 拒絕（逾時自動拒絕）", hi.ToolName, detail)
	_ = AtomicWriteJSON(pathIn(b.Root, "outbox", "pending", "perm-"+id+".json"),
		OutputJob{Schema: 1, JobID: "perm-" + id, Send: true, Text: msg, Components: permissionButtons(id)})

	// Block for the decision (written by the worker when the user replies).
	allow, remember, decided := waitDecision(ctx, b.Root, id, timeout)
	_ = os.Remove(pathIn(permPendingDir(b.Root), id+".json"))
	if !decided {
		fmt.Fprint(out, hookDecisionJSON(false, "權限請求逾時，自動拒絕"))
		return nil
	}
	if allow && remember {
		_ = rememberKey(b.Root, key)
	}
	fmt.Fprint(out, hookDecisionJSON(allow, "由使用者於頻道決定"))
	return nil
}

func waitDecision(ctx context.Context, root, id string, timeout time.Duration) (allow, remember, decided bool) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	path := pathIn(permDecisionDir(root), id+".json")
	for time.Now().Before(deadline) {
		var d struct {
			Allow    bool `json:"allow"`
			Remember bool `json:"remember"`
		}
		if err := ReadJSON(path, &d); err == nil {
			_ = os.Remove(path)
			return d.Allow, d.Remember, true
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, false, false
		}
		select {
		case <-ctx.Done():
			return false, false, false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false, false, false
}

// oldestPendingPermission returns the id of the oldest pending permission
// request for root, or "" if none.
func oldestPendingPermission(root string) string {
	p, err := oldestJSON(permPendingDir(root))
	if err != nil || p == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(p), ".json")
}

// newestPendingPermission returns the id of the NEWEST pending permission
// request, or "" if none. The Claude session is single-threaded, so at most one
// gate hook is ever actually blocked/waiting — and it is the most recent one.
// Any older pending files are orphans left by gate hooks that were killed
// (Claude Code's exec-timeout, a session restart, a cancelled tool) before they
// could remove their own pending marker. Resolving the OLDEST (as the code used
// to) sent the user's y/n to a dead orphan while the live gate starved to a
// timeout-deny — the "I replied y but it kept getting blocked" race. Resolve the
// newest instead so the decision always reaches the gate that is actually waiting.
func newestPendingPermission(root string) string {
	names := jsonNames(permPendingDir(root))
	if len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[len(names)-1], ".json")
}

// gcOrphanPermissions removes every pending permission marker except keepID.
// Called when a decision is applied to the live (newest) request: all older
// markers are provably dead orphans (single-threaded session), so clearing them
// stops the pile-up that keeps poisoning future y/n resolution.
func gcOrphanPermissions(root, keepID string) {
	for _, n := range jsonNames(permPendingDir(root)) {
		if strings.TrimSuffix(n, ".json") == keepID {
			continue
		}
		_ = os.Remove(pathIn(permPendingDir(root), n))
	}
}

// resolvePermission records the user's decision for a pending request id.
func resolvePermission(root, id string, allow, remember bool) error {
	return AtomicWriteJSON(pathIn(permDecisionDir(root), id+".json"), map[string]bool{"allow": allow, "remember": remember})
}

// permCustomIDPrefix 標記一個 Discord 元件互動屬於權限閘門。custom_id 格式為
// `ccperm|<action>|<id>`（action ∈ allow/remember/deny，id 即 pending 標記的 id）。
const permCustomIDPrefix = "ccperm"

// permissionButtons 建立權限閘門的 action row（3 顆按鈕）。custom_id 直接綁定
// 該次請求的 id，因此使用者按哪一顆就決定哪一個 gate，沒有 y/n 自由文字必須
// 猜「最新 pending」的競態。id 長度遠小於 Discord custom_id 100 字上限。
func permissionButtons(id string) []any {
	btn := func(style int, label, action string) map[string]any {
		return map[string]any{
			"type":      2, // 2 = Button
			"style":     style,
			"label":     label,
			"custom_id": permCustomIDPrefix + "|" + action + "|" + id,
		}
	}
	return []any{
		map[string]any{
			"type": 1, // 1 = Action Row
			"components": []any{
				btn(3, "允許一次", "allow"),    // 3 = success（綠）
				btn(1, "允許並記住", "remember"), // 1 = primary（藍紫）
				btn(4, "拒絕", "deny"),        // 4 = danger（紅）
			},
		},
	}
}

// parsePermissionCustomID 解析按鈕 custom_id。ok=false 表示不是權限閘門按鈕或格
// 式不合法（呼叫端應忽略）。
func parsePermissionCustomID(customID string) (action, id string, ok bool) {
	parts := strings.SplitN(customID, "|", 3)
	if len(parts) != 3 || parts[0] != permCustomIDPrefix {
		return "", "", false
	}
	switch parts[1] {
	case "allow", "remember", "deny":
	default:
		return "", "", false
	}
	if parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// applyPermissionInteraction 依按鈕 custom_id 把決定寫入對應 binding 的 decision
// 檔（等待中的 gate hook 會讀到並放行/拒絕），並移除該 id 的 pending 標記。這是
// 可單元測試的純函式：只吃 (registry, channelID, customID)，不碰 HTTP ACK。
// 回傳 resultLine（給 ACK 顯示的結果文字）、binding 名稱、id、是否成功處理。
func applyPermissionInteraction(reg Registry, channelID, customID string) (resultLine, name, id string, ok bool) {
	action, pid, pok := parsePermissionCustomID(customID)
	if !pok {
		return "", "", "", false
	}
	b, bok := reg.BindingByChannel(channelID)
	if !bok {
		return "", "", "", false
	}
	var allow, remember bool
	var line string
	switch action {
	case "allow":
		allow, remember, line = true, false, "✅ 已允許"
	case "remember":
		allow, remember, line = true, true, "🧠 已允許並記住"
	case "deny":
		allow, remember, line = false, false, "❌ 已拒絕"
	}
	if err := resolvePermission(b.Root, pid, allow, remember); err != nil {
		return "", "", "", false
	}
	// 移除這個 id 的 pending 標記（決定已寫入 decision 檔，等待中的 gate 會讀到）。
	_ = os.Remove(pathIn(permPendingDir(b.Root), pid+".json"))
	return line, b.Name, pid, true
}

func bindingByWorktree(reg Registry, cwd string) (Binding, bool) {
	cwd = cleanAbs(cwd)
	for _, b := range reg.Bindings {
		if cleanAbs(b.Worktree) == cwd {
			return b, true
		}
	}
	return Binding{}, false
}
