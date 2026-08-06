package channelagent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEnsureWorktreeCreatesBranchWhenMissing(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil // rev-parse "succeeds" => branch exists path
	}

	wt := filepath.Join(t.TempDir(), "does-not-exist", "wt") // os.Stat will fail => proceed
	if err := EnsureWorktree(context.Background(), "/repo", "feat", wt); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	wantProbe := []string{"git", "-C", "/repo", "rev-parse", "--verify", "--quiet", "refs/heads/feat"}
	wantAdd := []string{"git", "-c", "core.hooksPath=/dev/null", "-C", "/repo", "worktree", "add", wt, "feat"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], wantProbe) || !reflect.DeepEqual(calls[1], wantAdd) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestStartTmuxClaudeStartsWhenMissing(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	oldDelay := sessionBootDelay
	sessionBootDelay = 0
	defer func() { sessionBootDelay = oldDelay }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "has-session" {
			return context.Canceled // simulate "no such session"
		}
		return nil
	}
	cwd := t.TempDir()
	if err := StartTmuxClaude(context.Background(), "cc-proj", cwd, "/reg/root"); err != nil {
		t.Fatalf("StartTmuxClaude: %v", err)
	}
	wantStart := []string{"tmux", "new-session", "-d", "-s", "cc-proj", "-c", cwd, "-e", "CC_REGISTRY_ROOT=/reg/root", "env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN", "claude"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], wantStart) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestStartControlSessionInjectsTokenAndPrompt(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	oldDelay := sessionBootDelay
	sessionBootDelay = 0
	defer func() { sessionBootDelay = oldDelay }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "has-session" {
			return context.Canceled // not running yet
		}
		return nil
	}

	cwd := t.TempDir()
	if err := StartControlSession(context.Background(), "cc-control", cwd, "/reg/root", "DISCORD_BOT_TOKEN", "tok123", "SYS PROMPT"); err != nil {
		t.Fatalf("StartControlSession: %v", err)
	}
	var start []string
	for _, c := range calls {
		if len(c) > 1 && c[1] == "new-session" {
			start = c
		}
	}
	if start == nil {
		t.Fatalf("no new-session call: %#v", calls)
	}
	joined := strings.Join(start, " ")
	for _, want := range []string{"-e CC_REGISTRY_ROOT=/reg/root", "-e DISCORD_BOT_TOKEN=tok123", "-c " + cwd, "--append-system-prompt", "SYS PROMPT", "claude"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("new-session missing %q: %v", want, start)
		}
	}
}

func TestEnsureAgentSettingsWritesAllowlist(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureAgentSettings(dir); err != nil {
		t.Fatalf("EnsureAgentSettings: %v", err)
	}
	path := filepath.Join(dir, ".claude", "settings.local.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(data), `"Write"`) || !strings.Contains(string(data), `"Bash"`) {
		t.Fatalf("settings missing expected allowlist entries: %s", data)
	}

	// Existing file is left untouched.
	if err := os.WriteFile(path, []byte(`{"custom":true}`), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := EnsureAgentSettings(dir); err != nil {
		t.Fatalf("EnsureAgentSettings 2: %v", err)
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != `{"custom":true}` {
		t.Fatalf("existing settings should be preserved, got: %s", data2)
	}
}

func TestEnsureProjectRepoAndWipCommitRealGit(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err2 := exec.LookPath("git"); err2 != nil {
			t.Skip("git not available")
		}
	}
	ctx := context.Background()
	proj := filepath.Join(t.TempDir(), "fresh-proj")

	// First call provisions a repo with an initial commit on branch dev.
	if err := EnsureProjectRepo(ctx, proj); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, "README.md")); err != nil {
		t.Fatalf("README not created: %v", err)
	}
	out, err := exec.Command("git", "-C", proj, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "dev" {
		t.Fatalf("branch = %q, want dev", got)
	}
	// Idempotent: a second call on an existing repo is a no-op (no error).
	if err := EnsureProjectRepo(ctx, proj); err != nil {
		t.Fatalf("EnsureProjectRepo (idempotent): %v", err)
	}

	// WipCommit with no changes is a no-op; with changes it commits.
	before, _ := exec.Command("git", "-C", proj, "rev-list", "--count", "HEAD").Output()
	if err := WipCommit(ctx, proj); err != nil {
		t.Fatalf("WipCommit (clean): %v", err)
	}
	mid, _ := exec.Command("git", "-C", proj, "rev-list", "--count", "HEAD").Output()
	if strings.TrimSpace(string(before)) != strings.TrimSpace(string(mid)) {
		t.Fatal("WipCommit on clean tree should not add a commit")
	}
	if err := os.WriteFile(filepath.Join(proj, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WipCommit(ctx, proj); err != nil {
		t.Fatalf("WipCommit (dirty): %v", err)
	}
	after, _ := exec.Command("git", "-C", proj, "rev-list", "--count", "HEAD").Output()
	if strings.TrimSpace(string(mid)) == strings.TrimSpace(string(after)) {
		t.Fatal("WipCommit on dirty tree should add a commit")
	}
}

func TestWaitSessionReadyWaitsForPromptWithoutSendingKeys(t *testing.T) {
	oldRun, oldOut := runExternalCommand, runExternalCommandOutput
	oldDelay, oldSettle := sessionBootDelay, readyProbeSettle
	defer func() {
		runExternalCommand = oldRun
		runExternalCommandOutput = oldOut
		sessionBootDelay = oldDelay
		readyProbeSettle = oldSettle
	}()
	sessionBootDelay = 5 * time.Second
	readyProbeSettle = time.Millisecond

	// A keystroke (sentinel probe or C-c) into a still-booting Claude kills it —
	// the create-path death-loop. waitSessionReady must detect readiness by
	// READING the pane only, never by sending keys.
	var sentKeys bool
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		if name == "tmux" {
			for _, a := range args {
				if a == "send-keys" {
					sentKeys = true
				}
			}
		}
		return nil
	}
	captures := 0
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		captures++
		// Boot splash for the first two probes, then the input prompt renders.
		if captures < 3 {
			return "Welcome to Claude Code\nbooting...", nil
		}
		return "some output\n❯ \n  ? for shortcuts", nil
	}

	waitSessionReady(context.Background(), "cc-x")
	if captures < 3 {
		t.Fatalf("expected at least 3 capture probes, got %d", captures)
	}
	if sentKeys {
		t.Fatal("waitSessionReady must NOT send any keys during boot")
	}
}

func TestWaitSessionReadySkippedWhenDelayZero(t *testing.T) {
	oldOut := runExternalCommandOutput
	oldDelay := sessionBootDelay
	defer func() {
		runExternalCommandOutput = oldOut
		sessionBootDelay = oldDelay
	}()
	sessionBootDelay = 0
	called := false
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		called = true
		return "", nil
	}
	waitSessionReady(context.Background(), "cc-x")
	if called {
		t.Fatal("waitSessionReady must not probe when sessionBootDelay <= 0")
	}
}

// 沙盒 worktree 的 settings 不可以有 SessionStart hook:Claude Code 開機時會
// 因此跳「Managed settings require approval」閘,而該畫面被 classifyScreen 判為
// ScreenLogin,autoAnswerSandboxConfirm 永遠不會答到它 —— prompt 就被打進核准
// 畫面然後消失。cc- worker 的 agentSettings 一個字都不能動。
func TestSandboxSettingsDropSessionStartHookOnly(t *testing.T) {
	worker := t.TempDir()
	if err := EnsureAgentSettings(worker); err != nil {
		t.Fatal(err)
	}
	workerBlob, err := os.ReadFile(filepath.Join(worker, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workerBlob), "SessionStart") {
		t.Fatal("cc- worker settings must still carry the SessionStart hook")
	}

	sandbox := t.TempDir()
	if err := EnsureSandboxSettings(sandbox); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(sandbox, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(blob)
	if strings.Contains(got, "SessionStart") {
		t.Fatal("sandbox settings must NOT carry the SessionStart hook")
	}
	// 六條 PreToolUse matcher 一條不少 —— 少一條就是一個沒有 gate 的工具。
	for _, m := range []string{`"Edit"`, `"Write"`, `"Bash"`, `"WebFetch"`, `"WebSearch"`, `"mcp__.*"`} {
		if !strings.Contains(got, m) {
			t.Errorf("sandbox settings lost the %s matcher", m)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(blob, &parsed); err != nil {
		t.Fatalf("sandbox settings are not valid JSON: %v", err)
	}
}

// TestEnsureSandboxSettingsRewritesStaleSessionStartHook pins the upgrade
// path: writeAgentSettings/EnsureAgentSettings no-op when a
// settings.local.json already exists, and EnsureWorktree no-ops when the
// worktree already exists — so a sandbox worktree created by a pre-fix
// binary would otherwise keep its SessionStart-bearing settings forever and
// hang exactly like the original bug on every future boot. EnsureSandboxSettings
// must detect that stale content and rewrite it, unlike the cc-/control
// writers (which must stay untouched — see
// TestSandboxSettingsDropSessionStartHookOnly for that guarantee).
func TestEnsureSandboxSettingsRewritesStaleSessionStartHook(t *testing.T) {
	sandbox := t.TempDir()
	// Simulate a worktree created by the pre-fix binary: it wrote the WORKER
	// settings (agentSettings, complete with SessionStart) into what is now
	// treated as a sandbox worktree.
	if err := EnsureAgentSettings(sandbox); err != nil {
		t.Fatalf("seed stale worker settings: %v", err)
	}
	settingsPath := filepath.Join(sandbox, ".claude", "settings.local.json")
	stale, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stale), "SessionStart") {
		t.Fatal("fixture setup is wrong: seeded settings should carry SessionStart")
	}

	if err := EnsureSandboxSettings(sandbox); err != nil {
		t.Fatalf("EnsureSandboxSettings: %v", err)
	}
	rewritten, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "SessionStart") {
		t.Fatal("EnsureSandboxSettings left a stale SessionStart hook in place — an upgraded binary would hang on this worktree exactly like the original bug")
	}
	for _, m := range []string{`"Edit"`, `"Write"`, `"Bash"`, `"WebFetch"`, `"WebSearch"`, `"mcp__.*"`} {
		if !strings.Contains(string(rewritten), m) {
			t.Errorf("rewritten sandbox settings lost the %s matcher", m)
		}
	}

	// A second call on the now-correct sandbox settings must be a genuine
	// no-op — EnsureSandboxSettings must not thrash the file every boot.
	if err := os.Chmod(settingsPath, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureSandboxSettings(sandbox); err != nil {
		t.Fatalf("EnsureSandboxSettings (idempotent): %v", err)
	}
	after, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("EnsureSandboxSettings rewrote an already-correct sandbox settings file")
	}
}

// TestEnsureSandboxSettingsIgnoresSessionStartMentionOutsideHooks pins the
// structural check: detection must be "does hooks.SessionStart exist as a
// key", not "does the string SessionStart appear anywhere in the file". A
// file that merely mentions the word (e.g. in an unrelated comment-like
// field) must be left completely untouched — a raw substring scan would
// wrongly rewrite it.
func TestEnsureSandboxSettingsIgnoresSessionStartMentionOutsideHooks(t *testing.T) {
	sandbox := t.TempDir()
	settingsPath := filepath.Join(sandbox, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const content = `{"note":"no SessionStart hook configured here","hooks":{"PreToolUse":[]}}`
	if err := os.WriteFile(settingsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := EnsureSandboxSettings(sandbox); err != nil {
		t.Fatalf("EnsureSandboxSettings: %v", err)
	}

	after, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("EnsureSandboxSettings rewrote a file that only MENTIONS SessionStart, not one that actually has a hooks.SessionStart key — detection must be structural, not a substring scan")
	}
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("content changed: got %s, want unchanged %s", got, content)
	}
}

// TestEnsureSandboxSettingsRewritePreservesUnrelatedKeys pins the
// "delete only hooks.SessionStart, don't clobber the rest" requirement: a
// wholesale overwrite with sandboxAgentSettings would silently drop any
// hand-added key sitting alongside SessionStart (a custom top-level setting,
// or a sibling hook type). The targeted delete must leave everything else —
// including keys stripSessionStartHook has never heard of — exactly in
// place.
func TestEnsureSandboxSettingsRewritePreservesUnrelatedKeys(t *testing.T) {
	sandbox := t.TempDir()
	settingsPath := filepath.Join(sandbox, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const content = `{
  "customToolConfig": {"handAdded": true},
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "claude-cron session-hook"}]}],
    "PostToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "some-other-tool"}]}]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSandboxSettings(sandbox); err != nil {
		t.Fatalf("EnsureSandboxSettings: %v", err)
	}

	blob, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(blob, &parsed); err != nil {
		t.Fatalf("rewritten file is not valid JSON: %v", err)
	}
	custom, ok := parsed["customToolConfig"].(map[string]any)
	if !ok || custom["handAdded"] != true {
		t.Fatalf("hand-added top-level key was dropped by the rewrite: %#v", parsed["customToolConfig"])
	}
	hooks, ok := parsed["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks key was dropped by the rewrite: %#v", parsed)
	}
	if _, has := hooks["SessionStart"]; has {
		t.Fatal("hooks.SessionStart must be gone after the rewrite")
	}
	if _, has := hooks["PostToolUse"]; !has {
		t.Fatal("a sibling hook type (PostToolUse) was dropped by the rewrite — only SessionStart should be removed")
	}
}
