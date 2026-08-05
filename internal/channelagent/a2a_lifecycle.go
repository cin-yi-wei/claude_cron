package channelagent

import "context"

// MaxConcurrentSandboxes caps simultaneous aa-*-<ctx> instances. Industry
// guidance for parallel agent worktrees is 8-10; 8 is the conservative end and
// also bounds memory, which has run tight on this host.
const MaxConcurrentSandboxes = 8

func HasCapacity(s TaskStore) bool {
	return s.ActiveCount() < MaxConcurrentSandboxes
}

// DrainQueue starts queued (submitted) tasks while slots remain. Overflow stays
// queued rather than being rejected.
func DrainQueue(ctx context.Context, root string, ex TaskExecutor) (int, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, t := range tasks.Tasks {
		if t.State != TaskSubmitted {
			continue
		}
		cur, err := LoadTasks(root)
		if err != nil {
			return started, err
		}
		if !HasCapacity(cur) {
			break
		}
		if err := ex.Start(ctx, t, t.Prompt); err != nil {
			continue // executor already recorded the failure
		}
		started++
	}
	return started, nil
}
