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
func parseDecision(content string) (allow bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(content)) {
	case "y", "yes", "allow", "ok", "准", "允許", "可以", "好":
		return true, true
	case "n", "no", "deny", "拒絕", "不", "否":
		return false, true
	default:
		return false, false
	}
}

// summarizeToolInput renders a short human description of what's being run.
func summarizeToolInput(toolName string, raw json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if cmd, ok := m["command"].(string); ok && cmd != "" { // Bash
		return cmd
	}
	s := string(raw)
	if len(s) > 200 {
		s = s[:200] + "…"
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

	id := sanitize(hi.ToolName) + "-" + sanitize(strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000"), ".", ""))
	detail := summarizeToolInput(hi.ToolName, hi.ToolInput)

	// Record the pending request and post it to the binding's channel.
	req := map[string]string{"id": id, "tool": hi.ToolName, "detail": detail}
	if err := AtomicWriteJSON(pathIn(permPendingDir(b.Root), id+".json"), req); err != nil {
		fmt.Fprint(out, hookDecisionJSON(false, "permission gate: cannot record request"))
		return nil
	}
	msg := fmt.Sprintf("🔐 權限請求：session 想執行 %s\n```\n%s\n```\n回 y 允許 / n 拒絕（逾時自動拒絕）", hi.ToolName, detail)
	_ = AtomicWriteJSON(pathIn(b.Root, "outbox", "pending", "perm-"+id+".json"),
		OutputJob{Schema: 1, JobID: "perm-" + id, Send: true, Text: msg})

	// Block for the decision (written by the worker when the user replies).
	allow, decided := waitDecision(ctx, b.Root, id, timeout)
	_ = os.Remove(pathIn(permPendingDir(b.Root), id+".json"))
	if !decided {
		fmt.Fprint(out, hookDecisionJSON(false, "權限請求逾時，自動拒絕"))
		return nil
	}
	fmt.Fprint(out, hookDecisionJSON(allow, "由使用者於頻道決定"))
	return nil
}

func waitDecision(ctx context.Context, root, id string, timeout time.Duration) (allow bool, decided bool) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	path := pathIn(permDecisionDir(root), id+".json")
	for time.Now().Before(deadline) {
		var d struct {
			Allow bool `json:"allow"`
		}
		if err := ReadJSON(path, &d); err == nil {
			_ = os.Remove(path)
			return d.Allow, true
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, false
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false, false
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

// resolvePermission records the user's decision for a pending request id.
func resolvePermission(root, id string, allow bool) error {
	return AtomicWriteJSON(pathIn(permDecisionDir(root), id+".json"), map[string]bool{"allow": allow})
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
