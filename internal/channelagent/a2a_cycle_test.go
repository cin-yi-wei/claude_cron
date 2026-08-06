package channelagent

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestA2ACycleRunsAllStages is the test the final review (2026-08-06) found
// missing: cmd/claude-cron/main.go wired collect → sweep → drain → ensure
// drivers → enqueue callbacks → prune directly into its serve goroutine as
// six independent calls, and nothing anywhere exercised that wiring —
// deleting any one of those six lines (EnsureSandboxDrivers, say) would not
// have turned a single test red. That is structurally the same gap
// DIAGNOSIS.md records from an earlier cycle that shipped a feature nothing
// ever called.
//
// The fix moves the six calls into RunA2ACycleOnce (a2a_cycle.go), which
// main.go now calls exactly once per tick. This test seeds one task row per
// stage, each crafted so ONLY that stage can move it: if a future edit drops
// a stage from RunA2ACycleOnce, exactly that row's assertion fails.
func TestA2ACycleRunsAllStages(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()

	// caller/agent shared by every row below.
	var callers CallerStore
	if err := callers.Register("peer", "s"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	callers.Approve("peer", []string{"read"})
	// A loopback destination fails ValidateCallbackURL's public-IP check
	// deterministically and SYNCHRONOUSLY (no real network call, no
	// goroutine race) — see a2a_callback.go's isPublicIP. That is exactly
	// what stage 5 (EnqueueTerminalCallbacks) needs to touch this row's
	// CallbackState without this test depending on outbound network access.
	callers.SetCallback("peer", "https://127.0.0.1:9/hook", "")
	if err := SaveCallers(root, callers); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}
	var agents AgentStore
	if err := agents.Add(Agent{Name: "a", ProjectDir: "/p/a", Capabilities: []string{"read"}, Enabled: true}); err != nil {
		t.Fatalf("Add agent: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	// --- stage 1: CollectResults --- a TaskWorking row with a matching
	// result file sitting in its sandbox's outbox/pending. Only collect can
	// move this to completed.
	collectSession := SessionNameFor("a", "collect1")
	var seed TaskStore
	seed.Upsert(A2ATask{
		ContextID: "collect1", Agent: "a", CallerID: "peer", Session: collectSession,
		State: TaskWorking, LastMessageID: resultMsgID, StartedAt: now.Format(time.RFC3339),
	})

	// --- stage 2: SweepTimeouts --- a TaskWorking row started 3 hours ago
	// (past HardTimeout=2h). Only sweep can move this to canceled.
	sweepSession := SessionNameFor("a", "sweep1")
	seed.Upsert(A2ATask{
		ContextID: "sweep1", Agent: "a", CallerID: "peer", Session: sweepSession,
		State: TaskWorking, StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	})

	// --- stage 3: DrainQueue --- a queued (submitted) row with a valid
	// caller/agent. Only drain can claim it (submitted -> dispatching).
	seed.Upsert(A2ATask{
		ContextID: "drain1", Agent: "a", CallerID: "peer", State: TaskSubmitted,
		Prompt: "work", StartedAt: now.Format(time.RFC3339),
	})

	// --- stage 4: EnsureSandboxDrivers --- a TaskWorking row with a live
	// "aa-" session. Only this stage calls SandboxDriver.Ensure for it.
	driverSession := SessionNameFor("a", "driver1")
	seed.Upsert(A2ATask{
		ContextID: "driver1", Agent: "a", CallerID: "peer", Session: driverSession,
		State: TaskWorking, StartedAt: now.Format(time.RFC3339),
	})

	// --- stage 5: EnqueueTerminalCallbacks --- an already-terminal row
	// whose caller has a CallbackURL. Only this stage ever writes
	// CallbackState.
	seed.Upsert(A2ATask{
		ContextID: "callback1", Agent: "a", CallerID: "peer", State: TaskCompleted,
		StartedAt: now.Format(time.RFC3339), CompletedAt: now.Format(time.RFC3339),
	})

	// --- stage 6: PruneTasks --- a terminal row completed long past
	// TaskRetention (14d), with no Worktree/SessionStopPending in the way.
	// Only prune ever removes a row outright.
	seed.Upsert(A2ATask{
		ContextID: "prune1", Agent: "a", CallerID: "peer", State: TaskCompleted,
		StartedAt:   now.Add(-20 * 24 * time.Hour).Format(time.RFC3339),
		CompletedAt: now.Add(-20 * 24 * time.Hour).Format(time.RFC3339),
	})

	if err := SaveTasks(root, seed); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	writeSandboxResult(t, root, collectSession, "done")
	if err := Init(SandboxRoot(root, driverSession)); err != nil {
		t.Fatalf("Init driver sandbox: %v", err)
	}

	var sent []string
	stubTmuxPane(t, "", &sent) // no real tmux for stage 4's driver loop

	fakeSM := &FakeSessionManager{}
	stub := &StubExecutor{}
	driver := NewSandboxDriver(root, time.Second)
	defer driver.StopAll()
	// CallbackDispatcher.Wait blocks until its OWN ctx is done (unlike
	// SandboxDriver, whose per-session cancel funcs are independent of the
	// ctx passed to Ensure) — cbCancel must run before cb.Wait(), so declare
	// Wait's defer FIRST: defers run in reverse (LIFO) declaration order.
	cbCtx, cbCancel := context.WithCancel(context.Background())
	cb := NewCallbackDispatcher(cbCtx, root)
	defer cb.Wait()
	defer cbCancel()

	var stderr bytes.Buffer
	RunA2ACycleOnce(context.Background(), root, now, fakeSM, stub, driver, cb, &stderr)

	// stage 1
	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if tk, ok := tasks.ByContext("collect1"); !ok || tk.State != TaskCompleted {
		t.Fatalf("stage 1 (CollectResults) did not run: collect1 = %#v", tk)
	}

	// stage 2
	if tk, ok := tasks.ByContext("sweep1"); !ok || tk.State != TaskCanceled {
		t.Fatalf("stage 2 (SweepTimeouts) did not run: sweep1 = %#v", tk)
	}

	// stage 3
	if tk, ok := tasks.ByContext("drain1"); !ok || tk.State == TaskSubmitted {
		t.Fatalf("stage 3 (DrainQueue) did not run: drain1 = %#v", tk)
	}
	if stub.Calls != 1 {
		t.Fatalf("stage 3 (DrainQueue) never called Executor.Start: calls = %d", stub.Calls)
	}

	// stage 4
	running := driver.Running()
	found := false
	for _, s := range running {
		if s == driverSession {
			found = true
		}
	}
	if !found {
		t.Fatalf("stage 4 (EnsureSandboxDrivers) did not run: Running() = %#v, want it to include %s", running, driverSession)
	}

	// stage 5
	if tk, ok := tasks.ByContext("callback1"); !ok || tk.CallbackState == "" {
		t.Fatalf("stage 5 (EnqueueTerminalCallbacks) did not run: callback1 = %#v", tk)
	}

	// stage 6
	if _, ok := tasks.ByContext("prune1"); ok {
		t.Fatal("stage 6 (PruneTasks) did not run: prune1 row is still present")
	}
}
