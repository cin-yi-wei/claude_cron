package channelagent

import (
	"context"
	"fmt"
	"log"
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

// persist writes task into the store, preserving whatever other tasks are
// already there.
func (e *SandboxExecutor) persist(task A2ATask) error {
	tasks, err := LoadTasks(e.Root)
	if err != nil {
		return err
	}
	tasks.Upsert(task)
	return SaveTasks(e.Root, tasks)
}

// markFailed persists task as TaskFailed. The caller's in-memory task (which
// carries whatever Worktree/Branch/Session it had already computed) always
// wins over the on-disk copy for those three fields: a2a_server.go persists
// the task as TaskSubmitted with empty Worktree/Branch BEFORE ever calling
// Executor.Start, so the on-disk row can lag behind what's actually been
// created on disk by the time a later step fails. Clobbering the computed
// identity with the stale row would strand a real worktree or tmux session
// that nothing could then find to clean up.
func (e *SandboxExecutor) markFailed(task A2ATask, detail string) {
	tasks, err := LoadTasks(e.Root)
	if err != nil {
		return
	}
	worktree, branch, session := task.Worktree, task.Branch, task.Session
	if cur, ok := tasks.ByContext(task.ContextID); ok {
		task = cur
	}
	if worktree != "" {
		task.Worktree = worktree
	}
	if branch != "" {
		task.Branch = branch
	}
	if session != "" {
		task.Session = session
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

	// Persist the sandbox identity (Worktree/Branch/Session) BEFORE any real
	// side effect exists on disk (worktree creation, tmux session start), so
	// a crash partway through this function always leaves behind a task row
	// that already points at whatever got created — never an orphaned
	// worktree/session that nothing references (task-8 review finding 1).
	if err := e.persist(task); err != nil {
		e.markFailed(task, "persist sandbox identity: "+err.Error())
		return err
	}

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

	// From this point the sandbox is genuinely running: EnsureWorkspace,
	// Start and Inject all succeeded. a2a_server.go treats ANY non-nil error
	// returned from Start as TaskFailed, unconditionally — so a transient
	// failure to update bookkeeping here must NOT be surfaced as an error,
	// or a live, working sandbox gets mislabeled failed while its session
	// keeps running untracked (task-8 review finding 2). We choose to return
	// success and leave the row for the normal lifecycle to reconcile,
	// rather than fabricate a failure that contradicts the sandbox's actual
	// state.
	tasks, err := LoadTasks(e.Root)
	if err != nil {
		log.Printf("a2a: session %s is running but reloading the task store to mark it working failed: %v", task.Session, err)
		return nil
	}
	if cur, ok := tasks.ByContext(task.ContextID); ok && !CanTransition(cur.State, TaskWorking) {
		log.Printf("a2a: session %s is running but task %s is in state %s (not submitted); leaving its state alone", task.Session, task.ContextID, cur.State)
		return nil
	}
	task.State = TaskWorking
	tasks.Upsert(task)
	if err := SaveTasks(e.Root, tasks); err != nil {
		log.Printf("a2a: session %s is running but persisting TaskWorking failed: %v", task.Session, err)
	}
	return nil
}
