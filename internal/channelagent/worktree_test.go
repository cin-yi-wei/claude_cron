package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	if err := StartTmuxClaude(context.Background(), "cc-proj", cwd); err != nil {
		t.Fatalf("StartTmuxClaude: %v", err)
	}
	wantStart := []string{"tmux", "new-session", "-d", "-s", "cc-proj", "-c", cwd, "claude"}
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
	if err := StartControlSession(context.Background(), "cc-control", cwd, "DISCORD_BOT_TOKEN", "tok123", "SYS PROMPT"); err != nil {
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
	for _, want := range []string{"-e DISCORD_BOT_TOKEN=tok123", "-c " + cwd, "--append-system-prompt", "SYS PROMPT", "claude"} {
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
