package channelagent

import (
	"context"
	"time"
)

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
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, 0, err
	}
	canceled, reclaimed, changed := 0, 0, false

	for i := range tasks.Tasks {
		t := tasks.Tasks[i]
		switch t.State {
		case TaskWorking, TaskSubmitted:
			started, ok := parseRFC3339(t.StartedAt)
			if !ok || now.Sub(started) < HardTimeout {
				continue
			}
			_ = sm.Stop(ctx, t.Session)
			t.State = TaskCanceled
			t.Detail = "hard timeout exceeded"
			t.CompletedAt = now.UTC().Format(time.RFC3339)
			tasks.Tasks[i] = t
			canceled++
			changed = true
		case TaskCompleted:
			done, ok := parseRFC3339(t.CompletedAt)
			if !ok || now.Sub(done) < RetainAfterComplete {
				continue
			}
			if t.Session == "" {
				continue
			}
			_ = sm.Stop(ctx, t.Session)
			t.Session = "" // mark reclaimed; branch is kept
			tasks.Tasks[i] = t
			reclaimed++
			changed = true
		}
	}

	if !changed {
		return canceled, reclaimed, nil
	}
	return canceled, reclaimed, SaveTasks(root, tasks)
}
