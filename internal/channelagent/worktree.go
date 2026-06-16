package channelagent

import (
	"context"
	"os"
	"path/filepath"
)

// agentSettings is the Claude Code permission allowlist written into each
// binding's worktree so the driven agent can read the job, write its reply, and
// rename the output file without interactive permission prompts (which a
// tmux-driven session cannot answer). Scoped to the tools the agent prompt uses.
const agentSettings = `{
  "permissions": {
    "allow": ["Read", "Write", "Edit", "Bash(mv:*)", "Bash(ls:*)", "Bash(rtk:*)", "Bash(git:*)"]
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
	return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "remove", "--force", worktreePath)
}

// StartTmuxClaude ensures a detached tmux session named session is running
// `claude` with its working directory set to cwd. No-op if it already exists.
func StartTmuxClaude(ctx context.Context, session, cwd string) error {
	if err := EnsureAgentSettings(cwd); err != nil {
		return err
	}
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	return runExternalCommand(ctx, "tmux", "new-session", "-d", "-s", session, "-c", cwd, "claude")
}

// StopTmuxSession kills a tmux session. A missing session is not an error.
func StopTmuxSession(ctx context.Context, session string) error {
	_ = runExternalCommand(ctx, "tmux", "kill-session", "-t", session)
	return nil
}
