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
	_, _, _ = newExecutorFixture(t)
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

func TestSandboxExecutorMarksFailedWhenStartFails(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)
	ex := NewSandboxExecutor(root, &FakeSessionManager{FailOn: "start"})

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted}
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error when session start fails")
	}
	tasks, _ := LoadTasks(root)
	got, _ := tasks.ByContext("c1")
	if got.State != TaskFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

func TestSandboxExecutorRejectsUnknownAgent(t *testing.T) {
	root, _, ex := newExecutorFixture(t)
	_ = root
	task := A2ATask{ContextID: "c1", Agent: "ghost", Session: "aa-ghost-c1", State: TaskSubmitted}
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}
