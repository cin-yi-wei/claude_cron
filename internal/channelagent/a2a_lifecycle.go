package channelagent

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"time"
)

// errNothingSwept signals, from inside a WithTasks callback, that this sweep
// pass changed no task. Returning it makes WithTasks discard the (empty)
// mutation and skip the save, matching the pre-lock behaviour of never
// writing tasks.json when there was nothing to change.
var errNothingSwept = errors.New("a2a: sweep found nothing to change")

// MaxConcurrentSandboxes caps simultaneous aa-*-<ctx> instances. Industry
// guidance for parallel agent worktrees is 8-10; 8 is the conservative end and
// also bounds memory, which has run tight on this host.
const MaxConcurrentSandboxes = 8

// HasCapacity gates on RunningCount, not ActiveCount: a submitted task has no
// sandbox yet, so it must not count against the very capacity it is waiting
// for. Gating on ActiveCount would deadlock permanently once nothing is
// running — a pile of queued work would read as "full" forever, since
// nothing running means nothing can ever complete to free a slot.
func HasCapacity(s TaskStore) bool {
	return s.RunningCount() < MaxConcurrentSandboxes
}

// DrainQueue starts queued (submitted) tasks while slots remain. Overflow
// stays queued rather than being rejected.
//
// Capacity used is tracked as baseline (RunningCount at the top of this call)
// plus started (successful Start calls made so far in this loop), not by
// reloading the store and re-deriving RunningCount on every iteration. The
// TaskExecutor interface makes no promise that Start synchronously persists
// TaskWorking before returning — StubExecutor deliberately does not — so a
// reload-based re-check can't tell "this call already started this task" from
// "this task is still queued," and would keep reporting spare capacity that
// this same call has already committed. Tracking the count locally consumes
// a slot exactly once per successful start, regardless of what the executor
// does or doesn't write to disk.
func DrainQueue(ctx context.Context, root string, ex TaskExecutor) (int, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, err
	}
	running := tasks.RunningCount()
	started := 0
	for _, t := range tasks.Tasks {
		if t.State != TaskSubmitted {
			continue
		}
		if running+started >= MaxConcurrentSandboxes {
			break
		}
		if err := ex.Start(ctx, t, t.Prompt); err != nil {
			continue // executor already recorded the failure
		}
		started++
	}
	return started, nil
}

const (
	// SoftTimeout only flips reporting: A2A natively supports long-running
	// tasks, and real agent work routinely exceeds half an hour.
	SoftTimeout = 30 * time.Minute
	// HardTimeout is the backstop against a wedged sandbox holding a worktree
	// and memory forever.
	HardTimeout = 2 * time.Hour
	// RetainAfterComplete keeps the sandbox alive briefly so the caller can ask
	// a follow-up in the same contextId without paying to rebuild it.
	RetainAfterComplete = 10 * time.Minute
)

// MaxRetainedFailedSandboxes bounds the forensics rule. Keeping every failed
// sandbox forever is itself an unbounded disk-growth path; the newest are the
// ones worth inspecting.
const MaxRetainedFailedSandboxes = 20

func parseRFC3339(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// removal is one workspace this sweep decided to tear down: the git worktree
// (if any) plus its sandbox root (inbox/outbox/locks), keyed by session.
// projectDir may be empty when the owning agent can no longer be resolved
// (deleted since the task ran); RemoveWorktree still falls back to deleting
// the worktree directory directly in that case.
type removal struct {
	projectDir, worktree, session string
}

// SweepTimeouts cancels tasks past HardTimeout and tears down completed
// sandboxes past RetainAfterComplete, reclaiming their worktree and sandbox
// root — not just the tmux session. Failed sandboxes are deliberately left in
// place for forensics, but that exemption is capped at
// MaxRetainedFailedSandboxes: the oldest failures beyond the cap are reclaimed
// the same way, keeping only the newest (most likely to still be worth
// inspecting).
//
// canceled and reclaimed count tasks whose STATE was transitioned by this
// sweep, not tasks whose disk footprint was verifiably removed: the state
// mutation is committed under the tasks-file lock first, and the actual
// tmux/git/filesystem teardown happens afterwards and best-effort, matching
// the pre-existing sm.Stop handling below. A removal failure is logged and
// swallowed rather than retried or surfaced as a sweep error — the next sweep
// pass will simply try the same (by-then-already-cleared) path again, which
// is harmless. This mirrors sm.Stop's existing "_ = sm.Stop(...)" contract:
// the counts reflect bookkeeping intent, and disk state is reconciled
// best-effort rather than transactionally.
func SweepTimeouts(ctx context.Context, root string, sm SessionManager, now time.Time) (int, int, error) {
	canceled, reclaimed := 0, 0
	// Sessions to stop and workspaces to remove are collected under the lock
	// but acted on after it is released: sm.Stop/RemoveWorkspace shell out to
	// tmux and git, and os.RemoveAll touches the filesystem — nothing that
	// touches a session, process, or disk may run while tasksMu is held (the
	// same rule the executor's dispatch follows for the same reason).
	var toStop []string
	var toRemove []removal

	// A task only records its owning agent's name, not its ProjectDir, so
	// resolve it here the same way SandboxExecutor.Start does. A load failure
	// must not block cancellation/reclaim bookkeeping — that is the safety-
	// critical half of this sweep — so fall back to an empty AgentStore and
	// let projectDirFor return "" per task; RemoveWorktree's directory-delete
	// fallback still tears down the worktree even without it.
	agents, aerr := LoadAgents(root)
	if aerr != nil {
		log.Printf("a2a: sweep: load agents: %v (worktree removal will use an empty project dir)", aerr)
		agents = AgentStore{}
	}
	projectDirFor := func(agentName string) string {
		if a, ok := agents.Get(agentName); ok {
			return a.ProjectDir
		}
		return ""
	}

	err := WithTasks(root, func(tasks *TaskStore) error {
		changed := false
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			switch t.State {
			case TaskWorking, TaskSubmitted:
				if !CanTransition(t.State, TaskCanceled) {
					continue
				}
				// A missing or corrupt StartedAt must NOT pin this task's
				// sandbox forever: HardTimeout exists precisely as the backstop
				// against a wedged sandbox, and a task with unreadable state is
				// among the most likely to be wedged. Treat it as sweep-eligible
				// immediately (we cannot compute elapsed time), but record and
				// log a distinct reason so an operator can tell a corrupt-data
				// cancel apart from an ordinary hard timeout.
				var reason string
				started, ok := parseRFC3339(t.StartedAt)
				switch {
				case !ok:
					reason = "start time unreadable (missing or corrupt StartedAt); canceled as a hard-timeout backstop"
					log.Printf("a2a: sweep: task %s (session %s) has an unparseable StartedAt %q; canceling rather than leaving it unreclaimable forever", t.ContextID, t.Session, t.StartedAt)
				case now.Sub(started) >= HardTimeout:
					reason = "hard timeout exceeded"
				default:
					continue
				}
				toStop = append(toStop, t.Session)
				t.State = TaskCanceled
				t.Detail = reason
				t.CompletedAt = now.UTC().Format(time.RFC3339)
				tasks.Tasks[i] = t
				canceled++
				changed = true
			case TaskCompleted:
				// Same reasoning on the retention path: an unparseable
				// CompletedAt must make the task reclaim-eligible rather than
				// pinning its sandbox forever. TaskFailed is a separate switch
				// case (or rather, no case at all below) and is therefore never
				// touched here — the forensics exemption is untouched by this.
				done, ok := parseRFC3339(t.CompletedAt)
				if ok && now.Sub(done) < RetainAfterComplete {
					continue
				}
				if !ok {
					log.Printf("a2a: sweep: task %s (session %s) has an unparseable CompletedAt %q; reclaiming its sandbox rather than pinning it forever", t.ContextID, t.Session, t.CompletedAt)
				}
				if t.Session == "" {
					continue
				}
				toStop = append(toStop, t.Session)
				if t.Worktree != "" {
					toRemove = append(toRemove, removal{projectDirFor(t.Agent), t.Worktree, t.Session})
				}
				t.Session = ""  // mark reclaimed; branch is kept
				t.Worktree = "" // the worktree is being removed below; nothing left to track
				tasks.Tasks[i] = t
				reclaimed++
				changed = true
			}
		}

		// Cap the forensics exemption: failed sandboxes are kept on purpose,
		// but not without bound. Only consider ones that still hold a
		// worktree — a task already trimmed by a prior sweep pass has
		// Worktree == "" and must not be re-counted or re-removed.
		type failedCandidate struct {
			idx  int
			done time.Time // zero value (unparseable/missing) sorts as oldest
		}
		var failed []failedCandidate
		for i, t := range tasks.Tasks {
			if t.State != TaskFailed || t.Worktree == "" {
				continue
			}
			done, _ := parseRFC3339(t.CompletedAt)
			failed = append(failed, failedCandidate{i, done})
		}
		if len(failed) > MaxRetainedFailedSandboxes {
			sort.Slice(failed, func(a, b int) bool { return failed[a].done.Before(failed[b].done) })
			trim := len(failed) - MaxRetainedFailedSandboxes
			for _, fc := range failed[:trim] {
				t := tasks.Tasks[fc.idx]
				if t.Session != "" {
					toStop = append(toStop, t.Session)
				}
				toRemove = append(toRemove, removal{projectDirFor(t.Agent), t.Worktree, t.Session})
				t.Worktree = ""
				t.Session = ""
				tasks.Tasks[fc.idx] = t
				reclaimed++
				changed = true
			}
		}

		if !changed {
			return errNothingSwept
		}
		return nil
	})

	for _, session := range toStop {
		_ = sm.Stop(ctx, session)
	}
	for _, r := range toRemove {
		if r.worktree != "" {
			if rmErr := sm.RemoveWorkspace(ctx, r.projectDir, r.worktree); rmErr != nil {
				log.Printf("a2a: sweep: failed to remove worktree %s for session %s: %v", r.worktree, r.session, rmErr)
			}
		}
		if r.session != "" {
			if rmErr := os.RemoveAll(SandboxRoot(root, r.session)); rmErr != nil {
				log.Printf("a2a: sweep: failed to remove sandbox root for session %s: %v", r.session, rmErr)
			}
		}
	}

	if errors.Is(err, errNothingSwept) {
		return canceled, reclaimed, nil
	}
	return canceled, reclaimed, err
}
