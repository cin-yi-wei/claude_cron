package channelagent

import (
	"context"
	"strings"
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
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted}

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
	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	tasks.Upsert(task)
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
}

func TestSandboxExecutorMarksFailedWhenStartFails(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)
	ex := NewSandboxExecutor(root, &FakeSessionManager{FailOn: "start"})

	session := SessionNameFor("codereview", "c1")
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted}
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
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted}
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

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskSubmitted}
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
