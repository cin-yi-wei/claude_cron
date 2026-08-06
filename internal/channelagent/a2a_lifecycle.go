package channelagent

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"time"
)

// errNothingSwept signals, from inside a WithTasks callback, that this sweep
// pass changed no task. Returning it makes WithTasks discard the (empty)
// mutation and skip the save, matching the pre-lock behaviour of never
// writing tasks.json when there was nothing to change.
var errNothingSwept = errors.New("a2a: sweep found nothing to change")

// MaxConcurrentSandboxes caps simultaneous aa-*-<ctx> instances. Industry
// guidance for parallel agent worktrees is 8-10; 8 is the conservative end and
// also bounds memory, which has run tight on this host.
const MaxConcurrentSandboxes = 8

// HasCapacity gates on RunningCount, not ActiveCount: a submitted task has no
// sandbox yet, so it must not count against the very capacity it is waiting
// for. Gating on ActiveCount would deadlock permanently once nothing is
// running — a pile of queued work would read as "full" forever, since
// nothing running means nothing can ever complete to free a slot.
func HasCapacity(s TaskStore) bool {
	return s.RunningCount() < MaxConcurrentSandboxes
}

// errNothingToDrain 表示這一趟沒有取得任何派送權，WithTasks 因此不寫檔。
var errNothingToDrain = errors.New("a2a: nothing to drain")

// DispatchStaleAfter 是一列停留在 dispatching 的上限。超過即判為「派送中崩潰」
// （serve 被殺、機器重開），標 failed 把槽釋放出來。90 秒的 tmux 開機窗口加上
// git worktree add 的時間，5 分鐘是寬鬆但仍有界的值。
const DispatchStaleAfter = 5 * time.Minute

// DrainQueue 先在一次 WithTasks 內原子地「只把仍是 submitted 的 row 翻成
// dispatching」以取得派送權，再到鎖外才呼叫 Start。
//
// 為什麼非得原子不可：Start 要到 EnsureWorkspace + Start + Inject 全部成功
// （最長 90 秒）才寫 TaskWorking，而 cycle 每 10 秒跑一次。用未上鎖的
// LoadTasks 判斷「還是 submitted 嗎」，幾乎必然在開機窗口內把同一列再派一
// 次；因為 message id 現在刻意保證唯一，第二則 prompt 會真的送進同一個沙盒，
// 同一段委派工作跑兩遍（可能含 commit / push）。這正是 C2。
//
// 容量在同一個 critical section 內預留：翻成 dispatching 的那一刻起，這一列
// 就計入 RunningCount，下一次並發呼叫（handler 或另一次 DrainQueue）看到的
// 就是扣掉這一列之後的真實空位數 —— 同時修掉 I2。
func DrainQueue(ctx context.Context, root string, ex TaskExecutor) (int, error) {
	var claimed []A2ATask
	err := WithTasks(root, func(tasks *TaskStore) error {
		free := MaxConcurrentSandboxes - tasks.RunningCount()
		now := time.Now().UTC().Format(time.RFC3339)
		for i := range tasks.Tasks {
			if free <= 0 {
				break
			}
			t := tasks.Tasks[i]
			if t.State != TaskSubmitted {
				continue
			}
			t.State = TaskDispatching
			t.DispatchedAt = now
			tasks.Tasks[i] = t
			claimed = append(claimed, t)
			free--
		}
		if len(claimed) == 0 {
			return errNothingToDrain
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothingToDrain) {
		return 0, err
	}

	started := 0
	for _, t := range claimed {
		if err := ex.Start(ctx, t, t.Prompt); err != nil {
			continue // executor 已經把失敗記在 row 上了
		}
		started++
	}
	return started, nil
}

const (
	// SoftTimeout only flips reporting: A2A natively supports long-running
	// tasks, and real agent work routinely exceeds half an hour.
	SoftTimeout = 30 * time.Minute
	// HardTimeout is the backstop against a wedged sandbox holding a worktree
	// and memory forever.
	HardTimeout = 2 * time.Hour
	// RetainAfterComplete keeps the sandbox alive briefly so the caller can ask
	// a follow-up in the same contextId without paying to rebuild it.
	RetainAfterComplete = 10 * time.Minute
)

// MaxRetainedFailedSandboxes bounds the forensics rule. Keeping every failed
// sandbox forever is itself an unbounded disk-growth path; the newest are the
// ones worth inspecting.
const MaxRetainedFailedSandboxes = 20

func parseRFC3339(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// reclaimCandidate is one task this sweep pass considered tearing down: its
// worktree (if any) and sandbox root, identified but NOT yet cleared from the
// task row. Fields are only cleared after the actual removal below succeeds
// (see SweepTimeouts doc comment) — a candidate whose removal fails is simply
// left out of the next WithTasks write, so its Worktree/Session survive on
// disk for a later sweep to retry. projectDir may be empty when the owning
// agent can no longer be resolved (deleted since the task ran); RemoveWorktree
// still falls back to deleting the worktree directory directly in that case.
//
// taskID and state are the row's OWN values at the moment step 1 selected it,
// carried forward so step 3 can prove the row it is about to clear is still
// the same task in the same state — not a fresh task that reused the same
// contextId while teardown was in flight (task-8 review round 3, finding 1).
// contextId ownership is enforced on CallerID only, not on task state, and
// SessionNameFor/SandboxWorktree are deterministic functions of the
// contextId, so a resubmission during the teardown window lands a live
// Session/Worktree at the exact paths this candidate is tearing down, under
// the SAME contextId (Upsert keys on contextId, so the resubmission
// overwrites this row in place). Matching on contextId alone would then let
// step 3 zero out that brand-new task's live fields — the exact
// unrecoverable-orphan class finding 1 exists to close, reintroduced through
// the bookkeeping step meant to fix it.
type reclaimCandidate struct {
	taskID, contextID, projectDir, worktree, session string
	state                                            TaskState
}

// SweepTimeouts cancels tasks past HardTimeout and tears down completed and
// canceled sandboxes, reclaiming their worktree and sandbox root — not just
// the tmux session. A canceled task is not a failed one, so the forensics
// exemption does not apply to it: HardTimeout exists precisely to bound a
// wedged sandbox's disk footprint, and that bound would be meaningless if the
// worktree it was holding were left behind after cancellation. Failed
// sandboxes alone are deliberately kept for forensics, but that exemption is
// itself capped at MaxRetainedFailedSandboxes: the oldest failures beyond the
// cap are reclaimed the same way, keeping only the newest (most likely to
// still be worth inspecting).
//
// Reclamation is retry-safe: a task's Worktree/Session are only cleared AFTER
// its worktree and sandbox root have actually been removed from disk, never
// before. This runs in two passes over the tasks-file lock, with the slow
// tmux/git/filesystem work happening in between, outside the lock (WithTasks'
// mutex is non-reentrant, so nesting a second WithTasks call inside the first
// would deadlock):
//
//  1. Identify candidates (state transitions for hard-timeout cancels are
//     applied and persisted here, but no Worktree/Session field is cleared
//     yet for anything this pass wants to reclaim), recording each
//     candidate's own TaskID/State/Worktree/Session as they stood at
//     selection time.
//  2. Outside the lock: stop sessions, then attempt each candidate's
//     removal. A failure is logged and left for a later sweep to retry — the
//     candidate stays out of the "succeeded" list below.
//  3. Take the lock again and, for each succeeded candidate, clear
//     Worktree/Session ONLY if the row at that contextId still has the
//     exact same TaskID, State, Worktree and Session recorded in step 1. A
//     contextId is caller-chosen and its ownership check does not pin task
//     state, so the same caller can legally resubmit the same contextId
//     while step 2 is tearing down a terminal task under it; TaskStore.Upsert
//     keys on contextId, so that resubmission overwrites the row in place
//     with a fresh TaskID and a live Session/Worktree at the same
//     deterministic paths. Matching on contextId alone would let this step
//     zero out that new, live task's fields. A row that no longer matches is
//     left completely alone — it keeps its fields, so the retry path in
//     step 2 covers it again on the next sweep if it's still genuinely
//     reclaim-eligible.
//
// canceled counts state transitions from step 1 (unaffected by removal
// success, since no path information is lost by that transition alone).
// reclaimed counts candidates that were both successfully removed in step 2
// AND confirmed still the same task in step 3 — a candidate whose row
// changed identity underneath it is not counted, since nothing was actually
// cleared for it. reclaimed is independent of whether step 3's SaveTasks
// call itself succeeds; if it doesn't, the next sweep simply retries an
// already-removed path, which os.RemoveAll and RemoveWorktree both tolerate.
func SweepTimeouts(ctx context.Context, root string, sm SessionManager, now time.Time) (int, int, error) {
	canceled, reclaimed := 0, 0
	// Sessions to stop are collected under the lock but stopped after it is
	// released: sm.Stop shells out to tmux, and nothing that touches a
	// session or process may run while tasksMu is held (the same rule the
	// executor's dispatch follows for the same reason).
	var toStop []string
	var candidates []reclaimCandidate

	// A task only records its owning agent's name, not its ProjectDir, so
	// resolve it here the same way SandboxExecutor.Start does. A load failure
	// must not block cancellation/reclaim bookkeeping — that is the safety-
	// critical half of this sweep — so fall back to an empty AgentStore and
	// let projectDirFor return "" per task; RemoveWorktree's directory-delete
	// fallback still tears down the worktree even without it.
	agents, aerr := LoadAgents(root)
	if aerr != nil {
		log.Printf("a2a: sweep: load agents: %v (worktree removal will use an empty project dir)", aerr)
		agents = AgentStore{}
	}
	projectDirFor := func(agentName string) string {
		if a, ok := agents.Get(agentName); ok {
			return a.ProjectDir
		}
		return ""
	}

	// --- Step 1: transitions + candidate identification, under the lock. ---
	err := WithTasks(root, func(tasks *TaskStore) error {
		changed := false
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			switch t.State {
			case TaskWorking, TaskSubmitted, TaskDispatching:
				if !CanTransition(t.State, TaskCanceled) {
					continue
				}

				// 派送中崩潰:dispatching 只受 DispatchStaleAfter 管,絕不落進
				// 下面 StartedAt/HardTimeout 那條路 —— StartedAt 是最初提交
				// (排隊)的時間,一個排隊很久才被 DrainQueue 撿走的任務,從
				// 撿走那一刻才開始算「正在開機」;拿排隊等了多久去跟
				// HardTimeout 比,只會把剛認領、還在合法 90 秒開機窗口內的
				// 任務錯殺(review round 2, minor 1)。標 failed 不是
				// TaskCanceled:這不是操作者取消,而是我們自己的執行體死掉
				// 了,失敗歸類為 failed 才對得上其他派送失敗的分類。
				if t.State == TaskDispatching {
					d, dok := parseRFC3339(t.DispatchedAt)
					if !dok || now.Sub(d) >= DispatchStaleAfter {
						toStop = append(toStop, t.Session)
						t.State = TaskFailed
						detail := "dispatch stalled (no sandbox came up)"
						// 保留卡住前就已經留在 Detail 裡的線索,跟下面
						// TaskCanceled 那條路一樣的理由(task 4):不能讓
						// sweep 自己的 reason 蓋掉它(review round 2, minor
						// 2)。
						if t.Detail != "" {
							detail = t.Detail + "; " + detail
						}
						t.Detail = detail
						t.CompletedAt = now.UTC().Format(time.RFC3339)
						tasks.Tasks[i] = t
						changed = true
					}
					continue
				}

				// A missing or corrupt StartedAt must NOT pin this task's
				// sandbox forever: HardTimeout exists precisely as the backstop
				// against a wedged sandbox, and a task with unreadable state is
				// among the most likely to be wedged. Treat it as sweep-eligible
				// immediately (we cannot compute elapsed time), but record and
				// log a distinct reason so an operator can tell a corrupt-data
				// cancel apart from an ordinary hard timeout.
				var reason string
				started, ok := parseRFC3339(t.StartedAt)
				switch {
				case !ok:
					reason = "start time unreadable (missing or corrupt StartedAt); canceled as a hard-timeout backstop"
					log.Printf("a2a: sweep: task %s (session %s) has an unparseable StartedAt %q; canceling rather than leaving it unreclaimable forever", t.ContextID, t.Session, t.StartedAt)
				case now.Sub(started) >= HardTimeout:
					reason = "hard timeout exceeded"
				default:
					continue
				}
				toStop = append(toStop, t.Session)
				t.State = TaskCanceled
				// 保留卡住前就已經留在 Detail 裡的線索(最常見的是 task 4 的
				// TrustFolder 失敗警告):如果只用 sweep 自己的 reason 蓋掉它,
				// 「信任失敗 → 沙盒卡在對話框 → 兩小時後被 sweep 收掉」這整條
				// 因果就會在最後一步斷掉,使用者看到的只剩「hard timeout
				// exceeded」,跟這個 task 原本要修的「什麼原因都看不到」一樣。
				if t.Detail != "" {
					reason = t.Detail + "; " + reason
				}
				t.Detail = reason
				t.CompletedAt = now.UTC().Format(time.RFC3339)
				tasks.Tasks[i] = t
				canceled++
				changed = true
			case TaskCompleted:
				// Same reasoning on the retention path: an unparseable
				// CompletedAt must make the task reclaim-eligible rather than
				// pinning its sandbox forever. TaskFailed is a separate switch
				// case (or rather, no case at all below) and is therefore never
				// touched here — the forensics exemption is untouched by this.
				done, ok := parseRFC3339(t.CompletedAt)
				if ok && now.Sub(done) < RetainAfterComplete {
					continue
				}
				if !ok {
					log.Printf("a2a: sweep: task %s (session %s) has an unparseable CompletedAt %q; reclaiming its sandbox rather than pinning it forever", t.ContextID, t.Session, t.CompletedAt)
				}
				if t.Session == "" && t.Worktree == "" {
					continue // already fully reclaimed by an earlier sweep
				}
				if t.Session != "" {
					toStop = append(toStop, t.Session)
				}
				candidates = append(candidates, reclaimCandidate{t.TaskID, t.ContextID, projectDirFor(t.Agent), t.Worktree, t.Session, t.State})
			}
		}

		// Canceled tasks are not failed: the forensics exemption does not
		// apply to them, so any task sitting in TaskCanceled that still holds
		// a worktree or session is reclaim-eligible — including one just
		// transitioned above this same pass (tasks.Tasks already reflects
		// that mutation here), one canceled by an earlier sweep, and one
		// whose earlier removal attempt failed and is being retried. Its
		// session was already stopped at the moment it was canceled (either
		// just above, or by a previous sweep pass), so this does not
		// re-append to toStop.
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			if t.State != TaskCanceled {
				continue
			}
			if t.Session == "" && t.Worktree == "" {
				continue // already fully reclaimed
			}
			candidates = append(candidates, reclaimCandidate{t.TaskID, t.ContextID, projectDirFor(t.Agent), t.Worktree, t.Session, t.State})
		}

		// Cap the forensics exemption: failed sandboxes are kept on purpose,
		// but not without bound. Only consider ones that still hold a
		// worktree — a task already trimmed by a prior sweep pass has
		// Worktree == "" and must not be re-counted or re-removed.
		type failedCandidate struct {
			idx  int
			done time.Time // zero value (unparseable/missing) sorts as oldest
		}
		var failed []failedCandidate
		for i, t := range tasks.Tasks {
			if t.State != TaskFailed || t.Worktree == "" {
				continue
			}
			done, _ := parseRFC3339(t.CompletedAt)
			failed = append(failed, failedCandidate{i, done})
		}
		if len(failed) > MaxRetainedFailedSandboxes {
			sort.Slice(failed, func(a, b int) bool { return failed[a].done.Before(failed[b].done) })
			trim := len(failed) - MaxRetainedFailedSandboxes
			for _, fc := range failed[:trim] {
				t := tasks.Tasks[fc.idx]
				if t.Session != "" {
					toStop = append(toStop, t.Session)
				}
				candidates = append(candidates, reclaimCandidate{t.TaskID, t.ContextID, projectDirFor(t.Agent), t.Worktree, t.Session, t.State})
			}
		}

		if !changed {
			return errNothingSwept
		}
		return nil
	})

	// Don't destroy anything on a failed persist: if step 1's SaveTasks
	// failed, tasks.json still shows the pre-sweep state, so tearing down
	// sessions or disk now would desync reality from the file that is
	// supposed to track it. errNothingSwept means step 1 made no mutation
	// (state transitions only — the reclaim candidates above are never
	// mutated yet, so they're unaffected either way) and is treated as
	// success.
	if err != nil && !errors.Is(err, errNothingSwept) {
		return canceled, 0, err
	}

	// --- Step 2: outside the lock, stop sessions and attempt removal. ---
	for _, session := range toStop {
		_ = sm.Stop(ctx, session)
	}
	var succeeded []reclaimCandidate
	for _, c := range candidates {
		ok := true
		if c.worktree != "" {
			if rmErr := sm.RemoveWorkspace(ctx, c.projectDir, c.worktree); rmErr != nil {
				log.Printf("a2a: sweep: failed to remove worktree %s for context %s (left in place, will retry next sweep): %v", c.worktree, c.contextID, rmErr)
				ok = false
			}
		}
		if c.session != "" {
			if rmErr := os.RemoveAll(SandboxRoot(root, c.session)); rmErr != nil {
				log.Printf("a2a: sweep: failed to remove sandbox root for context %s (left in place, will retry next sweep): %v", c.contextID, rmErr)
				ok = false
			}
			// 政策檔與沙盒同生共死。清不掉只 log,不影響回收判定 —— 下一趟
			// sweep 會重試,而一份指向已不存在 worktree 的政策檔本身無害
			// (gate 只在該 session 名的 hook 行程裡才會讀到它)。
			if rmErr := RemoveSandboxPolicy(root, c.session); rmErr != nil {
				log.Printf("a2a: sweep: 刪除 %s 的政策檔失敗(下一趟重試): %v", c.session, rmErr)
			}
		}
		if ok {
			succeeded = append(succeeded, c)
		}
	}

	// --- Step 3: take the lock again, clear fields only for confirmed matches. ---
	//
	// contextId ownership is enforced on CallerID only, not on task state
	// (the deliberate I2 design), and SessionNameFor/SandboxWorktree are
	// deterministic functions of the contextId. So between step 1 releasing
	// the lock and this reacquiring it, the same caller can legally resubmit
	// the same contextId: TaskStore.Upsert keys on contextId, so a fresh
	// task with a new TaskID, live Session/Worktree, and TaskSubmitted state
	// overwrites this exact row in place. Matching on contextId alone here
	// would zero out that live task's fields — corrupting a task that step 1
	// never selected and turning it into the very unrecoverable orphan this
	// whole retry design exists to prevent. Clearing therefore requires the
	// row to still be an exact match — same TaskID, same State, same
	// Worktree, same Session as step 1 recorded — not merely the same
	// contextId. A row that no longer matches is left completely alone: it
	// keeps whatever fields it currently has, and if it is genuinely still
	// the same reclaim candidate (e.g. a transient mismatch that wasn't
	// actually a race), the next sweep picks it up again.
	if len(succeeded) > 0 {
		err2 := WithTasks(root, func(tasks *TaskStore) error {
			changed := false
			for _, c := range succeeded {
				for i := range tasks.Tasks {
					t := tasks.Tasks[i]
					if t.ContextID != c.contextID {
						continue
					}
					if t.TaskID != c.taskID || t.State != c.state || t.Worktree != c.worktree || t.Session != c.session {
						log.Printf("a2a: sweep: context %s changed during teardown (now task %s, state %s); leaving its worktree/session untouched", c.contextID, t.TaskID, t.State)
						break
					}
					t.Worktree = ""
					t.Session = ""
					tasks.Tasks[i] = t
					reclaimed++
					changed = true
					break
				}
			}
			if !changed {
				return errNothingSwept
			}
			return nil
		})
		if err2 != nil && !errors.Is(err2, errNothingSwept) {
			// The removals already happened on disk; only the bookkeeping
			// that marks them cleared failed to persist. Log it — the next
			// sweep will harmlessly retry an already-gone path — but don't
			// let it override step 1's result, which is what actually
			// happened to cancellation/reclaim eligibility this pass. Note
			// reclaimed was already incremented above per matched row inside
			// the (failed-to-save) callback; that overstates the persisted
			// count in this rare case, same as the pre-existing tradeoff
			// documented in the doc comment above.
			log.Printf("a2a: sweep: failed to persist reclaimed sandboxes: %v", err2)
		}
	}

	return canceled, reclaimed, nil
}
