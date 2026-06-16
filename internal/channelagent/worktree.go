package channelagent

import (
	"context"
	"os"
)

// EnsureWorktree makes sure worktreePath is a git worktree of branch, checked
// out from projectDir. Idempotent: if worktreePath already exists it is a no-op.
// If the branch does not exist yet it is created from current HEAD.
func EnsureWorktree(ctx context.Context, projectDir, branch, worktreePath string) error {
	if _, err := os.Stat(worktreePath); err == nil {
		return nil
	}
	branchExists := runExternalCommand(ctx, "git", "-C", projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
	if branchExists {
		return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "add", worktreePath, branch)
	}
	return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "add", "-b", branch, worktreePath)
}

// RemoveWorktree removes a git worktree. Force is used so dirty worktrees are
// still cleaned up on /unbind.
func RemoveWorktree(ctx context.Context, projectDir, worktreePath string) error {
	return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "remove", "--force", worktreePath)
}

// StartTmuxClaude ensures a detached tmux session named session is running
// `claude` with its working directory set to cwd. No-op if it already exists.
func StartTmuxClaude(ctx context.Context, session, cwd string) error {
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
