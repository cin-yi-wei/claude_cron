package channelagent

import (
	"strings"
	"testing"
)

func TestSessionNameForIsPrefixedAndSanitised(t *testing.T) {
	got := SessionNameFor("codereview", "ctx/with:weird chars")
	if !strings.HasPrefix(got, "aa-codereview-") {
		t.Fatalf("session name must start with aa-<agent>-, got %q", got)
	}
	if strings.ContainsAny(got, "/: ") {
		t.Fatalf("session name not sanitised: %q", got)
	}
	if strings.HasPrefix(got, "cc-") {
		t.Fatal("A2A sessions must never use the cc- prefix")
	}
}

func TestCanTransitionAllowsForwardOnly(t *testing.T) {
	ok := [][2]TaskState{
		{TaskSubmitted, TaskWorking},
		{TaskWorking, TaskCompleted},
		{TaskWorking, TaskFailed},
		{TaskWorking, TaskCanceled},
		{TaskSubmitted, TaskFailed},
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("CanTransition(%s,%s) = false, want true", c[0], c[1])
		}
	}
	bad := [][2]TaskState{
		{TaskCompleted, TaskWorking},
		{TaskFailed, TaskCompleted},
		{TaskCanceled, TaskWorking},
		{TaskCompleted, TaskCompleted},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("CanTransition(%s,%s) = true, want false", c[0], c[1])
		}
	}
}

func TestTaskStoreUpsertReplacesByContext(t *testing.T) {
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", State: TaskSubmitted})
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", State: TaskWorking})
	if len(s.Tasks) != 1 {
		t.Fatalf("Upsert must replace, got %d tasks", len(s.Tasks))
	}
	got, ok := s.ByContext("c1")
	if !ok || got.State != TaskWorking {
		t.Fatalf("ByContext = %#v, %v", got, ok)
	}
}

func TestActiveCountIgnoresTerminalStates(t *testing.T) {
	s := TaskStore{Tasks: []A2ATask{
		{ContextID: "a", State: TaskSubmitted},
		{ContextID: "b", State: TaskWorking},
		{ContextID: "c", State: TaskCompleted},
		{ContextID: "d", State: TaskFailed},
		{ContextID: "e", State: TaskCanceled},
	}}
	if got := s.ActiveCount(); got != 2 {
		t.Fatalf("ActiveCount = %d, want 2", got)
	}
}

func TestRunningCountCountsOnlyWorking(t *testing.T) {
	s := TaskStore{Tasks: []A2ATask{
		{ContextID: "a", State: TaskSubmitted},
		{ContextID: "b", State: TaskWorking},
		{ContextID: "c", State: TaskWorking},
		{ContextID: "d", State: TaskCompleted},
		{ContextID: "e", State: TaskFailed},
		{ContextID: "f", State: TaskCanceled},
	}}
	if got := s.RunningCount(); got != 2 {
		t.Fatalf("RunningCount = %d, want 2 (only the two working tasks occupy a sandbox slot)", got)
	}
}

func TestTaskStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "codereview", State: TaskWorking, StartedAt: "2026-08-05T00:00:00Z"})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	got, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	tk, ok := got.ByContext("c1")
	if !ok || tk.Agent != "codereview" {
		t.Fatalf("round-trip lost data: %#v", tk)
	}
}
