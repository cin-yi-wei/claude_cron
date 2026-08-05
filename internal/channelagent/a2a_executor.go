package channelagent

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// SandboxExecutor runs a delegated task in its own worktree + tmux session.
type SandboxExecutor struct {
	Root     string
	Sessions SessionManager
}

func NewSandboxExecutor(root string, sm SessionManager) *SandboxExecutor {
	return &SandboxExecutor{Root: root, Sessions: sm}
}

// SandboxRoot is the per-sandbox state dir (inbox/outbox/locks), separate from
// any binding root.
func SandboxRoot(root, session string) string {
	return filepath.Join(root, "sandboxes", session)
}

// SandboxWorktree places the sandbox checkout beside the project dir, named for
// the session so two contexts never share files.
func SandboxWorktree(projectDir, session string) string {
	if abs, err := filepath.Abs(projectDir); err == nil {
		projectDir = abs
	}
	return filepath.Join(filepath.Dir(projectDir), session)
}

// BranchFor namespaces sandbox branches so they are obvious in git output.
func BranchFor(session string) string { return "aa/" + session }

func (e *SandboxExecutor) markFailed(task A2ATask, detail string) {
	tasks, err := LoadTasks(e.Root)
	if err != nil {
		return
	}
	if cur, ok := tasks.ByContext(task.ContextID); ok {
		task = cur
	}
	task.State = TaskFailed
	task.Detail = detail
	task.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	tasks.Upsert(task)
	_ = SaveTasks(e.Root, tasks)
}

func (e *SandboxExecutor) Start(ctx context.Context, task A2ATask, prompt string) error {
	agents, err := LoadAgents(e.Root)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	agent, ok := agents.Get(task.Agent)
	if !ok {
		err := fmt.Errorf("unknown agent %q", task.Agent)
		e.markFailed(task, err.Error())
		return err
	}

	task.Worktree = SandboxWorktree(agent.ProjectDir, task.Session)
	task.Branch = BranchFor(task.Session)
	sandboxRoot := SandboxRoot(e.Root, task.Session)

	if err := Init(sandboxRoot); err != nil {
		e.markFailed(task, "init sandbox root: "+err.Error())
		return err
	}
	if err := e.Sessions.EnsureWorkspace(ctx, agent.ProjectDir, task.Branch, task.Worktree); err != nil {
		e.markFailed(task, "ensure worktree: "+err.Error())
		return err
	}
	if err := e.Sessions.Start(ctx, task.Session, task.Worktree, sandboxRoot); err != nil {
		e.markFailed(task, "start session: "+err.Error())
		return err
	}

	msg := SourceMessage{
		Platform:  "a2a",
		ChannelID: task.ContextID,
		MessageID: task.Session + "-" + task.ContextID,
		AuthorID:  task.CallerID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Content:   prompt,
	}
	if err := e.Sessions.Inject(ctx, sandboxRoot, msg); err != nil {
		e.markFailed(task, "inject: "+err.Error())
		return err
	}

	tasks, err := LoadTasks(e.Root)
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}
	if cur, ok := tasks.ByContext(task.ContextID); ok && !CanTransition(cur.State, TaskWorking) {
		return fmt.Errorf("cannot move task %s from %s to working", task.ContextID, cur.State)
	}
	task.State = TaskWorking
	tasks.Upsert(task)
	return SaveTasks(e.Root, tasks)
}
