package channelagent

import "context"

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
