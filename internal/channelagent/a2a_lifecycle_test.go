package channelagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now, nil)
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
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now, nil)
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

// TestSweepPreservesPriorDetailOnHardTimeout pins the fix for the exact
// scenario task 4 exists to address: TrustFolder fails, SandboxExecutor.Start
// leaves a warning on Detail while the task is Working, the sandbox wedges
// on a dialog nobody can answer, and two hours later the hard-timeout sweep
// cancels it. Without this fix, the sweep's own `t.Detail = reason`
// unconditionally overwrote that warning with "hard timeout exceeded",
// silently erasing the one clue that would have told an operator WHY it
// wedged — reproducing the "no reason visible anywhere" symptom on the very
// row meant to surface it.
func TestSweepPreservesPriorDetailOnHardTimeout(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	const trustNote = "預先信任 worktree 失敗,沙盒仍會啟動但可能卡在資料夾信任對話框: fake trust failure"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
		Detail:    trustNote,
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now, nil)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}

	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("state = %s, want canceled", tk.State)
	}
	if !strings.Contains(tk.Detail, trustNote) {
		t.Fatalf("Detail = %q, must still contain the trust-failure note %q", tk.Detail, trustNote)
	}
	if !strings.Contains(tk.Detail, "hard timeout exceeded") {
		t.Fatalf("Detail = %q, must still contain the sweep's own reason too", tk.Detail)
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
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil)
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
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 0 {
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
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now, nil)
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
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now, nil)
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
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil)
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

// TestSweepRemovesWorktreeOnReclaim also pins that reclamation removes the
// sandbox root (inbox/outbox/locks), not just the worktree: it creates a real
// marker file under SandboxRoot beforehand and asserts the whole directory is
// gone afterwards (finding 4 of the task-8 review — sandbox-root removal was
// previously asserted nowhere).
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

	sandboxRoot := SandboxRoot(root, "aa-a-c1")
	if err := os.MkdirAll(sandboxRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll sandbox root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandboxRoot, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	fake := &FakeSessionManager{}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 1 {
		t.Fatalf("reclaimed = %d err = %v", reclaimed, err)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "/p/aa-a-c1" {
		t.Fatalf("worktree not removed: %#v", fake.Removed)
	}
	if _, err := os.Stat(sandboxRoot); !os.IsNotExist(err) {
		t.Fatalf("sandbox root not removed: stat err = %v", err)
	}
}

// TestSweepRetriesFailedRemoval guards finding 1 of the task-8 review: clearing
// Worktree/Session before the actual removal succeeds would make a transient
// git/tmux/fs failure permanently orphan the sandbox, since nothing would
// reference its path any longer for a later sweep to find. A failed removal
// must leave the task's Worktree/Session in place so the next sweep retries.
func TestSweepRetriesFailedRemoval(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskCompleted,
		Worktree:    "/p/aa-a-c1",
		CompletedAt: now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{FailOn: "remove"}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 0 {
		t.Fatalf("first sweep: reclaimed = %d err = %v, want 0 (removal failed)", reclaimed, err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.Worktree != "/p/aa-a-c1" || tk.Session != "aa-a-c1" {
		t.Fatalf("worktree/session cleared despite a failed removal, now unreclaimable: %#v", tk)
	}

	fake.FailOn = ""
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 1 {
		t.Fatalf("retry sweep: reclaimed = %d err = %v, want 1", reclaimed, err)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "/p/aa-a-c1" {
		t.Fatalf("worktree not removed on retry: %#v", fake.Removed)
	}
	got, _ = LoadTasks(root)
	tk, _ = got.ByContext("c1")
	if tk.Worktree != "" || tk.Session != "" {
		t.Fatalf("task not cleared after the retry succeeded: %#v", tk)
	}
}

// TestSweepSkipsRowChangedDuringTeardown guards finding 1 of the task-8 review
// round 3: contextId ownership is checked on CallerID only, not on task
// state, and Session/Worktree paths are deterministic functions of the
// contextId. So a caller can legally resubmit the same contextId while a
// terminal task under it is mid-teardown (the window between SweepTimeouts'
// step 1, which identifies the row as a reclaim candidate, and step 3, which
// clears its fields). TaskStore.Upsert keys on contextId, so that
// resubmission overwrites the row in place with a new TaskID, a live
// Session/Worktree, and TaskSubmitted state. Step 3 must recognize the row no
// longer matches what step 1 selected and leave it untouched — clearing it
// would corrupt a live task's bookkeeping and orphan it with nothing left
// pointing at its (still very much alive) disk footprint.
//
// FakeSessionManager.OnRemove fires exactly once, from inside the
// RemoveWorkspace call that step 2 makes for the original candidate — i.e.
// precisely inside the window between steps 1 and 3 — and simulates the race
// by overwriting the tasks-file row for the same contextId with a different
// task carrying live Worktree/Session values.
func TestSweepSkipsRowChangedDuringTeardown(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "task-A", Session: "aa-a-c1", State: TaskCompleted,
		Worktree:    "/p/aa-a-c1",
		CompletedAt: now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	fake.OnRemove = func() {
		var live TaskStore
		live.Upsert(A2ATask{
			ContextID: "c1", TaskID: "task-B", Session: "aa-a-c1", State: TaskSubmitted,
			Worktree:  "/p/aa-a-c1",
			StartedAt: now.Format(time.RFC3339),
		})
		if err := SaveTasks(root, live); err != nil {
			t.Fatalf("inject race: %v", err)
		}
	}

	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 0 {
		t.Fatalf("reclaimed = %d err = %v, want 0 (the row now belongs to a different, live task)", reclaimed, err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.TaskID != "task-B" || tk.State != TaskSubmitted || tk.Worktree != "/p/aa-a-c1" || tk.Session != "aa-a-c1" {
		t.Fatalf("the racing task's live fields were clobbered by the stale sweep: %#v", tk)
	}
}

// TestSweepReclaimsWorktreeOnHardTimeoutCancel guards finding 2 of the task-8
// review: a canceled task is not a failed one, so the forensics exemption
// must not apply to it. HardTimeout's own purpose is bounding a wedged
// sandbox's disk footprint — leaving its worktree behind after cancellation
// would let a caller grow disk usage without bound purely via hard-timeouts.
func TestSweepReclaimsWorktreeOnHardTimeoutCancel(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		Worktree:  "/p/aa-a-c1",
		StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1 (a canceled task is not exempt like a failed one)", reclaimed)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "/p/aa-a-c1" {
		t.Fatalf("worktree not removed for the canceled task: %#v", fake.Removed)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("state = %s, want canceled", tk.State)
	}
	if tk.Worktree != "" || tk.Session != "" {
		t.Fatalf("canceled task's worktree/session not cleared: %#v", tk)
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
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
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
	_, _, _ = SweepTimeouts(context.Background(), root, fake, now, nil)
	if len(fake.Removed) != 0 {
		t.Fatalf("a single old failed sandbox must be kept for forensics, got %#v", fake.Removed)
	}
}

// TestSweepLeavesFailedSandboxForensicsEvenWithGarbageTimestamp pins that the
// forensics exemption is not weakened by the corrupt-timestamp fix: a failed
// task must never be reclaimed, regardless of how unreadable its CompletedAt
// is.
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
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 0 {
		t.Fatalf("failed sandboxes must be kept even with a corrupt timestamp: reclaimed=%d err=%v", reclaimed, err)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("failed sandbox must not be torn down: %#v", fake.Stopped)
	}
}

// 派送中崩潰（serve 被殺、機器重開）會留下永遠停在 dispatching 的 row，佔著
// 一個併發槽。
func TestSweepFailsStaleDispatchingRows(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Agent: "a", Session: "aa-a-c1", State: TaskDispatching,
		StartedAt:    now.Add(-10 * time.Minute).Format(time.RFC3339),
		DispatchedAt: now.Add(-DispatchStaleAfter - time.Minute).Format(time.RFC3339),
	})
	s.Upsert(A2ATask{
		ContextID: "c2", Agent: "a", Session: "aa-a-c2", State: TaskDispatching,
		StartedAt:    now.Format(time.RFC3339),
		DispatchedAt: now.Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	if _, _, err := SweepTimeouts(context.Background(), root, &FakeSessionManager{}, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	got, _ := LoadTasks(root)
	c1, _ := got.ByContext("c1")
	c2, _ := got.ByContext("c2")
	if c1.State != TaskFailed {
		t.Fatalf("stale dispatching row = %q, want failed", c1.State)
	}
	if c2.State != TaskDispatching {
		t.Fatalf("fresh dispatching row = %q, want it left alone", c2.State)
	}
}

// TestSweepDoesNotHardTimeoutFreshlyClaimedDispatchingRow guards review
// round 2, minor 1: a task can legitimately sit in TaskSubmitted for a long
// time (queued behind a full cap) before DrainQueue ever claims it. Its
// StartedAt is the ORIGINAL submission time, not the claim time — so once
// claimed, that StartedAt can already be close to or past HardTimeout even
// though DispatchedAt (the claim) is seconds old and it is still well inside
// its legal ~90s boot window. Falling through to the StartedAt/HardTimeout
// check (as the first draft of this fix did) would cancel a task that just
// started dispatching, purely because it waited a long time in queue first.
func TestSweepDoesNotHardTimeoutFreshlyClaimedDispatchingRow(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Agent: "a", Session: "aa-a-c1", State: TaskDispatching,
		// Queued for nearly 2 hours before finally being claimed — on its own
		// this StartedAt is already past HardTimeout.
		StartedAt: now.Add(-125 * time.Minute).Format(time.RFC3339),
		// ...but the claim itself is fresh and well inside DispatchStaleAfter.
		DispatchedAt: now.Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	// Sweep runs a couple of minutes after the claim — still nowhere near
	// DispatchStaleAfter (5m), and the row's StartedAt is now more than
	// HardTimeout (2h) in the past, which is exactly the condition that must
	// NOT cancel a dispatching row.
	sweepAt := now.Add(2 * time.Minute)
	if _, _, err := SweepTimeouts(context.Background(), root, fake, sweepAt, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskDispatching {
		t.Fatalf("state = %s, want dispatching (a fresh claim must never be hard-timed-out on its ORIGINAL queued StartedAt)", tk.State)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("session stopped despite a live, freshly-claimed dispatch: %#v", fake.Stopped)
	}
}

// TestSweepPreservesPriorDetailOnStaleDispatch mirrors
// TestSweepPreservesPriorDetailOnHardTimeout for the dispatching-stale path
// (review round 2, minor 2): the stale-dispatch branch must prepend, not
// discard, whatever Detail the row already carried.
func TestSweepPreservesPriorDetailOnStaleDispatch(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	const earlierNote = "預先信任 worktree 失敗,沙盒仍會啟動但可能卡在資料夾信任對話框: fake trust failure"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Agent: "a", Session: "aa-a-c1", State: TaskDispatching,
		StartedAt:    now.Add(-1 * time.Minute).Format(time.RFC3339),
		DispatchedAt: now.Add(-DispatchStaleAfter - time.Minute).Format(time.RFC3339),
		Detail:       earlierNote,
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskFailed {
		t.Fatalf("state = %s, want failed", tk.State)
	}
	if !strings.Contains(tk.Detail, earlierNote) {
		t.Fatalf("Detail = %q, must still contain the earlier note %q", tk.Detail, earlierNote)
	}
	if !strings.Contains(tk.Detail, "dispatch stalled") {
		t.Fatalf("Detail = %q, must still contain the sweep's own reason too", tk.Detail)
	}
}

// 規格第五節測試 3：既有的 TestSweepSkipsRowChangedDuringTeardown 只驗第 3 步
// 的帳面守衛（清欄位前的比對）。這一條驗第 2 步本身 —— 拆除的破壞性動作
// （停 driver、停 tmux session、刪 worktree、刪 sandbox root）一項都不能碰
// 到新身分,不只是「事後帳本沒被清空」。
//
// review round 2, important 3：原本這裡用 FakeSessionManager.OnRemove 在
// RemoveWorkspace 裡面才模擬重新提交,但那已經是 tryLockSandboxSessionForTeardown
// 成功、candidateStillMatches 也通過之後的事了 —— 測到的其實是第 3 步既有
// 的帳面比對,不是這個任務新加的鎖／重確認。把 candidateStillMatches 與鎖整
// 段刪掉,這個舊寫法照樣會過(os.RemoveAll 對不存在的路徑回 nil,斷言只讀
// row)。
//
// 改成把「持有共享鎖」這個真正的把柄移到 sweep 開始「之前」：直接呼叫
// lockSandboxSession（session 鎖同一顆物件、同一個 RWMutex）模擬一個正在
// 建立中的 Start（或正在投遞的 DeliverFollowUp）持有它，並在持有期間把 row
// 換成新身分（等同真正 Start 的 persist() 會做的事）。這是真正的鎖競爭
// （tryLockSandboxSessionForTeardown 底下呼叫的是同一個 RWMutex 的
// TryLock），不是計時賭注：只要共享鎖還被握著，TryLock 不管排程如何都會失
// 敗。斷言也從「row 沒被清空」升級成「stop 與 remove 兩類破壞性動作全部沒
// 發生、sandbox root 目錄真的還在磁碟上」。
func TestSweepDoesNotDestroyAResubmittedIdentity(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	const session = "aa-a-c1"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t-old", Agent: "a", Session: session,
		Worktree: "/p/aa-a-c1", State: TaskCompleted,
		CompletedAt: now.Add(-time.Hour).Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	// 沙盒真的存在過，好讓「還在磁碟上」的斷言有意義。
	if err := Init(SandboxRoot(root, session)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 在 sweep 開始之前就把共享鎖握住並換成新身分 —— 這正是 D3(a) 要擋的窗口
	// 本身，不是它的事後結果。
	unlock := lockSandboxSession(session)
	if err := WithTasks(root, func(tasks *TaskStore) error {
		tasks.Upsert(A2ATask{
			ContextID: "c1", TaskID: "t-new", Agent: "a", Session: session,
			Worktree: "/p/aa-a-c1", State: TaskDispatching, Level: GrantDevelop,
			StartedAt: now.Format(time.RFC3339), DispatchedAt: now.Format(time.RFC3339),
		})
		return nil
	}); err != nil {
		unlock()
		t.Fatalf("WithTasks (simulate resubmission): %v", err)
	}

	fake := &FakeSessionManager{}
	stopper := &recordingStopper{}
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, stopper)
	unlock()
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0 (the session is in use, sweep must not have touched it at all)", reclaimed)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("sweep stopped a tmux session that is currently in use: %#v", fake.Stopped)
	}
	if len(fake.Removed) != 0 {
		t.Fatalf("sweep removed a worktree that is currently in use: %#v", fake.Removed)
	}
	if len(stopper.stopped) != 0 {
		t.Fatalf("sweep stopped a driver whose session is currently in use: %#v", stopper.stopped)
	}
	if _, err := os.Stat(SandboxRoot(root, session)); err != nil {
		t.Fatalf("sandbox root must survive on disk while the session is in use: %v", err)
	}

	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.TaskID != "t-new" || tk.Session == "" || tk.Worktree == "" {
		t.Fatalf("the resubmitted identity was corrupted: %#v", tk)
	}
}

// TestSweepDoesNotStopADispatchStalledSessionCurrentlyInUse pins the same
// review round 2 critical finding for the OTHER code path that stops a
// session without ever building a reclaimCandidate: a row stuck in
// TaskDispatching past DispatchStaleAfter (dispatch-stalled crash recovery,
// a2a_lifecycle.go's stopOnly). Before this fix it appended straight to
// toStop with no identity snapshot at all, so nothing guarded it against
// the same resubmission race reclaimCandidate is guarded against.
//
// This is deliberately a one-shot guard, not a retry-forever one: unlike a
// reclaimCandidate (re-derived fresh from the row's still-live
// Worktree/Session on every sweep pass), a dispatch-stalled row is only
// ever considered for a session-stop at the exact instant it transitions to
// TaskFailed — scanning EVERY TaskFailed row on every later pass to retry a
// missed stop was tried and reverted, because it cannot distinguish "this
// TaskFailed row's session is a genuine leftover from OUR crashed dispatch"
// from "this TaskFailed row is being deliberately kept untouched for
// forensics" (see TestSweepLeavesFailedSandboxForensics — a live Session on
// a forensically-retained failure must never be stopped, ever, by design).
// The narrow race this guards against — the session is busy at the exact
// instant sweep tries to stop it — resolves itself without a retry anyway:
// "busy" only means some legitimate Start/DeliverFollowUp is using it right
// then, and that call's own completion changes this row's identity out from
// under the stale snapshot sweep would otherwise retry against.
//
// Uses the same real-lock-contention technique as
// TestSweepDoesNotDestroyAResubmittedIdentity: holding the session's shared
// lock IS the deterministic proof that tryLockSandboxSessionForTeardown
// cannot succeed, not a timing gamble.
func TestSweepDoesNotStopADispatchStalledSessionCurrentlyInUse(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	const session = "aa-a-c1"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", Session: session, State: TaskDispatching,
		StartedAt:    now.Add(-1 * time.Minute).Format(time.RFC3339),
		DispatchedAt: now.Add(-DispatchStaleAfter - time.Minute).Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	unlock := lockSandboxSession(session)
	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		unlock()
		t.Fatalf("SweepTimeouts: %v", err)
	}
	unlock()

	// 狀態轉換是第 1 步在 tasksMu 底下做的純帳面動作，跟 session 鎖無關，一
	// 樣會發生 —— 只有真正的破壞性動作（停 session）被擋下來。
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskFailed {
		t.Fatalf("state = %s, want failed (bookkeeping must proceed regardless of the session lock)", tk.State)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("sweep stopped a session that was in use at the time: %#v", fake.Stopped)
	}
}

// TestSweepNeverBlocksOnALiveBuild pins review round 2, important finding 2:
// handleRPC calls Executor.Start on the request goroutine, which holds the
// session's shared lock for the WHOLE build — up to ~90s in production,
// unbounded if git worktree add or tmux wedges. Sweep's exclusive
// acquisition (tryLockSandboxSessionForTeardown) must never wait for that
// to finish, or one wedged HTTP-dispatched Start blocks the sweep cycle and,
// with it, DrainQueue, EnsureSandboxDrivers and shutdown's driver.StopAll()
// — indefinitely.
//
// This is a genuine concurrency proof, not a timing gamble: a REAL Start()
// call runs on a REAL goroutine and is deterministically parked mid-build
// via FakeSessionManager's EnsureWorkspaceHold/Entered (the same idiom
// a2a_server_test.go's TestHandlerAndDrainQueueNeverDoubleDispatch and
// TestFollowUpDuringInFlightDrainQueueDispatchDoesNotDoubleDispatch already
// use for exactly this reason) — <-entered only unblocks once Start is
// genuinely inside EnsureWorkspace, still holding the shared lock. Sweep
// then runs concurrently against a candidate whose session is that exact
// same name and must return well within the timeout regardless — if
// tryLockSandboxSessionForTeardown ever regressed to a blocking Lock(),
// this test would hang until Go's test timeout, not merely run slow.
func TestSweepNeverBlocksOnALiveBuild(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "a", ProjectDir: "/p/x", Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	const session = "aa-a-c1"

	// c1 是這一輪 sweep 想回收的候選，session 名跟下面正在建立中的那個撞名
	// （合法追問落在確定性路徑上的同一個道理：session 名只是 agent+contextId
	// 的函式，這裡直接指定同名即可，不需要真的透過 SessionNameFor 產生撞
	// 名，測的是鎖本身，不是命名規則）。
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t-old", Agent: "a", Session: session,
		Worktree: "/p/aa-a-c1", State: TaskCompleted,
		CompletedAt: now.Add(-time.Hour).Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	hold := make(chan struct{})
	entered := make(chan struct{})
	fake := &FakeSessionManager{EnsureWorkspaceHold: hold, EnsureWorkspaceEntered: entered}
	ex := NewSandboxExecutor(root, fake)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		task := A2ATask{ContextID: "c2", Agent: "a", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
		_ = ex.Start(context.Background(), task, "go")
	}()
	<-entered // Start 現在真的卡在 EnsureWorkspace 裡，共享鎖確定被握著。
	defer func() {
		close(hold) // 放開它，讓它跑完，不留下測試結束後還在跑的 goroutine。
		wg.Wait()
	}()

	done := make(chan struct{})
	go func() {
		_, _, _ = SweepTimeouts(context.Background(), root, &FakeSessionManager{}, now, nil)
		close(done)
	}()
	select {
	case <-done:
		// 通過：sweep 沒有卡住，即使有一個活著的建置正握著同名 session 的
		// 共享鎖。
	case <-time.After(2 * time.Second):
		t.Fatal("SweepTimeouts blocked on a live build instead of using a non-blocking exclusive acquisition")
	}
}

// D4：sweep 必須在動手之前先停掉還活著的 driver。Stop 阻塞到 goroutine 真的
// 結束，那正是回收需要的保證 —— 否則下一輪 RunWorkerOnce 的 Init(root) 會把
// 剛刪掉的目錄樹重建回來。
func TestSweepStopsDriversBeforeRemoving(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", Session: "aa-a-c1",
		Worktree: "/p/aa-a-c1", State: TaskCompleted,
		CompletedAt: now.Add(-time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	stopper := &recordingStopper{}
	fake := &FakeSessionManager{}
	fake.OnRemove = func() {
		if len(stopper.stopped) == 0 {
			t.Error("the worktree was removed before the driver was stopped")
		}
	}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, stopper); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if len(stopper.stopped) != 1 || stopper.stopped[0] != "aa-a-c1" {
		t.Fatalf("stopped = %#v", stopper.stopped)
	}
}

// TestFollowUpAfterSweepReclaimDoesNotResurrectSandboxDir pins a task 7
// review finding (a2a_server.go:389, carried over from task 6): handleRPC
// reads its followUpTask snapshot under tasksMu, releases that lock, and
// only THEN calls DeliverFollowUp outside it (Inject touches the
// filesystem and must never run while tasksMu is held). If SweepTimeouts
// runs to completion for this exact session in that gap — a task sitting in
// TaskWorking past HardTimeout is canceled and reclaimed in the very same
// pass, see the "for canceled rows" loop in SweepTimeouts — the snapshot
// handed to DeliverFollowUp is stale even though it was never terminal at
// the moment handleRPC read it. Pre-fix, DeliverFollowUp injected
// unconditionally; Inject's underlying IngestMessages calls Init(root)
// unconditionally, which would recreate sandboxes/<session>/ right after
// this sweep just removed it — a directory holding a pending job nothing
// will ever reclaim.
//
// This does not need real goroutines to be a genuine proof: it reproduces
// the exact ordering handleRPC would observe — capture the snapshot, THEN
// let sweep run to completion for that session, THEN attempt delivery —
// deterministically, by simply doing those three steps in that order.
// There is no data race in question here (nothing touches shared state
// concurrently); what's under test is whether a stale snapshot is rejected
// rather than blindly acted on, which a fixed ordering already proves
// unambiguously.
func TestFollowUpAfterSweepReclaimDoesNotResurrectSandboxDir(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	const session = "aa-a-c1"
	seed := A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a",
		Session: session, State: TaskWorking, Level: GrantReadOnly,
		StartedAt: now.Add(-HardTimeout - time.Minute).Format(time.RFC3339),
	}
	var s TaskStore
	s.Upsert(seed)
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	// 沙盒真的存在過(模擬 Start 早就 Init 過)。
	if err := Init(SandboxRoot(root, session)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// handleRPC 在 tasksMu 之內讀到的、還沒過期的舊快照 —— 這是它會交給
	// DeliverFollowUp 的那份，狀態仍是 working。
	staleSnapshot := seed

	// sweep 在追問真正送達之前就跑完了一整趟：同一輪內硬逾時取消 + 回收。
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, &FakeSessionManager{}, now, nil); err != nil || reclaimed != 1 {
		t.Fatalf("SweepTimeouts: reclaimed=%d err=%v, want reclaimed=1", reclaimed, err)
	}
	if _, err := os.Stat(SandboxRoot(root, session)); !os.IsNotExist(err) {
		t.Fatalf("sandbox root must be gone after sweep, stat err = %v", err)
	}

	ex := &SandboxExecutor{Root: root, Sessions: &realInjectSessionManager{FakeSessionManager: &FakeSessionManager{}}}
	if err := ex.DeliverFollowUp(context.Background(), staleSnapshot, "late follow up"); err == nil {
		t.Fatal("DeliverFollowUp must refuse a snapshot sweep already reclaimed, not silently deliver")
	}
	if _, err := os.Stat(SandboxRoot(root, session)); !os.IsNotExist(err) {
		t.Fatalf("DeliverFollowUp must not resurrect the sandbox root sweep just removed, stat err = %v", err)
	}
}

type recordingStopper struct{ stopped []string }

func (r *recordingStopper) Stop(session string) { r.stopped = append(r.stopped, session) }
