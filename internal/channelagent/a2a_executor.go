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
//
// Prompt is NOT this call's to overwrite (task 7 review finding,
// a2a_executor.go:156): task is Start's OWN claim-time copy, taken back when
// DrainQueue (or the handler) first claimed this dispatch. A follow-up
// (a2a_server.go's message/send follow-up path) can merge a newer Prompt
// onto this exact row while Start is still between that claim and this call
// — Start holds the session lock across its whole build, but the follow-up
// path never needs it (it only touches the row's Prompt field, under
// tasksMu, before this call ever runs). Blindly Upserting task here would
// silently regress Prompt back to the stale claim-time value, discarding
// what the follow-up just recorded — exactly the same class of bug the final
// locked block in Start (which does `task.Prompt = cur.Prompt`) exists to
// avoid, just one write earlier. The store's current Prompt is always the
// latest one actually observed; keep it whenever a row already exists.
func (e *SandboxExecutor) persist(task A2ATask) error {
	return WithTasks(e.Root, func(tasks *TaskStore) error {
		if cur, ok := tasks.ByContext(task.ContextID); ok {
			if isTerminal(cur.State) {
				return errTaskAlreadyTerminal
			}
			task.Prompt = cur.Prompt
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
// safe 標記 detail 是否可以原文回給 tasks/get 的遠端呼叫方——見
// A2ATask.DetailSafe 的說明。呼叫方必須在每個呼叫點明確決定，不給預設值，
// 逼著新增的失敗分支也要做這個判斷，而不是靜默繼承上一個呼叫點的答案。
func (e *SandboxExecutor) markFailed(task A2ATask, detail string, safe bool) {
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
		t.DetailSafe = safe
		t.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		tasks.Upsert(t)
		return nil
	})
}

func (e *SandboxExecutor) Start(ctx context.Context, task A2ATask, prompt string) error {
	// 整段建立過程持有 session 鎖，於是 sweep 不可能在中途把同名 session 的
	// worktree / sandbox root 拆掉（task 7, D3）。鎖序：session 鎖 →
	// tasksMu（下面的 persist / WithTasks），全程不得反向 —— WithTasks 的
	// callback 內永遠不得取得 session 鎖。
	unlock := lockSandboxSession(task.Session)
	defer unlock()

	agents, err := LoadAgents(e.Root)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	agent, ok := agents.Get(task.Agent)
	if !ok {
		// safe=true：這三個分支的字串只由固定格式 + 呼叫方自己送出的
		// agent/contextId 組成,沒有 err.Error() 包住的任何 host 端錯誤——
		// 呼叫方回顯自己已經知道的輸入,不構成新的資訊洩漏。
		err := fmt.Errorf("unknown agent %q", task.Agent)
		e.markFailed(task, err.Error(), true)
		return err
	}
	if !agent.Enabled {
		err := fmt.Errorf("agent %q is disabled", task.Agent)
		e.markFailed(task, err.Error(), true)
		return err
	}

	// 沒有有效等級的 row 不可以起沙盒:政策檔會寫不出可用的 Level,gate 會
	// 全面拒絕,結果是一個活著卻什麼都做不了、還佔著併發額度的殭屍。
	if !ValidGrantLevel(task.Level) {
		err := fmt.Errorf("task %s has no valid grant level", task.ContextID)
		e.markFailed(task, err.Error(), true)
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
		// safe=false 由此往下都是這個方向:這些分支的 err 來自 tasks.json
		// I/O、檔案系統、git、政策檔、tmux、inject——都可能夾帶絕對路徑或
		// 其他 host 端細節,絕不可原文交給遠端呼叫方(見 handleMessageSend
		// 對這一類 err.Error() 的既有處理與 taskSnapshotPayload 的說明)。
		e.markFailed(task, "persist sandbox identity: "+err.Error(), false)
		return err
	}

	if err := Init(sandboxRoot); err != nil {
		e.markFailed(task, "init sandbox root: "+err.Error(), false)
		return err
	}
	if err := e.Sessions.EnsureWorkspace(ctx, agent.ProjectDir, task.Branch, task.Worktree); err != nil {
		e.markFailed(task, "ensure worktree: "+err.Error(), false)
		return err
	}

	// 失敗只 log 不中止:它只是省一個對話框,不是必要條件(driver 的第 3 層
	// backstop 仍在)。讓一個 ~/.claude.json 的暫時性讀寫錯誤害死每一個委派
	// 任務,遠比多跳一次對話框糟糕。但也不能只寫進 journal:task 4 要修的就是
	// 「使用者除了乾等兩小時什麼都看不到」這個症狀,journal 使用者看不到,
	// 所以把原因記進 task row 的 Detail —— 下面的 WithTasks 區塊把 task
	// upsert 成 TaskWorking 時會一併帶上這個值,讓卡住的任務至少在狀態查詢
	// 上留下線索,而不是靜默地跟原本的 bug 一樣看起來什麼都沒發生。
	if err := e.Sessions.TrustFolder(ctx, task.Worktree); err != nil {
		// safe=false：%v 包住的 err 來自 ~/.claude.json 讀寫，可能夾帶路徑。
		task.Detail = fmt.Sprintf("預先信任 worktree 失敗,沙盒仍會啟動但可能卡在資料夾信任對話框: %v", err)
		task.DetailSafe = false
		log.Printf("a2a: 預先信任 %s 失敗(沙盒仍會啟動,靠 driver 的畫面 backstop): %v", task.Worktree, err)
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
		e.markFailed(task, "write sandbox policy: "+err.Error(), false)
		return err
	}

	if err := e.Sessions.Start(ctx, task.Session, task.Worktree, sandboxRoot); err != nil {
		e.markFailed(task, "start session: "+err.Error(), false)
		return err
	}

	// Unique per message, not per context: IngestMessages dedups on
	// platform:channel:messageID, so a constant ID would silently drop
	// every follow-up in the same contextId.
	msgID := nextInjectedMessageID(task.Session)
	msg := SourceMessage{
		Platform:  "a2a",
		ChannelID: task.ContextID,
		MessageID: msgID,
		AuthorID:  task.CallerID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Content:   prompt,
	}
	if err := e.Sessions.Inject(ctx, sandboxRoot, msg); err != nil {
		e.markFailed(task, "inject: "+err.Error(), false)
		return err
	}
	// 記在 row 上，CollectResults 才有東西可以比對來源。
	task.LastMessageID = msgID

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
		cur, ok := tasks.ByContext(task.ContextID)
		if ok && isTerminal(cur.State) {
			// The task reached a terminal state (most likely canceled) while
			// its session was starting: EnsureWorkspace/Start/Inject already
			// succeeded, so a real tmux session is now running that this
			// terminal row does not reference. Leave the terminal row exactly
			// as it is; the actual teardown happens after this call returns.
			stopOrphan = true
			orphanState = cur.State
			return nil
		}
		if ok && !CanTransition(cur.State, TaskWorking) {
			log.Printf("a2a: session %s is running but task %s is in state %s (not submitted); leaving its state alone", task.Session, task.ContextID, cur.State)
			// round 10 review, Minor（D10-4）：Inject 已經真的把訊息送進沙盒
			// 了——即使這一列現在的狀態不允許我們把它轉成 working，這個
			// MessageID 仍然是「這個沙盒最後收到的訊息」，CollectResults 之
			// 後得靠它才能比對到接下來的回覆。只更新 LastMessageID，其餘欄
			// 位（尤其是 State）完全不動——那屬於別的路徑（不是這次 Start
			// 呼叫）的決定權。
			cur.LastMessageID = task.LastMessageID
			tasks.Upsert(cur)
			return nil
		}
		task.State = TaskWorking
		if ok {
			// task 6 review round 2: task is Start's OWN parameter — a copy
			// taken when this dispatch was first claimed, carrying whatever
			// Prompt existed back then. A follow-up (a2a_server.go's
			// message/send follow-up path) can land on this very row while
			// this dispatch is still booting and update Prompt to record the
			// caller's latest text. Blindly Upserting task here would
			// silently regress Prompt back to that stale claim-time value,
			// discarding what the follow-up just recorded. The store's
			// current Prompt is always the latest one actually observed;
			// keep it.
			task.Prompt = cur.Prompt
		}
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

// errFollowUpTargetGone signals that, by the time DeliverFollowUp actually
// tried to deliver, the row it was handed no longer matches a live
// dispatch — see the staleness re-check below.
var errFollowUpTargetGone = errors.New("a2a: follow-up target is no longer a live dispatch")

// DeliverFollowUp injects prompt directly into an already-dispatched task's
// sandbox inbox — no EnsureWorkspace, no TrustFolder, no policy rewrite, no
// Sessions.Start. handleRPC calls this ONLY for a task it found already in
// TaskDispatching or TaskWorking (a genuine follow-up, never a fresh
// dispatch): the sandbox already exists (working) or is already coming up
// (dispatching) from a dispatch some other call already owns, so repeating
// any of Start's setup steps here would race that in-flight dispatch on the
// same worktree/session — exactly the double EnsureWorkspace/Sessions.Start
// race task 6 review round 2 found. This never touches task.State: that
// belongs entirely to whichever call is actually running the dispatch this
// message is following up on.
//
// task carries the row as it stood when handleRPC read it, under tasksMu,
// BEFORE releasing that lock and calling this method outside it (Inject
// touches the filesystem and must never run while tasksMu is held). Between
// that read and the Inject call below, SweepTimeouts can run to completion
// for this exact session: a task sitting in TaskWorking past HardTimeout is
// canceled and reclaimed in the very same sweep pass (a2a_lifecycle.go), so
// task's snapshot can go stale without ever being terminal at read time. If
// it does, e.Sessions.Inject's underlying IngestMessages calls Init(root)
// unconditionally — that would recreate sandboxes/<session>/ right after
// sweep just removed it, leaving a directory holding a pending job that
// nothing will ever reclaim (task 7 review finding, a2a_server.go:389). The
// lock below is what makes the re-check race-free rather than merely
// reducing the odds of losing: sweep's own removal (a2a_lifecycle.go) takes
// the same session lock before its destructive phase, so either this
// re-check runs first and wins the lock for the whole Inject call (sweep
// then blocks until it's done, so nothing can remove out from under it), or
// sweep's removal already ran and this re-check observes the row's new,
// non-live state and refuses to deliver. The message itself is then simply
// dropped (logged by the caller) — accepting that loss is explicitly out of
// this task's scope; only the directory resurrection is.
func (e *SandboxExecutor) DeliverFollowUp(ctx context.Context, task A2ATask, prompt string) error {
	unlock := lockSandboxSession(task.Session)
	defer unlock()

	live := false
	_ = WithTasks(e.Root, func(tasks *TaskStore) error {
		if cur, ok := tasks.ByContext(task.ContextID); ok {
			live = cur.TaskID == task.TaskID && cur.Session == task.Session &&
				(cur.State == TaskDispatching || cur.State == TaskWorking)
		}
		return errNothingSwept // 只讀不寫，讓 WithTasks 跳過存檔
	})
	if !live {
		return fmt.Errorf("%w: contextId %s", errFollowUpTargetGone, task.ContextID)
	}

	sandboxRoot := SandboxRoot(e.Root, task.Session)
	msgID := nextInjectedMessageID(task.Session)
	msg := SourceMessage{
		Platform:  "a2a",
		ChannelID: task.ContextID,
		MessageID: msgID,
		AuthorID:  task.CallerID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Content:   prompt,
	}
	if err := e.Sessions.Inject(ctx, sandboxRoot, msg); err != nil {
		return err
	}

	// 跟 Start 同一個道理：CollectResults 用 LastMessageID 比對結果檔的來源，
	// 沒有這一筆，這則追問的回覆永遠對不上、卡死在 working（buildJobID 把
	// 每一則被處理的訊息自己的 MessageID 刻進 job_id，追問跟最初派送用的是
	// 兩個不同的 MessageID）。這裡多一個窗口：session 鎖只擋得住拆除，擋不
	// 住 hard-timeout 這種純記憶體、不必先拿 session 鎖的狀態轉移，所以落地
	// 前必須重新核對身分（TaskID + Session）仍然一致 —— 不一致就放棄，寧可
	// 讓這筆追問的結果永遠沒有機會被撿到，也不能把 LastMessageID 蓋到一個已
	// 經不是它的 row 上。
	_ = WithTasks(e.Root, func(tasks *TaskStore) error {
		cur, ok := tasks.ByContext(task.ContextID)
		if !ok || cur.TaskID != task.TaskID || cur.Session != task.Session {
			return errFollowUpIdentityChanged
		}
		cur.LastMessageID = msgID
		tasks.Upsert(cur)
		return nil
	})
	return nil
}

// errFollowUpIdentityChanged signals, from inside a WithTasks callback, that
// the row no longer matches the identity DeliverFollowUp started with — the
// re-check right before recording LastMessageID. Returning it makes WithTasks
// discard the write rather than stamping a stale MessageID onto a row that a
// resubmission or a hard-timeout cancel already moved on from while the
// Inject call above was in flight (outside any lock).
var errFollowUpIdentityChanged = errors.New("a2a: follow-up target changed identity before LastMessageID could be recorded")
