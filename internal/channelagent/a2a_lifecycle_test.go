package channelagent

import (
	"context"
	"testing"
)

func TestHasCapacityRespectsCap(t *testing.T) {
	var s TaskStore
	for i := 0; i < MaxConcurrentSandboxes; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), State: TaskWorking})
	}
	if HasCapacity(s) {
		t.Fatalf("should be full at %d active", MaxConcurrentSandboxes)
	}
	s.Tasks[0].State = TaskCompleted
	if !HasCapacity(s) {
		t.Fatal("completing a task should free a slot")
	}
}

func TestMaxConcurrentSandboxesIsEight(t *testing.T) {
	if MaxConcurrentSandboxes != 8 {
		t.Fatalf("MaxConcurrentSandboxes = %d, want 8", MaxConcurrentSandboxes)
	}
}

func TestDrainQueueStartsSubmittedTasksUpToCap(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	// One already working, plus three queued.
	s.Upsert(A2ATask{ContextID: "live", Agent: "a", State: TaskWorking})
	for _, id := range []string{"q1", "q2", "q3"} {
		s.Upsert(A2ATask{ContextID: id, Agent: "a", State: TaskSubmitted, Prompt: "work " + id})
	}
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	stub := &StubExecutor{}
	n, err := DrainQueue(context.Background(), root, stub)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if n != 3 {
		t.Fatalf("started = %d, want 3", n)
	}
	if stub.Calls != 3 {
		t.Fatalf("executor calls = %d", stub.Calls)
	}
	if stub.LastPrompt == "" {
		t.Fatal("queued task started with an empty prompt — Prompt was not persisted")
	}
}

func TestDrainQueueStopsAtCapacity(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	for i := 0; i < MaxConcurrentSandboxes; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), Agent: "a", State: TaskWorking})
	}
	s.Upsert(A2ATask{ContextID: "queued", Agent: "a", State: TaskSubmitted})
	_ = SaveTasks(root, s)

	stub := &StubExecutor{}
	n, err := DrainQueue(context.Background(), root, stub)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if n != 0 || stub.Calls != 0 {
		t.Fatalf("must not start work when full: started=%d calls=%d", n, stub.Calls)
	}
}
