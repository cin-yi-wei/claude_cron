package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDecision(t *testing.T) {
	for _, c := range []struct {
		in        string
		wantAllow bool
		wantOK    bool
	}{
		{"y", true, true}, {"YES", true, true}, {"允許", true, true}, {"好", true, true},
		{"n", false, true}, {"no", false, true}, {"拒絕", false, true},
		{"maybe", false, false}, {"查 log", false, false}, {"", false, false},
	} {
		a, ok := parseDecision(c.in)
		if a != c.wantAllow || ok != c.wantOK {
			t.Errorf("parseDecision(%q) = %v,%v want %v,%v", c.in, a, ok, c.wantAllow, c.wantOK)
		}
	}
}

func TestHookDecisionJSON(t *testing.T) {
	var m map[string]map[string]any
	_ = json.Unmarshal([]byte(hookDecisionJSON(true, "ok")), &m)
	if m["hookSpecificOutput"]["permissionDecision"] != "allow" {
		t.Fatalf("allow decode: %v", m)
	}
	_ = json.Unmarshal([]byte(hookDecisionJSON(false, "no")), &m)
	if m["hookSpecificOutput"]["permissionDecision"] != "deny" {
		t.Fatalf("deny decode: %v", m)
	}
}

// Full gate cycle: gate posts a request + waits; we resolve it; gate returns allow.
func TestPermissionGateApproveFlow(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	// A binding whose worktree == the hook's cwd.
	wt := t.TempDir()
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: pathIn(root, "bindings", "b")})

	hookJSON := `{"cwd":"` + wt + `","tool_name":"Bash","tool_input":{"command":"npm install"}}`

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		_ = RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second)
		close(done)
	}()

	// Wait for the pending request to appear, then approve it.
	bRoot := pathIn(root, "bindings", "b")
	waitFor(t, func() bool { return oldestPendingPermission(bRoot) != "" })
	// A request message should have been posted to the binding's outbox.
	if n := countJSONFilesSafe(pathIn(bRoot, "outbox", "pending")); n < 1 {
		t.Fatalf("expected a posted permission request in outbox, got %d", n)
	}
	id := oldestPendingPermission(bRoot)
	if err := resolvePermission(bRoot, id, true); err != nil {
		t.Fatal(err)
	}
	<-done

	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("gate output = %s", out.String())
	}
}

func TestPermissionGateTimeoutDenies(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	wt := t.TempDir()
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: pathIn(root, "bindings", "b")})
	hookJSON := `{"cwd":"` + wt + `","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`
	var out bytes.Buffer
	_ = RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 300*time.Millisecond)
	if !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("timeout should deny; got %s", out.String())
	}
}
