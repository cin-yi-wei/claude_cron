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

// TestCanTransitionAllowsForwardOnly 原本斷言 submitted→working 直接可行；
// 這條路徑正是 C2 的根源（handler 派送與 DrainQueue 之間沒有任何佔位，兩者都
// 能把同一列從 submitted 直接判定成「可以 Start」）。task 6 插入 dispatching
// 作為必經的中間狀態後，submitted 只能先變 dispatching，於是這裡改成斷言
// submitted→dispatching→working 這條兩段式路徑，取代原本的一步到位。
func TestCanTransitionAllowsForwardOnly(t *testing.T) {
	ok := [][2]TaskState{
		{TaskSubmitted, TaskDispatching},
		{TaskDispatching, TaskWorking},
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
		{TaskSubmitted, TaskWorking},
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

func TestDispatchingStateMachine(t *testing.T) {
	for _, c := range []struct {
		from, to TaskState
		want     bool
	}{
		{TaskSubmitted, TaskDispatching, true},
		{TaskSubmitted, TaskWorking, false}, // 必須先取得派送權
		{TaskSubmitted, TaskCanceled, true},
		{TaskDispatching, TaskWorking, true},
		{TaskDispatching, TaskFailed, true},
		{TaskDispatching, TaskCanceled, true},
		{TaskDispatching, TaskCompleted, false},
		{TaskCompleted, TaskDispatching, false},
	} {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%q,%q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// dispatching 的 row 已經在起沙盒了，它就是佔著一個槽。不計入 RunningCount
// 正是 40 個並發請求全部算出「有容量」的原因。
func TestRunningCountIncludesDispatching(t *testing.T) {
	s := TaskStore{Tasks: []A2ATask{
		{ContextID: "a", State: TaskWorking},
		{ContextID: "b", State: TaskDispatching},
		{ContextID: "c", State: TaskSubmitted},
		{ContextID: "d", State: TaskCompleted},
	}}
	if got := s.RunningCount(); got != 2 {
		t.Fatalf("RunningCount = %d, want 2 (working + dispatching)", got)
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
