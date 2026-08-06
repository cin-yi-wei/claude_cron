package channelagent

import "sync"

// tasksMu serializes read-modify-write cycles on tasks.json. AtomicWriteJSON
// prevents torn files but not lost updates: the HTTP handler, the executor,
// CollectResults and SweepTimeouts all load, mutate and save, and after the
// listener and the lifecycle loop began running concurrently a stale snapshot
// could overwrite a completion whose result file had already been consumed —
// leaving a task that can never finish. Only `serve` writes tasks.json, so an
// in-process mutex is sufficient.
var tasksMu sync.Mutex

// WithTasks runs fn against the current task store under the lock and saves the
// result. If fn returns an error, nothing is written.
//
// Callers MUST NOT perform slow work inside fn — in particular never dispatch
// to an executor, whose session start can block for a minute or more. Holding
// this lock across that would stall every other sandbox.
func WithTasks(root string, fn func(*TaskStore) error) error {
	tasksMu.Lock()
	defer tasksMu.Unlock()

	tasks, err := LoadTasks(root)
	if err != nil {
		return err
	}
	if err := fn(&tasks); err != nil {
		return err
	}
	return SaveTasks(root, tasks)
}
