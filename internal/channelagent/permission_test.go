package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDecision(t *testing.T) {
	for _, c := range []struct {
		in                      string
		wantAllow, wantRem, wOK bool
	}{
		{"y", true, false, true}, {"YES", true, false, true}, {"允許", true, false, true}, {"好", true, false, true},
		{"ya", true, true, true}, {"記住", true, true, true}, {"always", true, true, true},
		{"n", false, false, true}, {"no", false, false, true}, {"拒絕", false, false, true},
		{"maybe", false, false, false}, {"查 log", false, false, false}, {"", false, false, false},
	} {
		a, rem, ok := parseDecision(c.in)
		if a != c.wantAllow || rem != c.wantRem || ok != c.wOK {
			t.Errorf("parseDecision(%q) = %v,%v,%v want %v,%v,%v", c.in, a, rem, ok, c.wantAllow, c.wantRem, c.wOK)
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
	if err := resolvePermission(bRoot, id, true, true); err != nil { // allow + remember
		t.Fatal(err)
	}
	<-done

	// Approving with remember should persist the category for auto-allow next time.
	if !isRemembered(bRoot, "bash:npm install") {
		t.Fatalf("expected 'bash:npm install' remembered, allowed.json missing it")
	}
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

func TestPermissionGateEditInWorktreeAutoAllows(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	wt := t.TempDir()
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: pathIn(root, "bindings", "b")})

	target := filepath.Join(wt, "app", "models", "foo.rb")
	hookJSON := `{"cwd":"` + wt + `","tool_name":"Edit","tool_input":{"file_path":"` + target + `"}}`
	var out bytes.Buffer
	if err := RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("in-worktree Edit should auto-allow; got %s", out.String())
	}
	// No channel round-trip should have happened.
	bRoot := pathIn(root, "bindings", "b")
	if n := countJSONFilesSafe(pathIn(bRoot, "outbox", "pending")); n != 0 {
		t.Fatalf("in-worktree Edit should not post to the channel, got %d pending", n)
	}
}

// A worker writing its OWN outbox reply (job_id.json.tmp) lives under b.Root
// (the .channel-agent state dir), NOT b.Worktree (the git checkout) — those are
// two separate trees. This must auto-allow same as an in-worktree edit, or
// every single job reply would wrongly ask the channel for permission to write
// itself (the regression this test guards against).
func TestPermissionGateWriteToOwnOutboxAutoAllows(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	wt := t.TempDir()
	bRoot := pathIn(root, "bindings", "b")
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: bRoot})

	target := pathIn(bRoot, "outbox", "pending", "20260803T000000000000000-1-abc.json.tmp")
	hookJSON := `{"cwd":"` + wt + `","tool_name":"Write","tool_input":{"file_path":"` + target + `"}}`
	var out bytes.Buffer
	if err := RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("writing its own outbox reply should auto-allow; got %s", out.String())
	}
	if n := countJSONFilesSafe(pathIn(bRoot, "outbox", "pending")); n != 0 {
		t.Fatalf("should not have posted a permission request, got %d pending", n)
	}
}

// projectSlug must match the harness's own naming, which is what
// <TMPDIR>/claude-<uid>/<slug> and ~/.claude/projects/<slug> are built from:
// every non-alphanumeric byte becomes '-'. The second case is a real path from
// this deployment (note '_' and '.' both collapse to '-').
func TestProjectSlug(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/home/conray/project/fatgame-jfg-4908", "-home-conray-project-fatgame-jfg-4908"},
		{"/home/conray/project/claude_cron/.channel-agent/control-dc-workspace",
			"-home-conray-project-claude-cron--channel-agent-control-dc-workspace"},
	} {
		if got := projectSlug(c.in); got != c.want {
			t.Errorf("projectSlug(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// The harness tells every session to put drafts and intermediate files in its
// scratchpad (<TMPDIR>/claude-<uid>/<worktree slug>/<session>/scratchpad). That
// is a third tree outside both b.Worktree and b.Root, so without an explicit
// allowance every single draft file asked the channel for permission — which is
// exactly what happened in production (reply6.txt, reply7.txt, ...).
func TestPermissionGateWriteToScratchpadAutoAllows(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	wt := t.TempDir()
	bRoot := pathIn(root, "bindings", "b")
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: bRoot})

	target := filepath.Join(scratchpadRoot(wt), "68673ea1-session", "scratchpad", "reply6.txt")
	hookJSON := `{"cwd":"` + wt + `","tool_name":"Write","tool_input":{"file_path":"` + target + `"}}`
	var out bytes.Buffer
	if err := RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("scratchpad write should auto-allow; got %s", out.String())
	}
	if n := countJSONFilesSafe(pathIn(bRoot, "outbox", "pending")); n != 0 {
		t.Fatalf("scratchpad write should not post to the channel, got %d pending", n)
	}
}

// The scratchpad allowance is scoped to THIS binding's own slug, not to the
// whole <TMPDIR>/claude-<uid> tree: one binding's session must not silently
// write into another binding's scratchpad.
func TestPermissionGateWriteToOtherBindingScratchpadAsksChannel(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	bRoot := pathIn(root, "bindings", "b")
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: bRoot})

	// Same /tmp/claude-<uid> parent, different project slug.
	other := filepath.Join(scratchpadRoot(t.TempDir()), "sess", "scratchpad", "steal.txt")
	hookJSON := `{"cwd":"` + wt + `","tool_name":"Write","tool_input":{"file_path":"` + other + `"}}`

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		_ = RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second)
		close(done)
	}()

	waitFor(t, func() bool { return oldestPendingPermission(bRoot) != "" })
	id := oldestPendingPermission(bRoot)
	if id == "" {
		t.Fatal("writing another binding's scratchpad must ask the channel")
	}
	if err := resolvePermission(bRoot, id, true, false); err != nil {
		t.Fatal(err)
	}
	<-done
}

// Auto-memory is a harness feature the session drives itself: it writes memory
// files under ~/.claude/projects/<slug>/memory. That directory keys off the
// project's main repo, not the worktree the worker runs in, so both slugs must
// be allowed — otherwise every saved memory raises a channel prompt.
func TestPermissionGateWriteToProjectMemoryAutoAllows(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	bRoot := pathIn(root, "bindings", "b")
	projectDir := "/home/x/project/fatgame"
	worktree := "/home/x/project/fatgame-jfg-4908"
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", ProjectDir: projectDir, Worktree: worktree, Root: bRoot})

	// Memory dir derived from ProjectDir (the main repo), not the worktree.
	target := filepath.Join(home, ".claude", "projects", projectSlug(projectDir), "memory", "project_betlog.md")
	hookJSON := `{"cwd":"` + worktree + `","tool_name":"Write","tool_input":{"file_path":"` + target + `"}}`
	var out bytes.Buffer
	if err := RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("project memory write should auto-allow; got %s", out.String())
	}
	if n := countJSONFilesSafe(pathIn(bRoot, "outbox", "pending")); n != 0 {
		t.Fatalf("memory write should not post to the channel, got %d pending", n)
	}
}

// The memory allowance covers THIS project only: another project's memory dir
// is still a cross-project write and must be asked about.
func TestPermissionGateWriteToOtherProjectMemoryAsksChannel(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "b")
	worktree := "/home/x/project/fatgame-jfg-4908"
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", ProjectDir: "/home/x/project/fatgame", Worktree: worktree, Root: bRoot})

	other := filepath.Join(home, ".claude", "projects", projectSlug("/home/x/project/some-other-repo"), "memory", "steal.md")
	hookJSON := `{"cwd":"` + worktree + `","tool_name":"Write","tool_input":{"file_path":"` + other + `"}}`

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		_ = RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second)
		close(done)
	}()

	waitFor(t, func() bool { return oldestPendingPermission(bRoot) != "" })
	id := oldestPendingPermission(bRoot)
	if id == "" {
		t.Fatal("another project's memory dir must ask the channel")
	}
	if err := resolvePermission(bRoot, id, true, false); err != nil {
		t.Fatal(err)
	}
	<-done
}

// cc- binding 經過 gate 的每一個判定，逐字寫死在這裡。沙盒分支插在
// LoadRegistry 之前並直接 return，所以這些輸出一位元都不該變。任何一條
// 失敗都代表 cc- 的行為被改到了 —— 那是本輪最不能發生的事。
func TestPermissionGateBindingPathUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	seedBinding(t, root, Binding{
		Name: "b", ChannelID: "c1", Worktree: wt, Root: pathIn(root, "bindings", "b"),
	})
	auto := t.TempDir()
	seedBinding(t, root, Binding{
		Name: "auto", ChannelID: "c2", Worktree: auto, Root: pathIn(root, "bindings", "auto"),
		AutoApprove: true,
	})

	for _, c := range []struct{ name, hook, want string }{
		{
			"in-worktree edit",
			`{"cwd":"` + wt + `","tool_name":"Edit","tool_input":{"file_path":"` + wt + `/a.go"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"permission gate: in-worktree edit auto-allowed"}}`,
		},
		{
			"ordinary bash",
			`{"cwd":"` + wt + `","tool_name":"Bash","tool_input":{"command":"git status"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"permission gate: ordinary command auto-allowed"}}`,
		},
		{
			"auto-approve binding",
			`{"cwd":"` + auto + `","tool_name":"mcp__x__y","tool_input":{}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"permission gate: auto-approved (binding bypass)"}}`,
		},
		{
			// cc- 的 fail-open 對未知 cwd 仍然保留 —— 這是既有行為，不在本輪範圍。
			"unknown cwd stays fail-open for cc-",
			`{"cwd":"` + t.TempDir() + `","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"permission gate: no binding for cwd, allowing"}}`,
		},
		{
			"out-of-worktree write times out to deny",
			`{"cwd":"` + wt + `","tool_name":"Write","tool_input":{"file_path":"/etc/hosts"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"權限請求逾時，自動拒絕"}}`,
		},
	} {
		var out bytes.Buffer
		if err := RunPermissionGate(context.Background(), root, strings.NewReader(c.hook), &out, 50*time.Millisecond); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := out.String(); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestPermissionGateWriteOutsideWorktreeAsksChannel(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	seedBinding(t, root, Binding{Name: "b", ChannelID: "c1", Worktree: wt, Root: pathIn(root, "bindings", "b")})

	outside := filepath.Join(t.TempDir(), "skills", "gstack-review", "SKILL.md")
	hookJSON := `{"cwd":"` + wt + `","tool_name":"Write","tool_input":{"file_path":"` + outside + `"}}`

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		_ = RunPermissionGate(context.Background(), root, strings.NewReader(hookJSON), &out, 10*time.Second)
		close(done)
	}()

	bRoot := pathIn(root, "bindings", "b")
	waitFor(t, func() bool { return oldestPendingPermission(bRoot) != "" })
	if n := countJSONFilesSafe(pathIn(bRoot, "outbox", "pending")); n < 1 {
		t.Fatalf("expected out-of-worktree Write to post a permission request, got %d", n)
	}
	id := oldestPendingPermission(bRoot)
	if err := resolvePermission(bRoot, id, true, false); err != nil { // allow once
		t.Fatal(err)
	}
	<-done

	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("gate output = %s", out.String())
	}
}
