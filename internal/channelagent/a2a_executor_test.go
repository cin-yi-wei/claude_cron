package channelagent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

func newExecutorFixture(t *testing.T) (string, *FakeSessionManager, *SandboxExecutor) {
	t.Helper()
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	fake := &FakeSessionManager{}
	return root, fake, NewSandboxExecutor(root, fake)
}

func TestSandboxExecutorCreatesWorkspaceStartsAndInjects(t *testing.T) {
	root, fake, ex := newExecutorFixture(t)
	// task 6: submitted -> working is no longer a legal direct transition
	// (CanTransition requires the intermediate TaskDispatching claim state);
	// by the time anything calls Executor.Start in production, the caller
	// (a2a_server.go or DrainQueue) has already flipped the row to
	// TaskDispatching in the same locked section that reserved its capacity
	// slot. Reproduce that precondition here.
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskDispatching, Level: GrantReadOnly}

	if err := ex.Start(context.Background(), task, "please review"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(fake.Workspaces) != 1 {
		t.Fatalf("workspace not created: %#v", fake.Workspaces)
	}
	if len(fake.Started) != 1 || fake.Started[0] != task.Session {
		t.Fatalf("session not started: %#v", fake.Started)
	}
	if len(fake.Injected) != 1 || fake.Injected[0].Content != "please review" {
		t.Fatalf("prompt not injected: %#v", fake.Injected)
	}

	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext("c1")
	if !ok || got.State != TaskWorking {
		t.Fatalf("task should be working, got %#v", got)
	}
	if got.Worktree == "" || got.Branch == "" {
		t.Fatalf("worktree/branch not recorded: %#v", got)
	}
}

func TestSandboxExecutorUsesAAPrefixedIsolatedPaths(t *testing.T) {
	session := SessionNameFor("codereview", "c1")
	wt := SandboxWorktree("/p/x", session)
	if !strings.Contains(wt, session) {
		t.Fatalf("worktree %q must be unique per session", wt)
	}
	if strings.HasSuffix(wt, "/x") {
		t.Fatal("sandbox must not reuse the agent project dir itself")
	}
	if br := BranchFor(session); !strings.HasPrefix(br, "aa/") {
		t.Fatalf("branch %q should be namespaced under aa/", br)
	}
}

// assertFailedRowKeepsIdentity is shared by every "started side effects, then
// failed" test: a task marked TaskFailed after EnsureWorkspace succeeded must
// still carry the Worktree/Branch it created, or nobody can find the orphan
// to clean it up (task-8 review finding 3).
func assertFailedRowKeepsIdentity(t *testing.T, root, contextID, wantWorktree, wantBranch string) {
	t.Helper()
	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext(contextID)
	if !ok {
		t.Fatalf("task %s not found", contextID)
	}
	if got.State != TaskFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if got.Worktree != wantWorktree {
		t.Fatalf("worktree = %q, want %q (failed row must keep the sandbox identity so it can be cleaned up)", got.Worktree, wantWorktree)
	}
	if got.Branch != wantBranch {
		t.Fatalf("branch = %q, want %q", got.Branch, wantBranch)
	}
}

// seedTask persists task into the store exactly as given, standing in for
// any actor that writes a row independently of the executor: a2a_server.go
// before it ever calls Executor.Start, an operator cancel path, or task 11's
// future sweep.
func seedTask(t *testing.T, root string, task A2ATask) {
	t.Helper()
	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	tasks.Upsert(task)
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
}

// seedSubmittedTask mimics what a2a_server.go actually does before ever
// calling TaskExecutor.Start: it persists the task as TaskSubmitted with no
// Worktree/Branch yet (those are the executor's job to compute). Tests must
// reproduce this pre-existing empty-worktree row on disk, or they cannot
// catch markFailed clobbering the freshly computed identity with it
// (task-8 review finding 3) — a fixture that starts with an empty store
// masks the bug because markFailed then has nothing on disk to overwrite
// with.
func seedSubmittedTask(t *testing.T, root string, task A2ATask) {
	t.Helper()
	seedTask(t, root, task)
}

func TestSandboxExecutorMarksFailedWhenStartFails(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)
	ex := NewSandboxExecutor(root, &FakeSessionManager{FailOn: "start"})

	session := SessionNameFor("codereview", "c1")
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
	seedSubmittedTask(t, root, task)
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error when session start fails")
	}
	assertFailedRowKeepsIdentity(t, root, "c1", SandboxWorktree("/p/x", session), BranchFor(session))
}

func TestSandboxExecutorMarksFailedWhenInjectFails(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)
	ex := NewSandboxExecutor(root, &FakeSessionManager{FailOn: "inject"})

	session := SessionNameFor("codereview", "c1")
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
	seedSubmittedTask(t, root, task)
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error when inject fails")
	}
	assertFailedRowKeepsIdentity(t, root, "c1", SandboxWorktree("/p/x", session), BranchFor(session))
}

func TestSandboxExecutorRejectsUnknownAgent(t *testing.T) {
	root, _, ex := newExecutorFixture(t)
	_ = root
	task := A2ATask{ContextID: "c1", Agent: "ghost", Session: SessionNameFor("ghost", "c1"), State: TaskSubmitted}
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

// orderSpy wraps FakeSessionManager to observe what has already been
// persisted to the task store by the time EnsureWorkspace is invoked. This
// pins the ordering from task-8 review finding 1: the sandbox identity
// (Worktree/Branch/Session) must be written to disk BEFORE any real side
// effect (worktree creation, tmux start) happens, so a crash mid-flight
// always finds a task row that already points at whatever got created.
type orderSpy struct {
	*FakeSessionManager
	root      string
	contextID string

	sawWorktree string
	sawBranch   string
	sawSession  string
	sawTask     bool
}

func (o *orderSpy) EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error {
	tasks, _ := LoadTasks(o.root)
	if cur, ok := tasks.ByContext(o.contextID); ok {
		o.sawTask = true
		o.sawWorktree = cur.Worktree
		o.sawBranch = cur.Branch
		o.sawSession = cur.Session
	}
	return o.FakeSessionManager.EnsureWorkspace(ctx, projectDir, branch, worktree)
}

func TestSandboxExecutorPersistsIdentityBeforeSessionSideEffects(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)

	session := SessionNameFor("codereview", "c1")
	spy := &orderSpy{FakeSessionManager: &FakeSessionManager{}, root: root, contextID: "c1"}
	ex := NewSandboxExecutor(root, spy)

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
	if err := ex.Start(context.Background(), task, "x"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !spy.sawTask {
		t.Fatal("task row must exist before EnsureWorkspace runs")
	}
	wantWorktree := SandboxWorktree("/p/x", session)
	wantBranch := BranchFor(session)
	if spy.sawWorktree != wantWorktree {
		t.Fatalf("worktree not persisted before EnsureWorkspace: got %q want %q", spy.sawWorktree, wantWorktree)
	}
	if spy.sawBranch != wantBranch {
		t.Fatalf("branch not persisted before EnsureWorkspace: got %q want %q", spy.sawBranch, wantBranch)
	}
	if spy.sawSession != session {
		t.Fatalf("session not persisted before EnsureWorkspace: got %q want %q", spy.sawSession, session)
	}
}

// TestSandboxExecutorPersistGuardRefusesToReviveTerminalRow pins the
// round-2 latent-issue finding: persist() must not silently revert an
// already-terminal row back to Submitted. Unreachable in production today
// (nothing yet cancels a task before Executor.Start runs), but task 11 adds
// exactly such a sweep, so the guard is required now.
func TestSandboxExecutorPersistGuardRefusesToReviveTerminalRow(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)

	session := SessionNameFor("codereview", "c1")
	seedTask(t, root, A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskCanceled, Detail: "canceled by operator"})

	fake := &FakeSessionManager{}
	ex := NewSandboxExecutor(root, fake)
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
	if err := ex.Start(context.Background(), task, "x"); err != nil {
		t.Fatalf("Start should report success for an already-terminal task (an error here would let a2a_server.go clobber it to Failed), got: %v", err)
	}

	if len(fake.Workspaces) != 0 || len(fake.Started) != 0 || len(fake.Injected) != 0 {
		t.Fatalf("no side effect should run once the row is already terminal: workspaces=%v started=%v injected=%v", fake.Workspaces, fake.Started, fake.Injected)
	}

	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("task row must not disappear")
	}
	if got.State != TaskCanceled || got.Detail != "canceled by operator" {
		t.Fatalf("terminal row must be left untouched, got %#v", got)
	}
}

// TestPersistDoesNotRegressPromptMergedWhileClaimIsInFlight pins a task 7
// review finding (a2a_executor.go:156, carried over from task 6): persist()
// used to Upsert task wholesale — Start's OWN claim-time copy, taken back
// when DrainQueue first claimed this dispatch. A follow-up
// (a2a_server.go's message/send follow-up path) can land on the very same
// row and merge a newer Prompt onto it while Start is still between that
// claim and its own persist() call — Start does not hold tasksMu across that
// whole span, only around each individual WithTasks call, and the
// follow-up path never needs the session lock (it only ever touches Prompt,
// under tasksMu). Interleaving: DrainQueue claims c1 (Prompt "first") and
// enters Start; a follow-up merges Prompt to "follow up"; Start then reaches
// persist and, pre-fix, wrote Prompt "first" straight back over it.
//
// This is proved with a real goroutine, not a hand-simulated sequence: the
// test goroutine calling persist() is forced to block on tasksMu (grabbed
// directly, bypassing WithTasks) until the main goroutine has performed the
// "follow-up" write and released it — a deterministic hold on the exact
// window persist() runs in, not a timing gamble. Because tasksMu excludes
// all access to the store, there is no race for -race to flag; what's under
// test is pure ordering, and the mutex is what makes that ordering
// unambiguous rather than probabilistic.
func TestPersistDoesNotRegressPromptMergedWhileClaimIsInFlight(t *testing.T) {
	root := t.TempDir()
	claimTime := A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskDispatching,
		Level: GrantReadOnly, Prompt: "first",
	}
	seedTask(t, root, claimTime)

	ex := NewSandboxExecutor(root, &FakeSessionManager{})

	// 直接搶下 tasksMu(套件內測試可以碰,而 persist 底下的 WithTasks 一定要
	// 這把鎖才能往下走),逼下面那條呼叫 persist 的 goroutine 卡在它自己的
	// Lock() 裡 —— 不管排程怎麼跑,它在我們放手之前絕對碰不到 tasks.json。
	tasksMu.Lock()

	done := make(chan error, 1)
	go func() {
		done <- ex.persist(claimTime) // task.Prompt == "first",Start 拿到的舊副本
	}()

	// 模擬追問在這個窗口內把 Prompt 併過去。因為我們自己握著 tasksMu,直接
	// 讀寫檔案是安全的(等同 a2a_server.go 的 isFollowUp 分支在 WithTasks
	// 內做的事,只是這裡沒有巢狀鎖的問題,因為呼叫的是底層 Load/Save 而不是
	// 再進一次 WithTasks)。
	live, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	cur, ok := live.ByContext("c1")
	if !ok {
		t.Fatal("seeded row must exist")
	}
	cur.Prompt = "follow up"
	live.Upsert(cur)
	if err := SaveTasks(root, live); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	tasksMu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("persist: %v", err)
	}

	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.Prompt != "follow up" {
		t.Fatalf("Prompt = %q, want %q (persist must not overwrite a follow-up merged after its claim-time copy was taken)", tk.Prompt, "follow up")
	}
}

// cancelDuringInject wraps FakeSessionManager and, immediately after a
// successful Inject, flips the on-disk task row to TaskCanceled —
// simulating an external actor canceling the task in the narrow window
// while its session was still starting. This is round 2's terminal-conflict
// sub-case: by the time Start reaches its final check, EnsureWorkspace and
// Sessions.Start have already succeeded, so a real (fake) session is
// "running" that the now-canceled row no longer references.
type cancelDuringInject struct {
	*FakeSessionManager
	root      string
	contextID string
}

func (c *cancelDuringInject) Inject(ctx context.Context, root string, msg SourceMessage) error {
	if err := c.FakeSessionManager.Inject(ctx, root, msg); err != nil {
		return err
	}
	tasks, _ := LoadTasks(c.root)
	if cur, ok := tasks.ByContext(c.contextID); ok {
		cur.State = TaskCanceled
		cur.Detail = "canceled mid-start"
		tasks.Upsert(cur)
		_ = SaveTasks(c.root, tasks)
	}
	return nil
}

// TestSandboxExecutorTearsDownSessionWhenTaskCanceledDuringStart pins the
// chosen resolution for round-2 finding 2: when the row is found terminal
// after the session is already up, Start must stop that session (no
// orphan) and report success (never an error a2a_server.go would clobber
// the terminal row with), leaving the canceled row exactly as it was.
func TestSandboxExecutorTearsDownSessionWhenTaskCanceledDuringStart(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)

	session := SessionNameFor("codereview", "c1")
	spy := &cancelDuringInject{FakeSessionManager: &FakeSessionManager{}, root: root, contextID: "c1"}
	ex := NewSandboxExecutor(root, spy)

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
	seedSubmittedTask(t, root, task)

	if err := ex.Start(context.Background(), task, "x"); err != nil {
		t.Fatalf("Start should report success (row already terminal) rather than an error a2a_server.go would clobber, got: %v", err)
	}

	if len(spy.Stopped) != 1 || spy.Stopped[0] != session {
		t.Fatalf("orphaned session must be stopped, got Stopped=%#v", spy.Stopped)
	}

	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("task row must not disappear")
	}
	if got.State != TaskCanceled || got.Detail != "canceled mid-start" {
		t.Fatalf("canceled row must be left exactly as it was, got %#v", got)
	}
}

// alreadyWorkingDuringInject wraps FakeSessionManager and, immediately after
// a successful Inject, flips the on-disk row to TaskWorking — simulating the
// row having already moved past whatever CanTransition(_, TaskWorking) will
// accept by the time Start reaches its final check (the exact branch that
// used to return without recording anything).
type alreadyWorkingDuringInject struct {
	*FakeSessionManager
	root      string
	contextID string
}

func (a *alreadyWorkingDuringInject) Inject(ctx context.Context, root string, msg SourceMessage) error {
	if err := a.FakeSessionManager.Inject(ctx, root, msg); err != nil {
		return err
	}
	tasks, _ := LoadTasks(a.root)
	if cur, ok := tasks.ByContext(a.contextID); ok {
		cur.State = TaskWorking
		tasks.Upsert(cur)
		_ = SaveTasks(a.root, tasks)
	}
	return nil
}

// TestSandboxExecutorRecordsLastMessageIDEvenWhenRowCannotTransition pins
// the D10-4 fix (round 10 review, Minor): Inject already genuinely delivered
// the message into a real sandbox before Start's final check runs, so
// whatever the row's bookkeeping ends up being at that point, LastMessageID
// must still be recorded — without it, CollectResults can never match this
// sandbox's eventual reply and the task is permanently unmatchable, even
// though the branch itself correctly leaves State/Detail alone (that
// decision belongs to whichever path actually put the row in this state).
func TestSandboxExecutorRecordsLastMessageIDEvenWhenRowCannotTransition(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)

	session := SessionNameFor("codereview", "c1")
	spy := &alreadyWorkingDuringInject{FakeSessionManager: &FakeSessionManager{}, root: root, contextID: "c1"}
	ex := NewSandboxExecutor(root, spy)

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted, Level: GrantReadOnly}
	seedSubmittedTask(t, root, task)

	if err := ex.Start(context.Background(), task, "x"); err != nil {
		t.Fatalf("Start should report success, got: %v", err)
	}

	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("task row must not disappear")
	}
	if got.State != TaskWorking {
		t.Fatalf("state must be left exactly as the concurrent actor set it, got %s", got.State)
	}
	if got.LastMessageID == "" {
		t.Fatal("LastMessageID must still be recorded even when the row could not be transitioned by this call")
	}
}

// realInjectSessionManager fakes every side effect except Inject: workspace
// creation and session start/stop never touch git or tmux (inherited from
// FakeSessionManager), but Inject calls the real IngestMessages. That is the
// only way to prove a follow-up message was genuinely queued — asserting on
// FakeSessionManager.Injected (a recorded call) cannot distinguish "queued"
// from "silently deduped", because IngestMessages returning created=0 with no
// error is exactly the bug this test guards against.
type realInjectSessionManager struct {
	*FakeSessionManager
}

func (r *realInjectSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	_, err := IngestMessages(ctx, root, []SourceMessage{msg})
	return err
}

// TestSandboxExecutorSecondMessageInSameContextIsDelivered pins the task-5
// fix: a follow-up Start() call in the same contextId (as happens when a
// caller replies within the sandbox's post-completion retention window) must
// actually reach the sandbox's inbox, not be silently dropped by
// IngestMessages' platform:channel:messageID dedup. Asserting on the real
// on-disk inbox (not a mock's call count) is the only assertion that can
// catch a call that "succeeds" while doing nothing.
func TestSandboxExecutorSecondMessageInSameContextIsDelivered(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	sm := &realInjectSessionManager{FakeSessionManager: &FakeSessionManager{}}
	ex := NewSandboxExecutor(root, sm)

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted, Level: GrantReadOnly}

	if err := ex.Start(context.Background(), task, "first"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := ex.Start(context.Background(), task, "second"); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	dir := pathIn(SandboxRoot(root, task.Session), "inbox", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("inbox has %d job(s); the second message was deduped away", len(entries))
	}
}

// TestNextInjectedMessageIDIsAlwaysDistinct exercises the id generator
// directly rather than through timing: two Start calls for the same
// contextId are reachable concurrently (a same-caller follow-up sent while
// the prior task is still TaskWorking runs in its own goroutine, and
// task 11's DrainQueue can also Start the same task from another goroutine),
// so a timestamp alone only makes a collision unlikely, not impossible. The
// atomic counter folded into every id is what makes this actually
// structural: run it concurrently and require every id to be unique.
func TestNextInjectedMessageIDIsAlwaysDistinct(t *testing.T) {
	const n = 200
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = nextInjectedMessageID("aa-x-c1")
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = true
	}
}

// 政策檔必須在 Sessions.Start 之前就落地:session 一起來就能發工具呼叫,晚一步
// 寫等於開了一個沒有約束的窗口。
func TestSandboxExecutorWritesPolicyBeforeStart(t *testing.T) {
	root, fake, ex := newExecutorFixture(t)
	var policyAtStart SandboxPolicy
	var policyErr error
	fake.OnStart = func(session string) {
		policyAtStart, policyErr = LoadSandboxPolicy(root, session)
	}
	task := A2ATask{
		ContextID: "c1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted, Level: GrantDevelop,
	}
	if err := ex.Start(context.Background(), task, "go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if policyErr != nil {
		t.Fatalf("policy not present when the session started: %v", policyErr)
	}
	if policyAtStart.Level != GrantDevelop || policyAtStart.CallerID != "peer-a" {
		t.Fatalf("policy = %#v", policyAtStart)
	}
	if policyAtStart.Worktree == "" || policyAtStart.SandboxRoot == "" {
		t.Fatalf("policy must pin both scopes: %#v", policyAtStart)
	}
}

// 沒有有效等級的 row 不可以起沙盒 —— 那會是一個永遠被 gate 全拒的殭屍。
func TestSandboxExecutorRefusesTaskWithoutLevel(t *testing.T) {
	root, fake, ex := newExecutorFixture(t)
	task := A2ATask{
		ContextID: "c1", Agent: "codereview",
		Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted,
	}
	if err := ex.Start(context.Background(), task, "go"); err == nil {
		t.Fatal("a task with no grant level must not start a sandbox")
	}
	if len(fake.Started) != 0 {
		t.Fatalf("started %v despite having no grant level", fake.Started)
	}
	tasks, _ := LoadTasks(root)
	tk, _ := tasks.ByContext("c1")
	if tk.State != TaskFailed {
		t.Fatalf("state = %q, want failed", tk.State)
	}
}

// trustOrderSpy wraps FakeSessionManager to prove TrustFolder actually ran
// BEFORE Start, not just that both ran at some point during Start.
// FakeSessionManager records each call into its own slice (Trusted vs
// Started), so asserting both slices are non-empty after Start returns
// cannot distinguish "trusted then started" from "started then trusted" —
// only observing Trusted's state from INSIDE Start, as it fires, can.
type trustOrderSpy struct {
	*FakeSessionManager
	trustedBeforeStart bool
}

func (s *trustOrderSpy) Start(ctx context.Context, session, cwd, registryRoot string) error {
	s.trustedBeforeStart = len(s.Trusted) == 1
	return s.FakeSessionManager.Start(ctx, session, cwd, registryRoot)
}

// EnsureFolderTrusted 目前是死碼(零正式呼叫端),所以資料夾信任對話框在每個
// 沙盒開機時都會跳。接上它 —— 但一定要走 SessionManager,否則單元測試會改寫
// operator 的 ~/.claude.json。信任必須在 session 啟動之前完成:trustOrderSpy
// 從 Start 內部觀察 Trusted 是否已經記錄,而不是只在 Start 回傳後檢查兩個
// slice 都非空(那種寫法連「先 Start 再 Trust」都會誤判成通過)。
func TestSandboxExecutorTrustsWorktreeBeforeStart(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	spy := &trustOrderSpy{FakeSessionManager: &FakeSessionManager{}}
	ex := NewSandboxExecutor(root, spy)

	task := A2ATask{
		ContextID: "c1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted, Level: GrantDevelop,
	}
	if err := ex.Start(context.Background(), task, "go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(spy.Trusted) != 1 {
		t.Fatalf("TrustFolder calls = %#v, want exactly one", spy.Trusted)
	}
	if spy.Trusted[0] != SandboxWorktree("/p/x", task.Session) {
		t.Fatalf("trusted %q, want the sandbox worktree", spy.Trusted[0])
	}
	if !spy.trustedBeforeStart {
		t.Fatal("TrustFolder must run before Sessions.Start, not after")
	}
}

// 信任只是省一個對話框,不是必要條件(第 3 層 backstop 仍在)。它失敗時
// dispatch 必須照常完成,否則一個 ~/.claude.json 的暫時性讀寫錯誤就會讓每一
// 個委派任務失敗。
func TestSandboxExecutorContinuesWhenTrustFails(t *testing.T) {
	_, fake, ex := newExecutorFixture(t)
	fake.FailOn = "trust"
	task := A2ATask{
		ContextID: "c1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted, Level: GrantDevelop,
	}
	if err := ex.Start(context.Background(), task, "go"); err != nil {
		t.Fatalf("a trust failure must not abort dispatch: %v", err)
	}
	if len(fake.Started) != 1 {
		t.Fatalf("session not started: %#v", fake.Started)
	}
}
