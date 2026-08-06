package channelagent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync/atomic"
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

// injectSeq is a process-wide monotonic counter mixed into every injected
// message's id. Two Start calls for the same contextId are NOT guaranteed to
// be sequential: the same caller may send a follow-up while the prior task
// is still TaskWorking (its HTTP request runs in its own goroutine alongside
// the in-flight Start), and task 11's DrainQueue can also Start the same
// task from a different goroutine. A timestamp alone only makes a collision
// unlikely; atomically incrementing this counter makes it structurally
// impossible regardless of clock resolution or goroutine interleaving.
var injectSeq uint64

// nextInjectedMessageID builds the MessageID for a message injected into
// session's sandbox. It must differ across every call, including concurrent
// ones for the same session: IngestMessages dedups on
// platform:channel:messageID (watcher.go), so a repeated id silently drops
// the message instead of queuing it. The timestamp keeps ids ordered and
// readable for debugging; the atomic counter is what actually guarantees
// uniqueness.
func nextInjectedMessageID(session string) string {
	seq := atomic.AddUint64(&injectSeq, 1)
	return fmt.Sprintf("%s-%d-%d", session, time.Now().UnixNano(), seq)
}

// isTerminal reports whether a TaskState is one CanTransition treats as
// final: nothing may transition out of it. Round 2 of the task-8 review
// treats "already terminal" as its own case, distinct from a transient I/O
// error: it means some other actor (an operator, or task 11's future sweep)
// already decided this task's fate, and that decision must not be reverted.
func isTerminal(s TaskState) bool {
	return s == TaskFailed || s == TaskCompleted || s == TaskCanceled
}

// errTaskAlreadyTerminal signals that persist found the on-disk row already
// terminal. Callers must NOT let this surface as a returned error from
// Start: a2a_server.go marks ANY non-nil Start() error TaskFailed
// unconditionally, using its own stale pre-call copy of the task — which
// would clobber the true terminal state this guard exists to protect.
var errTaskAlreadyTerminal = errors.New("task already reached a terminal state")

// persist writes task into the store, preserving whatever other tasks are
// already there. It refuses to overwrite an already-terminal row: without
// this guard, a call racing behind an external cancel/complete/fail would
// silently revive the row back to task.State (task-8 review round 2,
// latent-issue finding — unreachable today, but task 11 adds a sweep that
// would hit it).
func (e *SandboxExecutor) persist(task A2ATask) error {
	return WithTasks(e.Root, func(tasks *TaskStore) error {
		if cur, ok := tasks.ByContext(task.ContextID); ok && isTerminal(cur.State) {
			return errTaskAlreadyTerminal
		}
		tasks.Upsert(task)
		return nil
	})
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
	_ = WithTasks(e.Root, func(tasks *TaskStore) error {
		worktree, branch, session := task.Worktree, task.Branch, task.Session
		t := task
		if cur, ok := tasks.ByContext(t.ContextID); ok {
			t = cur
		}
		if worktree != "" {
			t.Worktree = worktree
		}
		if branch != "" {
			t.Branch = branch
		}
		if session != "" {
			t.Session = session
		}
		t.State = TaskFailed
		t.Detail = detail
		t.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		tasks.Upsert(t)
		return nil
	})
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

	// 沒有有效等級的 row 不可以起沙盒:政策檔會寫不出可用的 Level,gate 會
	// 全面拒絕,結果是一個活著卻什麼都做不了、還佔著併發額度的殭屍。
	if !ValidGrantLevel(task.Level) {
		err := fmt.Errorf("task %s has no valid grant level", task.ContextID)
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
		if errors.Is(err, errTaskAlreadyTerminal) {
			// The row was already terminal (e.g. canceled) before we ever
			// created a worktree or session. Nothing has happened yet, so
			// there is nothing to tear down: bail out without touching the
			// row, and report success rather than an error a2a_server.go
			// would clobber it with (task-8 review round 2, finding 2).
			return nil
		}
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

	// 政策檔必須在 session 起來之前落地:session 一起來就能發工具呼叫,晚一步
	// 寫等於開了一個沒有約束的窗口。寫入失敗 = dispatch 失敗,不可以降級成
	// 「先開起來再說」。
	if err := WriteSandboxPolicy(e.Root, SandboxPolicy{
		Session:     task.Session,
		ContextID:   task.ContextID,
		Agent:       task.Agent,
		CallerID:    task.CallerID,
		Level:       task.Level,
		Worktree:    cleanAbs(task.Worktree),
		SandboxRoot: cleanAbs(sandboxRoot),
	}); err != nil {
		e.markFailed(task, "write sandbox policy: "+err.Error())
		return err
	}

	if err := e.Sessions.Start(ctx, task.Session, task.Worktree, sandboxRoot); err != nil {
		e.markFailed(task, "start session: "+err.Error())
		return err
	}

	msg := SourceMessage{
		Platform:  "a2a",
		ChannelID: task.ContextID,
		// Unique per message, not per context: IngestMessages dedups on
		// platform:channel:messageID, so a constant ID would silently drop
		// every follow-up in the same contextId.
		MessageID: nextInjectedMessageID(task.Session),
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
	// returned from Start as TaskFailed, unconditionally, using its own
	// stale pre-call copy of the task — so nothing below may return an
	// error, or a live, working sandbox gets mislabeled failed (or a true
	// terminal state gets clobbered back to Failed). Every branch here
	// reports success; the two terminal-vs-transient sub-cases are handled
	// distinctly (task-8 review round 2, finding 2).
	// The store load, terminal/transition checks and the TaskWorking upsert
	// all happen inside one WithTasks call so a concurrent cancel/complete
	// can never land between the check and the write. Sessions.Stop is kept
	// OUTSIDE the callback: it shells out to tmux, and nothing that touches a
	// session or process may run while tasksMu is held.
	var (
		stopOrphan  bool
		orphanState TaskState
	)
	err = WithTasks(e.Root, func(tasks *TaskStore) error {
		if cur, ok := tasks.ByContext(task.ContextID); ok && isTerminal(cur.State) {
			// The task reached a terminal state (most likely canceled) while
			// its session was starting: EnsureWorkspace/Start/Inject already
			// succeeded, so a real tmux session is now running that this
			// terminal row does not reference. Leave the terminal row exactly
			// as it is; the actual teardown happens after this call returns.
			stopOrphan = true
			orphanState = cur.State
			return nil
		}
		if cur, ok := tasks.ByContext(task.ContextID); ok && !CanTransition(cur.State, TaskWorking) {
			log.Printf("a2a: session %s is running but task %s is in state %s (not submitted); leaving its state alone", task.Session, task.ContextID, cur.State)
			return nil
		}
		task.State = TaskWorking
		tasks.Upsert(task)
		return nil
	})
	if err != nil {
		// Transient I/O error (on load or on save): the session really is
		// running and we simply couldn't persist TaskWorking. Log and leave
		// the row for the normal lifecycle to reconcile rather than
		// fabricate a failure that contradicts the sandbox's actual state.
		log.Printf("a2a: session %s is running but recording its state failed: %v", task.Session, err)
		return nil
	}
	if stopOrphan {
		// The work in flight is correctly discarded, since the task was
		// already decided elsewhere.
		if serr := e.Sessions.Stop(ctx, task.Session); serr != nil {
			log.Printf("a2a: task %s reached terminal state %s during start; stopping its orphaned session %s failed: %v", task.ContextID, orphanState, task.Session, serr)
		}
	}
	return nil
}
