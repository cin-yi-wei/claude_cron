package channelagent

import (
	"sync"
	"testing"
)

// WithTasks must serialize read-modify-write. Without it, concurrent
// increments lose updates — the exact bug that let a completed task revert to
// working after its result file had already been consumed.
func TestWithTasksSerializesConcurrentUpdates(t *testing.T) {
	root := t.TempDir()
	if err := SaveTasks(root, TaskStore{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = WithTasks(root, func(s *TaskStore) error {
				s.Upsert(A2ATask{ContextID: string(rune('A' + i%26)), State: TaskWorking})
				return nil
			})
		}(i)
	}
	wg.Wait()

	got, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(got.Tasks) != 26 {
		t.Fatalf("tasks = %d, want 26 distinct contextIds (lost update)", len(got.Tasks))
	}
}

func TestWithTasksDoesNotSaveWhenCallbackErrors(t *testing.T) {
	root := t.TempDir()
	_ = SaveTasks(root, TaskStore{Tasks: []A2ATask{{ContextID: "keep", State: TaskWorking}}})

	wantErr := errTaskAlreadyTerminal
	err := WithTasks(root, func(s *TaskStore) error {
		s.Upsert(A2ATask{ContextID: "should-not-persist", State: TaskWorking})
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	got, _ := LoadTasks(root)
	if len(got.Tasks) != 1 || got.Tasks[0].ContextID != "keep" {
		t.Fatalf("callback error must discard the mutation, got %#v", got.Tasks)
	}
}
