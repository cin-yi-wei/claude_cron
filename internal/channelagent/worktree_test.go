package channelagent

import (
	"context"
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
	wantStart := []string{"tmux", "new-session", "-d", "-s", "cc-proj", "-c", cwd, "-e", "CC_REGISTRY_ROOT=/reg/root", "env", "-u", "ANTHROPIC_API_KEY", "claude"}
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

func TestWaitSessionReadyProbesUntilEcho(t *testing.T) {
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

	var sawClear bool
	captures := 0
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		// Detect the sentinel-clearing C-c after readiness.
		for _, a := range args {
			if a == "C-c" {
				sawClear = true
			}
		}
		return nil
	}
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		captures++
		// Not ready for the first two probes, then the sentinel echoes.
		if captures < 3 {
			return "booting...", nil
		}
		return "some pane __cc_ready_probe__ here", nil
	}

	waitSessionReady(context.Background(), "cc-x")
	if captures < 3 {
		t.Fatalf("expected at least 3 capture probes, got %d", captures)
	}
	if !sawClear {
		t.Fatal("expected a C-c to clear the sentinel once ready")
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
