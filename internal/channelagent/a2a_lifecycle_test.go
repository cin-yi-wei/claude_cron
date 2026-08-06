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

// seedApprovedCallerAndEnabledAgent writes a minimal callers.json/agents.json
// so DrainQueue's per-cycle revalidation (task 8, I1) doesn't reject a
// capacity-mechanics test's queued rows for reasons unrelated to what that
// test is actually exercising: these rows carry no Level, and grantRank("")
// is 0 — never greater than any valid grant — so the caller's exact grant
// level here is irrelevant, only that callerID/agent resolve and are
// approved/enabled.
func seedApprovedCallerAndEnabledAgent(t *testing.T, root, callerID, agentName string) {
	t.Helper()
	var callers CallerStore
	if err := callers.Register(callerID, "s"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	callers.Approve(callerID, []string{"read"})
	if err := SaveCallers(root, callers); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}
	var agents AgentStore
	if err := agents.Add(Agent{Name: agentName, ProjectDir: "/p/" + agentName, Capabilities: []string{"read"}, Enabled: true}); err != nil {
		t.Fatalf("Add agent: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
}

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
	seedApprovedCallerAndEnabledAgent(t, root, "peer", "a")
	var s TaskStore
	// One already working, plus three queued.
	s.Upsert(A2ATask{ContextID: "live", Agent: "a", State: TaskWorking})
	for _, id := range []string{"q1", "q2", "q3"} {
		s.Upsert(A2ATask{ContextID: id, Agent: "a", CallerID: "peer", State: TaskSubmitted, Prompt: "work " + id})
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
	seedApprovedCallerAndEnabledAgent(t, root, "peer", "a")
	var s TaskStore
	// More queued tasks than the cap, and nothing running at all.
	for i := 0; i < MaxConcurrentSandboxes+2; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), Agent: "a", CallerID: "peer", State: TaskSubmitted, Prompt: "work"})
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
// 的帳面比對,不是這個任務新加的鎖／重確認。
//
// review round 3：修成「在呼叫 sweep 之前就把 row 換成新身分」之後,又踩到
// 完全同一類問題,只是搬了位置：新身分一旦在 sweep 呼叫之前就寫進 row,第 1
// 步根本不會把舊身分選成 candidate,sweep 不管有沒有守衛都什麼都不做——刪
// 掉 tryLockSandboxSessionForTeardown 跟 candidateStillMatches 整段,這個版
// 本一樣會過。
//
// 真正修法：用一個真正的 goroutine 模擬「一個正在建立中的 Start」——它先搶
// 下共享鎖,發訊號說「鎖已經握住」,然後卡住不動,等主測試 goroutine 明確放
// 行才把新身分寫進 row。sweep 必須在「鎖已經握住、但 row 還沒被覆寫」這個真
// 實存在、由 <-entered 精確釘死的瞬間執行——這正是真正的 Start 在拿到鎖與呼
// 叫 persist() 之間會有的那個窗口,不是計時賭注,因為 entered 通道保證這個
// 順序不會被排程打亂。sweep 執行的那一刻,第 1 步看到的還是舊身分（真的被
// 選成 candidate),第 2 步嘗試拿鎖時鎖確定被握著（TryLock 保證失敗，不是機
// 率性的),所以這個版本能同時證明「候選有被選中」與「守衛擋下了它」。
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

	entered := make(chan struct{})
	hold := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 一個真正的 Start 會先搶共享鎖，再呼叫 persist() 覆寫 row —— 這兩
		// 件事之間有真實的窗口：鎖已經握住，row 還是舊的。
		unlock := lockSandboxSession(session)
		defer unlock()
		close(entered)
		<-hold
		_ = WithTasks(root, func(tasks *TaskStore) error {
			tasks.Upsert(A2ATask{
				ContextID: "c1", TaskID: "t-new", Agent: "a", Session: session,
				Worktree: "/p/aa-a-c1", State: TaskDispatching, Level: GrantDevelop,
				StartedAt: now.Format(time.RFC3339), DispatchedAt: now.Format(time.RFC3339),
			})
			return nil
		})
	}()
	<-entered // 共享鎖已經握住；row 這一刻仍是舊身分。

	fake := &FakeSessionManager{}
	stopper := &recordingStopper{}
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, stopper)
	if err != nil {
		close(hold)
		wg.Wait()
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if reclaimed != 0 {
		close(hold)
		wg.Wait()
		t.Fatalf("reclaimed = %d, want 0 (the session is in use, sweep must not have touched it at all)", reclaimed)
	}
	if len(fake.Stopped) != 0 {
		close(hold)
		wg.Wait()
		t.Fatalf("sweep stopped a tmux session that is currently in use: %#v", fake.Stopped)
	}
	if len(fake.Removed) != 0 {
		close(hold)
		wg.Wait()
		t.Fatalf("sweep removed a worktree that is currently in use: %#v", fake.Removed)
	}
	if len(stopper.stopped) != 0 {
		close(hold)
		wg.Wait()
		t.Fatalf("sweep stopped a driver whose session is currently in use: %#v", stopper.stopped)
	}
	if _, err := os.Stat(SandboxRoot(root, session)); err != nil {
		close(hold)
		wg.Wait()
		t.Fatalf("sandbox root must survive on disk while the session is in use: %v", err)
	}

	close(hold) // 放行「建立」，讓它把新身分寫進去，跟真正的 Start 一樣。
	wg.Wait()

	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.TaskID != "t-new" || tk.Session == "" || tk.Worktree == "" {
		t.Fatalf("the resubmitted identity was corrupted: %#v", tk)
	}
	if _, err := os.Stat(SandboxRoot(root, session)); err != nil {
		t.Fatalf("resubmitted identity's sandbox root must still be on disk: %v", err)
	}
}

// TestSweepStopsSessionAfterASkippedPassBeforeReclaiming pins the task 7
// review round 3 critical finding: a skipped teardown pass must not lose
// the session-stop permanently. Sequence, matching the reviewer's own
// probe: pass N has c1 TaskWorking past HardTimeout — step 1 flips it to
// TaskCanceled (pure bookkeeping, unaffected by any lock), but the session
// is busy (a legitimate Start/DeliverFollowUp holds the shared lock) so
// step 2's teardown lock attempt fails and NOTHING is stopped or removed.
// Pass N+1, ten seconds later, the session is free. Before this fix,
// whether step 2 would stop the session on THIS pass was decided by
// justCanceled[contextId] — a map rebuilt fresh every pass from ONLY the
// rows that switch-case transitioned to TaskCanceled during THIS exact
// pass. c1 already became TaskCanceled back on pass N, so pass N+1 never
// re-enters that branch, justCanceled[c1] is false, stopSession reads
// false, and removeCandidate deletes the worktree, sandbox root and policy
// file having never called sm.Stop or stopper.Stop even once — a live
// tmux pane whose cwd was just deleted, referenced by no row, and (because
// the driver was never told to stop either) still able to re-Init() that
// same root a moment later. The fix derives "stop it" from the row's own,
// durable Session field — read fresh every pass via step 1 — rather than a
// per-pass transition flag, so a skipped pass simply retries the exact same
// stop-then-remove attempt next time, atomically, never one without the
// other.
func TestSweepStopsSessionAfterASkippedPassBeforeReclaiming(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	const session = "aa-a-c1"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", Session: session,
		Worktree: "/p/aa-a-c1", State: TaskWorking,
		StartedAt: now.Add(-HardTimeout - time.Minute).Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	if err := Init(SandboxRoot(root, session)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	fake := &FakeSessionManager{}

	// Pass N：session 忙碌。硬逾時取消的帳面動作照常發生（跟鎖無關），但整
	// 段拆除（含停 session）被跳過。
	unlock := lockSandboxSession(session)
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 0 {
		unlock()
		t.Fatalf("pass N: reclaimed=%d err=%v, want 0 (the session is in use)", reclaimed, err)
	}
	unlock()
	if len(fake.Stopped) != 0 {
		t.Fatalf("pass N stopped a session that was in use: %#v", fake.Stopped)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("pass N state = %s, want canceled (bookkeeping must proceed regardless of the session lock)", tk.State)
	}
	if tk.Session != session || tk.Worktree == "" {
		t.Fatalf("pass N must not have cleared anything it never removed: %#v", tk)
	}

	// Pass N+1：session 現在閒置。修好之前，這裡會直接刪掉 worktree/sandbox
	// root，卻從來沒有停過 session —— probe 印出的正是
	// "pass N+1: reclaimed=1 stopped=[] removed=[/p/aa-a-c1]"。
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil || reclaimed != 1 {
		t.Fatalf("pass N+1: reclaimed=%d err=%v, want 1", reclaimed, err)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != session {
		t.Fatalf("pass N+1 must stop the session before reclaiming it, stopped = %#v", fake.Stopped)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "/p/aa-a-c1" {
		t.Fatalf("pass N+1 removed = %#v, want exactly the worktree", fake.Removed)
	}
	if _, err := os.Stat(SandboxRoot(root, session)); !os.IsNotExist(err) {
		t.Fatalf("sandbox root should be gone after the retried reclaim, stat err = %v", err)
	}
}

// TestSweepNeverRemovesAWorktreeWithNoSessionIdentity pins the task 7
// review round 3 important finding: a row with Worktree != "" but
// Session == "" carries no identity a lock can be taken against
// (lockSandboxSession/tryLockSandboxSessionForTeardown are both keyed on
// session name), so removeCandidate's c.session == "" branch used to run
// completely unguarded — no lock, and (before this fix) not even
// candidateStillMatches. The reviewer's probe reached this state via
// stopSessionGuarded's step-3 write-back clearing ONLY Session for a
// dispatch-stalled row while leaving Worktree alone; that write-back has
// since been removed entirely (stopTarget's Session is never cleared, see
// its doc comment) precisely so this state can no longer arise through any
// sweep path. This test proves the OTHER half of the fix: even confronted
// with a row already in this state (e.g. hand-edited tasks.json, or any
// future code that reintroduces it), the failed-sandbox-cap trim path
// refuses to remove it at all, rather than attempting a check-then-act that
// cannot actually close its own window (there being no lock to hold
// between the check and the act).
func TestSweepNeverRemovesAWorktreeWithNoSessionIdentity(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	// 故意產生一列 Worktree != "" 但 Session == "" 的 TaskFailed row —— 沒
	// 有任何目前的 sweep 路徑會製造這個狀態，這裡直接手動構造，模擬「萬一
	// 某個未來的 bug 或手動編輯又造出這種列」的邊界情況。
	s.Upsert(A2ATask{
		ContextID: "cold", TaskID: "t-cold", Agent: "a", Worktree: "/p/aa-a-cold",
		State: TaskFailed, CompletedAt: now.Add(-48 * time.Hour).Format(time.RFC3339),
	})
	for i := 0; i < MaxRetainedFailedSandboxes; i++ {
		id := fmt.Sprintf("recent%02d", i)
		s.Upsert(A2ATask{
			ContextID: id, TaskID: "t-" + id, Agent: "a", Session: "aa-a-" + id,
			Worktree: "/p/aa-a-" + id, State: TaskFailed,
			CompletedAt: now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
		})
	}
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}

	if len(fake.Removed) != 0 {
		t.Fatalf("a session-less candidate must never be removed, no lock can guard it: %#v", fake.Removed)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("cold")
	if tk.Worktree != "/p/aa-a-cold" {
		t.Fatalf("the session-less row must be left completely untouched: %#v", tk)
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

// task 8 (D6)：呼叫方灌爆佇列後被 operator 撤銷，backlog 不可以繼續被排空成
// 新沙盒 —— DrainQueue 必須在派送前重讀 callers.json，不是只在最初 dispatch
// 那一次查過就算數。
func TestDrainQueueFailsRowsWhoseCallerWasRevoked(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetGrantLevel("peer-a", GrantDevelop)
	callers.Revoke("peer-a")
	_ = SaveCallers(root, callers)

	var agents AgentStore
	_ = agents.Add(Agent{Name: "a", ProjectDir: "/p/a", Capabilities: []string{"read"}, Enabled: true})
	_ = SaveAgents(root, agents)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Agent: "a", CallerID: "peer-a", Level: GrantDevelop,
		Session: "aa-a-c1", State: TaskSubmitted, StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if n, err := DrainQueue(context.Background(), root, NewSandboxExecutor(root, fake)); err != nil || n != 0 {
		t.Fatalf("started = %d err = %v; a revoked caller's backlog must not drain", n, err)
	}
	if len(fake.Started) != 0 {
		t.Fatalf("started %#v for a revoked caller", fake.Started)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskFailed || !strings.Contains(tk.Detail, "revoked") {
		t.Fatalf("row = %q / %q, want failed with a revocation detail", tk.State, tk.Detail)
	}
	entries, _ := ReadAudit(root)
	if len(entries) == 0 || entries[len(entries)-1].Outcome != "drain_rejected" {
		t.Fatalf("a silent continue is exactly the defect; audit tail = %#v", entries)
	}
}

// task 8 (D6)：disabling an agent must also stop its already-queued backlog
// from draining into a new sandbox, not just future submissions.
func TestDrainQueueFailsRowsForDisabledAgents(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetGrantLevel("peer-a", GrantDevelop)
	_ = SaveCallers(root, callers)

	var agents AgentStore
	_ = agents.Add(Agent{Name: "a", ProjectDir: "/p/a", Capabilities: []string{"read"}, Enabled: false})
	_ = SaveAgents(root, agents)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Agent: "a", CallerID: "peer-a", Level: GrantDevelop,
		Session: "aa-a-c1", State: TaskSubmitted, StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	_, _ = DrainQueue(context.Background(), root, NewSandboxExecutor(root, fake))
	if len(fake.Started) != 0 {
		t.Fatalf("started %#v for a disabled agent", fake.Started)
	}
}

// task 8 (I7)：session 不存在但任務未完成 → 標 failed（不是 canceled），worktree 保留。
func TestSweepFailsTasksWhoseSessionVanished(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", Session: "aa-a-c1", Worktree: "/p/aa-a-c1",
		State: TaskWorking, StartedAt: now.Add(-LivenessGrace - time.Minute).Format(time.RFC3339),
	})
	s.Upsert(A2ATask{
		ContextID: "c2", TaskID: "t2", Agent: "a", Session: "aa-a-c2", Worktree: "/p/aa-a-c2",
		State: TaskWorking, StartedAt: now.Format(time.RFC3339), // 還在寬限期內
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{AliveSessions: map[string]bool{"aa-a-c1": false, "aa-a-c2": false}}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	got, _ := LoadTasks(root)
	c1, _ := got.ByContext("c1")
	if c1.State != TaskFailed || !strings.Contains(c1.Detail, "vanished") {
		t.Fatalf("c1 = %q / %q, want failed", c1.State, c1.Detail)
	}
	// forensics：failed 的 worktree 保留。
	if c1.Worktree == "" || len(fake.Removed) != 0 {
		t.Fatalf("a vanished sandbox's worktree must be kept for forensics: worktree=%q removed=%#v", c1.Worktree, fake.Removed)
	}
	c2, _ := got.ByContext("c2")
	if c2.State != TaskWorking {
		t.Fatalf("c2 = %q; a task inside the liveness grace must be left alone", c2.State)
	}
}

// D6, running half: DrainQueue's revalidation never sees a row again once it
// reaches TaskWorking — it never returns to DrainQueue's claim loop. A caller
// revoked while its sandbox is already running must still be stopped from
// acting, not merely bounded by HardTimeout. sweep now closes that gap by
// rewriting the sandbox's policy to revoked (denying its very next tool call)
// and routing the actual teardown through the existing stopOnly/
// stopSessionGuarded path.
func TestSweepRevokesRunningTaskWhoseCallerWasRevoked(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetGrantLevel("peer-a", GrantFull)
	callers.Revoke("peer-a")
	_ = SaveCallers(root, callers)

	var agents AgentStore
	_ = agents.Add(Agent{Name: "a", ProjectDir: "/p/a", Capabilities: []string{"read"}, Enabled: true})
	_ = SaveAgents(root, agents)

	now := time.Now().UTC()
	const session = "aa-a-c1"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantFull,
		Session: session, State: TaskWorking, StartedAt: now.Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	if err := WriteSandboxPolicy(root, SandboxPolicy{
		Session: session, ContextID: "c1", Agent: "a", CallerID: "peer-a", Level: GrantFull,
	}); err != nil {
		t.Fatalf("WriteSandboxPolicy: %v", err)
	}

	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}

	// 政策檔必須立刻變成 revoked：gate 每次工具呼叫都重讀，這是唯一真正讓
	// 沙盒下一次工具呼叫就被擋的動作。
	pol, err := LoadSandboxPolicy(root, session)
	if err != nil {
		t.Fatalf("LoadSandboxPolicy: %v", err)
	}
	if pol.Level != GrantRevoked {
		t.Fatalf("policy level = %q, want revoked — a running sandbox's very next tool call must be denied", pol.Level)
	}

	// row 必須落在一個「說明原因」的終態，不是繼續 TaskWorking，也不是被
	// 誤標成 canceled（那會套錯 forensics 邊界）。
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskFailed || !strings.Contains(tk.Detail, "revoked") {
		t.Fatalf("row = %q / %q, want failed with a revocation detail", tk.State, tk.Detail)
	}

	// 既有的單一拆除路徑必須真的跑到：driver/tmux 停掉，不是只改政策檔跟
	// row 狀態就結束。
	if len(fake.Stopped) != 1 || fake.Stopped[0] != session {
		t.Fatalf("session not stopped: %#v — the existing single teardown path must still run", fake.Stopped)
	}
}

// task 9 review: ordering must be fail-safe in the OTHER direction too — the
// slower step (stopping the session) failing must never leave the sandbox
// MORE capable than the (already revoked) policy says. Capability removal
// happened first and does not get undone by a later failure.
func TestSweepPolicyRevocationSurvivesSessionStopFailure(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.Revoke("peer-a")
	_ = SaveCallers(root, callers)
	var agents AgentStore
	_ = agents.Add(Agent{Name: "a", ProjectDir: "/p/a", Capabilities: []string{"read"}, Enabled: true})
	_ = SaveAgents(root, agents)

	now := time.Now().UTC()
	const session = "aa-a-c1"
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantReadOnly,
		Session: session, State: TaskWorking, StartedAt: now.Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	if err := WriteSandboxPolicy(root, SandboxPolicy{
		Session: session, ContextID: "c1", Agent: "a", CallerID: "peer-a", Level: GrantReadOnly,
	}); err != nil {
		t.Fatalf("WriteSandboxPolicy: %v", err)
	}

	fake := &FakeSessionManager{FailOn: "stop"} // the slower teardown step fails
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}

	pol, err := LoadSandboxPolicy(root, session)
	if err != nil {
		t.Fatalf("LoadSandboxPolicy: %v", err)
	}
	if pol.Level != GrantRevoked {
		t.Fatalf("policy level = %q, want revoked regardless of the later session-stop failure", pol.Level)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskFailed {
		t.Fatalf("row state = %q, want failed even though the session itself could not be stopped", tk.State)
	}
}

// task 9 review: the mirror ordering proof — if the FIRST, fastest step
// (the policy rewrite itself) fails, nothing downstream may run: the row
// must stay exactly as capable as before (still TaskWorking, session never
// touched), never transitioned to a state that implies it was handled when
// its policy was, in fact, never revoked. session is deliberately an invalid
// sandbox session name (rejected by PolicyPath's regex) — a clean,
// deterministic, OS-independent way to force RevokeSandboxPolicy to fail
// without touching filesystem permissions.
func TestSweepLeavesRunningTaskUntouchedWhenPolicyRevocationFails(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.Revoke("peer-a")
	_ = SaveCallers(root, callers)
	var agents AgentStore
	_ = agents.Add(Agent{Name: "a", ProjectDir: "/p/a", Capabilities: []string{"read"}, Enabled: true})
	_ = SaveAgents(root, agents)

	now := time.Now().UTC()
	const session = "aa bad session" // fails sandboxSessionRe
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantReadOnly,
		Session: session, State: TaskWorking, StartedAt: now.Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}

	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("row state = %q, want still working — a failed policy revocation must retry next sweep, not fail the row while its policy was never actually touched", tk.State)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("session must not be stopped when its policy could not be revoked: %#v", fake.Stopped)
	}
}
