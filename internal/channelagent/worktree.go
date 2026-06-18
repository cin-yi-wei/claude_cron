package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionBootDelay bounds how long waitSessionReady probes a freshly-created
// tmux Claude session for input readiness. A blind fixed delay was too short on
// cold start: the first injected prompt raced the Claude TUI boot splash, the
// keystrokes dropped, and the job stalled until its 120s timeout. waitSessionReady
// returns as soon as the session echoes a sentinel (usually a few seconds), so
// this is an upper bound, not a fixed cost. Set to 0 in tests to skip probing.
var sessionBootDelay = 30 * time.Second

// readyProbeSettle is the pause between typing the readiness sentinel and
// capturing the pane to look for its echo.
var readyProbeSettle = 500 * time.Millisecond

// waitSessionReady blocks until a freshly-created tmux Claude session is actually
// accepting keystrokes, by repeatedly typing a sentinel and checking it echoes in
// the pane, then clearing it. This replaces a blind boot delay that dropped the
// first inject on cold start. No-op when sessionBootDelay <= 0 (tests).
func waitSessionReady(ctx context.Context, session string) {
	if sessionBootDelay <= 0 {
		return
	}
	const sentinel = "__cc_ready_probe__"
	start := time.Now()
	for time.Since(start) < sessionBootDelay {
		time.Sleep(readyProbeSettle)
		_ = runExternalCommand(ctx, "tmux", "send-keys", "-t", session, "-l", sentinel)
		time.Sleep(readyProbeSettle)
		pane, err := runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", session)
		if err == nil && strings.Contains(pane, sentinel) {
			// Ready: clear the sentinel so it doesn't pollute the first prompt.
			_ = runExternalCommand(ctx, "tmux", "send-keys", "-t", session, "C-c")
			time.Sleep(readyProbeSettle)
			return
		}
	}
}

// agentSettings is the Claude Code permission allowlist written into each
// binding's worktree so the driven agent can read the job, write its reply, and
// rename the output file without interactive permission prompts (which a
// tmux-driven session cannot answer). Scoped to the tools the agent prompt uses.
const agentSettings = `{
  "permissions": {
    "allow": ["Read", "Write", "Edit"]
  },
  "hooks": {
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "claude-cron permission-gate" } ] },
      { "matcher": "mcp__.*", "hooks": [ { "type": "command", "command": "claude-cron permission-gate" } ] }
    ]
  }
}
`

// EnsureAgentSettings writes .claude/settings.local.json into dir if it does not
// already exist, so a freshly-created worktree grants the agent the permissions
// it needs to run unattended. An existing file is left untouched.
func EnsureAgentSettings(dir string) error {
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if _, err := os.Stat(settingsPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, []byte(agentSettings), 0o644)
}

// gitIdentity supplies a fallback committer so commits work even when the host
// has no global git user configured. Real per-repo identity, if set, wins.
var gitIdentity = []string{"-c", "user.name=claude_cron", "-c", "user.email=claude_cron@localhost"}

// EnsureProjectRepo makes sure projectDir exists and is a git repo, so a binding
// can be created against a brand-new project. Existing repos are a no-op. A fresh
// project is created with `git init -b dev`, seeded with a README, and given one
// initial commit so a branch (and HEAD) exists for `git worktree add` to fork.
func EnsureProjectRepo(ctx context.Context, projectDir string) error {
	if runExternalCommand(ctx, "git", "-C", projectDir, "rev-parse", "--git-dir") == nil {
		return nil
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return err
	}
	if err := runExternalCommand(ctx, "git", "-C", projectDir, "init", "-b", "dev"); err != nil {
		return err
	}
	readme := filepath.Join(projectDir, "README.md")
	if _, err := os.Stat(readme); err != nil {
		if werr := os.WriteFile(readme, []byte("# "+filepath.Base(projectDir)+"\n"), 0o644); werr != nil {
			return werr
		}
	}
	if err := runExternalCommand(ctx, "git", "-C", projectDir, "add", "-A"); err != nil {
		return err
	}
	args := append([]string{"-c", "core.hooksPath=/dev/null"}, gitIdentity...)
	args = append(args, "-C", projectDir, "commit", "-m", "chore: init project (claude_cron)")
	return runExternalCommand(ctx, "git", args...)
}

// WipCommit commits any uncommitted changes in worktree onto its current branch
// before the worktree is removed on /unbind, so in-flight work is preserved on
// the branch (which lives in the shared main repo). No-op if the worktree is gone
// or has nothing to commit.
func WipCommit(ctx context.Context, worktree string) error {
	if _, err := os.Stat(worktree); err != nil {
		return nil
	}
	_ = runExternalCommand(ctx, "git", "-C", worktree, "add", "-A")
	// diff --cached --quiet exits 0 when nothing is staged → nothing to commit.
	if runExternalCommand(ctx, "git", "-C", worktree, "diff", "--cached", "--quiet") == nil {
		return nil
	}
	args := append([]string{"-c", "core.hooksPath=/dev/null"}, gitIdentity...)
	args = append(args, "-C", worktree, "commit", "-m", "wip: claude_cron unbind snapshot")
	return runExternalCommand(ctx, "git", args...)
}

// EnsureWorktree makes sure worktreePath is a git worktree of branch, checked
// out from projectDir. Idempotent: if worktreePath already exists it is a no-op.
// If the branch does not exist yet it is created from current HEAD.
func EnsureWorktree(ctx context.Context, projectDir, branch, worktreePath string) error {
	if _, err := os.Stat(worktreePath); err == nil {
		return nil
	}
	branchExists := runExternalCommand(ctx, "git", "-C", projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
	// `-c core.hooksPath=/dev/null` disables repo git hooks for this command. A
	// `worktree add` runs the post-checkout hook, which fails (non-zero exit) in
	// repos using hook frameworks like Overcommit when their tooling is not
	// installed — that failure should not block provisioning. worktreePath must
	// be absolute: git resolves a relative path against the -C directory, not the
	// caller's cwd, which would place the worktree inside the project repo.
	if branchExists {
		return runExternalCommand(ctx, "git", "-c", "core.hooksPath=/dev/null", "-C", projectDir, "worktree", "add", worktreePath, branch)
	}
	return runExternalCommand(ctx, "git", "-c", "core.hooksPath=/dev/null", "-C", projectDir, "worktree", "add", "-b", branch, worktreePath)
}

// RemoveWorktree removes a git worktree. Force is used so dirty worktrees are
// still cleaned up on /unbind.
func RemoveWorktree(ctx context.Context, projectDir, worktreePath string) error {
	err := runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "remove", "--force", worktreePath)
	// Prune any stale registration, then make sure the directory is actually
	// gone — `git worktree remove` can leave the dir/registration behind (busy
	// session, gitdir pointer issues), which used to orphan worktrees on unbind.
	_ = runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "prune")
	if _, statErr := os.Stat(worktreePath); statErr == nil {
		if rmErr := os.RemoveAll(worktreePath); rmErr != nil && err == nil {
			err = rmErr
		}
		_ = runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "prune")
	}
	return err
}

// StartTmuxClaude ensures a detached tmux session named session is running
// `claude` with its working directory set to cwd. No-op if it already exists.
func StartTmuxClaude(ctx context.Context, session, cwd, registryRoot string) error {
	if err := EnsureAgentSettings(cwd); err != nil {
		return err
	}
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	// CC_REGISTRY_ROOT lets the PreToolUse permission-gate hook find the registry
	// (to resolve this worktree's binding + channel) without per-binding config.
	// claudeArgs resumes the latest transcript so a (re)created session — on
	// reap, serve restart, or reboot — keeps its prior conversation.
	args := append([]string{"new-session", "-d", "-s", session, "-c", cwd, "-e", "CC_REGISTRY_ROOT=" + registryRoot}, claudeArgs(cwd)...)
	if err := runExternalCommand(ctx, "tmux", args...); err != nil {
		return err
	}
	waitSessionReady(ctx, session)
	return nil
}

// encodeProjectDir maps an absolute path to Claude Code's project-history dir
// name: every non-alphanumeric character becomes '-'.
func encodeProjectDir(p string) string {
	var b strings.Builder
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// latestTranscript returns the id of the most recent Claude transcript for a
// session whose cwd is worktree, or "" if none.
func latestTranscript(worktree string) string {
	abs := worktree
	if a, err := filepath.Abs(worktree); err == nil {
		abs = a
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "projects", encodeProjectDir(abs)))
	if err != nil {
		return ""
	}
	var newest string
	var newestT time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestT) {
			newestT = info.ModTime()
			newest = strings.TrimSuffix(e.Name(), ".jsonl")
		}
	}
	return newest
}

// claudeArgs builds the `claude ...` tail for a tmux launch, resuming the latest
// transcript for cwd when one exists. extra is appended after (e.g. flags).
func claudeArgs(cwd string, extra ...string) []string {
	args := []string{"claude"}
	if id := latestTranscript(cwd); id != "" {
		args = append(args, "--resume", id)
	}
	return append(args, extra...)
}

// StartControlSession starts the control channel's AI assistant session: a
// detached tmux session running `claude` with the given system prompt appended
// and the Discord bot token injected into the session environment (so the
// assistant's `claude-cron` management calls can authenticate). No-op if the
// session already exists. tokenEnv is the env var name, tokenValue its value.
func StartControlSession(ctx context.Context, session, cwd, tokenEnv, tokenValue, systemPrompt string) error {
	if err := EnsureAgentSettings(cwd); err != nil {
		return err
	}
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	base := []string{"new-session", "-d", "-s", session, "-c", cwd}
	if tokenEnv != "" {
		// A web control plane has no bot token; only inject -e when there is one.
		base = append(base, "-e", tokenEnv+"="+tokenValue)
	}
	args := append(base, claudeArgs(cwd, "--append-system-prompt", systemPrompt)...)
	if err := runExternalCommand(ctx, "tmux", args...); err != nil {
		return err
	}
	waitSessionReady(ctx, session)
	return nil
}

// StopTmuxSession kills a tmux session. A missing session is not an error.
func StopTmuxSession(ctx context.Context, session string) error {
	_ = runExternalCommand(ctx, "tmux", "kill-session", "-t", session)
	return nil
}
