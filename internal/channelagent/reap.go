package channelagent

import (
	"context"
	"strings"
)

// orphanCCSessions returns the cc-* tmux session names that are not in valid.
// Only "cc-" sessions are considered (binding/control sessions); anything else
// (cron-serve, a user's own session) is left alone.
func orphanCCSessions(names []string, valid map[string]bool) []string {
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || !strings.HasPrefix(n, "cc-") {
			continue
		}
		if !valid[n] {
			out = append(out, n)
		}
	}
	return out
}

// reapOrphanSessions kills any cc-* tmux session that has no matching binding
// (valid holds the session names that should exist: cc-control + cc-<binding>).
// This closes the unbind race: the CLI `unbind` stops a session, but an
// in-flight serve cycle can re-StartTmuxClaude it; the next cycle's reap removes
// the orphan because its binding is gone from the registry.
func reapOrphanSessions(ctx context.Context, valid map[string]bool) []string {
	out, err := runExternalCommandOutput(ctx, "tmux", "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil // no tmux server / no sessions
	}
	orphans := orphanCCSessions(strings.Split(out, "\n"), valid)
	for _, s := range orphans {
		_ = runExternalCommand(ctx, "tmux", "kill-session", "-t", s)
	}
	return orphans
}
