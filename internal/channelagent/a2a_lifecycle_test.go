package channelagent

import (
	"context"
	"fmt"
	"testing"
	"time"
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

// TestDrainQueueRecoversWhenOnlyQueuedTasksExist guards against a permanent
// deadlock: if capacity were gated on ActiveCount (submitted OR working),
// a pile of queued (submitted) work with zero tasks actually running would
// read as "full" even though no sandbox is occupied — and since nothing is
// running, nothing can ever complete to lower that count. Nothing would ever
// start again. Capacity must be gated on RunningCount (working only), under
// which a submitted task waiting for a slot doesn't count against itself.
func TestDrainQueueRecoversWhenOnlyQueuedTasksExist(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	// More queued tasks than the cap, and nothing running at all.
	for i := 0; i < MaxConcurrentSandboxes+2; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), Agent: "a", State: TaskSubmitted, Prompt: "work"})
	}
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	stub := &StubExecutor{}
	n, err := DrainQueue(context.Background(), root, stub)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if n != MaxConcurrentSandboxes {
		t.Fatalf("started = %d, want %d — queued-only work must still drain up to the cap", n, MaxConcurrentSandboxes)
	}
	if stub.Calls != MaxConcurrentSandboxes {
		t.Fatalf("executor calls = %d, want %d", stub.Calls, MaxConcurrentSandboxes)
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

func TestSweepTimeoutsConstants(t *testing.T) {
	if SoftTimeout != 30*time.Minute {
		t.Fatalf("SoftTimeout = %v, want 30m", SoftTimeout)
	}
	if HardTimeout != 2*time.Hour {
		t.Fatalf("HardTimeout = %v, want 2h", HardTimeout)
	}
	if RetainAfterComplete != 10*time.Minute {
		t.Fatalf("RetainAfterComplete = %v, want 10m", RetainAfterComplete)
	}
}

func TestSweepDoesNotCancelBeforeHardTimeout(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		StartedAt: now.Add(-45 * time.Minute).Format(time.RFC3339), // past soft, before hard
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 0 {
		t.Fatalf("canceled = %d, want 0 (soft timeout must not kill)", canceled)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state = %s, want still working", tk.State)
	}
}

func TestSweepCancelsAfterHardTimeout(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("session not stopped: %#v", fake.Stopped)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("state = %s, want canceled", tk.State)
	}
}

func TestSweepReclaimsCompletedAfterRetention(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskCompleted,
		CompletedAt: now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	s.Upsert(A2ATask{
		ContextID: "c2", Session: "aa-a-c2", State: TaskCompleted,
		CompletedAt: now.Add(-2 * time.Minute).Format(time.RFC3339), // still in retention
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("wrong session reclaimed: %#v", fake.Stopped)
	}
}

func TestSweepLeavesFailedSandboxForensics(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskFailed,
		CompletedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now); err != nil || reclaimed != 0 {
		t.Fatalf("failed sandboxes must be kept: reclaimed=%d err=%v", reclaimed, err)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("failed sandbox must not be torn down: %#v", fake.Stopped)
	}
}

// TestSweepCancelsWorkingTaskWithEmptyStartedAt guards against a malformed
// timestamp permanently defeating the hard-timeout backstop: a non-terminal
// task with no StartedAt at all is at least as likely to be wedged as one
// with a known-old timestamp, so it must be sweep-eligible rather than
// skipped forever.
func TestSweepCancelsWorkingTaskWithEmptyStartedAt(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		StartedAt: "",
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1 (empty StartedAt must be sweep-eligible)", canceled)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("session not stopped: %#v", fake.Stopped)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("state = %s, want canceled", tk.State)
	}
	if tk.Detail == "" || tk.Detail == "hard timeout exceeded" {
		t.Fatalf("Detail = %q, want a distinct reason naming the unreadable timestamp", tk.Detail)
	}
}

// TestSweepCancelsWorkingTaskWithGarbageStartedAt is the same guard for a
// StartedAt that fails to parse (as opposed to being merely empty).
func TestSweepCancelsWorkingTaskWithGarbageStartedAt(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskSubmitted,
		StartedAt: "not-a-timestamp",
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1 (garbage StartedAt must be sweep-eligible)", canceled)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("session not stopped: %#v", fake.Stopped)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("state = %s, want canceled", tk.State)
	}
	if tk.Detail == "" || tk.Detail == "hard timeout exceeded" {
		t.Fatalf("Detail = %q, want a distinct reason naming the unreadable timestamp", tk.Detail)
	}
}

// TestSweepReclaimsCompletedTaskWithUnparseableCompletedAt applies the same
// reasoning to the retention path: an unparseable CompletedAt on a completed
// task must not pin its sandbox forever.
func TestSweepReclaimsCompletedTaskWithUnparseableCompletedAt(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskCompleted,
		CompletedAt: "garbage",
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1 (unparseable CompletedAt must be sweep-eligible)", reclaimed)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("wrong session reclaimed: %#v", fake.Stopped)
	}
}

// TestSweepLeavesFailedSandboxForensicsEvenWithGarbageTimestamp pins that the
// forensics exemption is not weakened by the corrupt-timestamp fix: a failed
// task must never be reclaimed, regardless of how unreadable its CompletedAt
// is.
func TestSweepRemovesWorktreeOnReclaim(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskCompleted,
		Worktree:    "/p/aa-a-c1",
		CompletedAt: now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now); err != nil || reclaimed != 1 {
		t.Fatalf("reclaimed = %d err = %v", reclaimed, err)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "/p/aa-a-c1" {
		t.Fatalf("worktree not removed: %#v", fake.Removed)
	}
}

func TestSweepCapsRetainedFailedSandboxes(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	for i := 0; i < MaxRetainedFailedSandboxes+5; i++ {
		s.Upsert(A2ATask{
			ContextID: fmt.Sprintf("c%d", i),
			Session:   fmt.Sprintf("aa-a-c%d", i),
			Worktree:  fmt.Sprintf("/p/aa-a-c%d", i),
			State:     TaskFailed,
			// oldest first
			CompletedAt: now.Add(-time.Duration(200-i) * time.Hour).Format(time.RFC3339),
		})
	}
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if len(fake.Removed) != 5 {
		t.Fatalf("removed %d failed sandboxes, want 5 (the oldest beyond the cap)", len(fake.Removed))
	}
	for _, r := range fake.Removed {
		if r == "/p/aa-a-c24" {
			t.Fatal("newest failed sandbox was reclaimed; the cap must drop the OLDEST")
		}
	}
}

func TestSweepKeepsFailedSandboxesUnderTheCap(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", Worktree: "/p/aa-a-c1", State: TaskFailed,
		CompletedAt: now.Add(-300 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	_, _, _ = SweepTimeouts(context.Background(), root, fake, now)
	if len(fake.Removed) != 0 {
		t.Fatalf("a single old failed sandbox must be kept for forensics, got %#v", fake.Removed)
	}
}

func TestSweepLeavesFailedSandboxForensicsEvenWithGarbageTimestamp(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskFailed,
		CompletedAt: "garbage",
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now); err != nil || reclaimed != 0 {
		t.Fatalf("failed sandboxes must be kept even with a corrupt timestamp: reclaimed=%d err=%v", reclaimed, err)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("failed sandbox must not be torn down: %#v", fake.Stopped)
	}
}
