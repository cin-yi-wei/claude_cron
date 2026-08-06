package channelagent

import (
	"context"
	"errors"
	"log"
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

func parseRFC3339(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// SweepTimeouts cancels tasks past HardTimeout and tears down completed
// sandboxes past RetainAfterComplete. Failed sandboxes are deliberately left in
// place for forensics.
func SweepTimeouts(ctx context.Context, root string, sm SessionManager, now time.Time) (int, int, error) {
	canceled, reclaimed := 0, 0
	// Sessions to stop are collected under the lock but stopped after it is
	// released: sm.Stop shells out to tmux, and nothing that touches a
	// session or process may run while tasksMu is held (the same rule the
	// executor's dispatch follows for the same reason).
	var toStop []string

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
				t.Session = "" // mark reclaimed; branch is kept
				tasks.Tasks[i] = t
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

	if errors.Is(err, errNothingSwept) {
		return canceled, reclaimed, nil
	}
	return canceled, reclaimed, err
}
