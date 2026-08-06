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
//
// Stopping this candidate's session (and driver) is NOT conditional on how
// it got here (task 7 review round 3, critical finding): step 2 always
// attempts sm.Stop/stopper.Stop for a non-empty session, on every single
// pass that reaches this candidate, whether or not an earlier pass already
// canceled or attempted it. A per-pass flag recording "does THIS pass need
// to stop it" (this type used to carry one, derived from a map of rows
// canceled in the CURRENT pass only) cannot survive a pass where the
// teardown lock was busy: the candidate is skipped entirely that pass, and
// on the NEXT pass the row is no longer "freshly canceled", so a
// per-pass-derived flag would read false and let removeCandidate delete the
// worktree/sandbox root while the tmux session (and its driver) keep
// running untouched — a live pane with a deleted cwd, referenced by no row,
// findable by no future sweep. sm.Stop/stopper.Stop on an already-stopped
// or never-started session is a harmless no-op, so re-attempting it every
// pass has no downside and closes this hole completely.
type reclaimCandidate struct {
	taskID, contextID, projectDir, worktree, session string
	state                                            TaskState
}

// stopTarget is a session this sweep pass wants to stop but does NOT want
// to remove from disk — currently only the dispatch-stalled crash-recovery
// path (a row stuck in TaskDispatching past DispatchStaleAfter, transitioned
// to TaskFailed): its worktree/sandbox root are left alone for the
// forensics exemption, same as any other TaskFailed row, but a lingering
// tmux session from the crashed dispatch attempt should still be cleaned
// up. It never becomes a reclaimCandidate, so without its own identity
// snapshot it would have no guard at all against the same resubmission race
// candidates are guarded against (task 7 review round 2, critical finding).
//
// Appended once, at the exact instant of the TaskDispatching→TaskFailed
// transition — deliberately NOT re-derived by scanning every TaskFailed row
// on later passes (see the "why one-shot" note at its one call site in
// SweepTimeouts's step 1). Its Session is also never cleared afterward, even
// on a successful stop: clearing it alone (keeping Worktree) was tried and
// reverted (task 7 review round 3, important finding) — it left a row with
// Worktree != "" and Session == "", which is exactly the identity-less state
// the reclaimCandidate loop below refuses to touch. A row can legitimately
// be both a stopTarget (this pass) and a reclaimCandidate later (the
// failed-sandbox-cap trim, if it also holds a Worktree) — that overlap means
// its session can be stopped twice across the two loops in the same pass;
// harmless (sm.Stop/stopper.Stop no-op on an already-stopped session), but
// step 3 has no write-back for stopTarget at all, so there is no spurious
// "changed before its session-stop could be recorded" log from this
// overlap (task 7 review round 3, minor finding).
type stopTarget struct {
	taskID, contextID, session string
	state                      TaskState
}

// SandboxStopper 只有一個方法，讓 sweep 可以在動手之前先停掉還活著的 driver
// 而不必認識整個 SandboxDriver。nil 代表不停（測試用）。
// SandboxDriver.Stop 阻塞到 goroutine 真的結束，那正是回收需要的保證 ——
// cycle 的順序是 collect → sweep → drain → EnsureSandboxDrivers，所以 sweep
// 動手時 driver 還活著；它下一輪 RunWorkerOnce 的第一件事就是 Init(root)，
// 會把剛刪掉的目錄樹重建回來（task 7, D4）。
type SandboxStopper interface {
	Stop(session string)
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
//  2. Outside the lock: for each session this pass wants to touch — every
//     stopTarget and every reclaimCandidate with a non-empty session — try
//     to take that session's exclusive teardown lock
//     (tryLockSandboxSessionForTeardown, D3(a); non-blocking: it only
//     succeeds if no in-flight Start/DeliverFollowUp currently holds the
//     shared lock, and sweep must never block waiting for one to finish —
//     see that function's doc comment). Failing to get it is treated
//     exactly like a failed identity check: skip this session entirely this
//     pass, touching nothing, and let a later sweep retry it. Only once the
//     lock is held does it re-confirm under it that the row hasn't changed
//     identity since step 1 (candidateStillMatches / a stopTarget's own
//     check, D3(b)) — and ONLY THEN, still under the same lock, does it stop
//     the driver (stopper, D4), stop the tmux session, and (for a
//     reclaimCandidate) remove the worktree and sandbox root. Every
//     destructive action for a given session — stop or remove — happens
//     inside this same lock-then-verify block; none of them ever runs
//     before or outside it (task 7 review round 2, critical finding: a
//     tmux stop issued outside this guard can kill a brand-new session a
//     resubmission just created at the same deterministic name). The stop
//     is unconditional on every attempt that reaches it — never gated by
//     whether an EARLIER pass might already have stopped it (task 7 review
//     round 3, critical finding: a flag derived fresh each pass cannot
//     survive a pass where the lock was busy, so a later pass would delete
//     the worktree/sandbox root having never actually stopped the live
//     session; sm.Stop/stopper.Stop on an already-stopped session is a
//     no-op, so repeating it costs nothing). A reclaimCandidate with no
//     session name at all is never touched by this step — there is no
//     lock to take on an empty name, so there is no safe way to re-verify
//     it right before acting (task 7 review round 3, important finding);
//     it is logged and left for manual inspection rather than risked. A
//     removal failure is logged and left for a later sweep to retry — the
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
func SweepTimeouts(ctx context.Context, root string, sm SessionManager, now time.Time, stopper SandboxStopper) (int, int, error) {
	canceled, reclaimed := 0, 0
	// Sessions to stop are collected under the lock but stopped after it is
	// released: sm.Stop shells out to tmux, and nothing that touches a
	// session or process may run while tasksMu is held (the same rule the
	// executor's dispatch follows for the same reason).
	var stopOnly []stopTarget
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
						// 這一列不會變成 reclaimCandidate(worktree/sandbox
						// root 留給鑑識——跟任何其他 TaskFailed row 一樣,
						// forensics 政策不因為它剛好是這條路徑產生的就有
						// 差別),只需要收掉可能還殘留的 tmux session,但一
						// 樣要記下這一刻的身分,好讓 stopTarget 在真正動手
						// 前重新確認(task 7 review round 2, critical
						// finding:同一個 session 名的合法追問可能就在這個
						// 窗口內把它重新建成活的)。這是一次性判定,只在轉
						// 換的這一刻記錄——不像 reclaimCandidate 每輪重新
						// 掃描:一般的 TaskFailed row 的 session 一旦被判定
						// 要留給鑑識,就不該在之後哪一輪又被重新盯上並收掉
						// (那正是下面 forensics 測試釘住的行為)。若這一刻
						// 剛好 session 忙碌(鎖拿不到),這次就不停——極窄的
						// 邊界情況,且會自我化解:忙碌代表另一個合法呼叫正
						// 在使用它,那個呼叫完成後 row 的身分本來就會變(不
						// 再是這個 stale 的 dispatching row 了),沒有東西
						// 真正被漏下。
						if t.Session != "" {
							stopOnly = append(stopOnly, stopTarget{taskID: t.TaskID, contextID: t.ContextID, session: t.Session, state: TaskFailed})
						}
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
				candidates = append(candidates, reclaimCandidate{
					taskID: t.TaskID, contextID: t.ContextID, projectDir: projectDirFor(t.Agent),
					worktree: t.Worktree, session: t.Session, state: t.State,
				})
			}
		}

		// Canceled tasks are not failed: the forensics exemption does not
		// apply to them, so any task sitting in TaskCanceled that still holds
		// a worktree or session is reclaim-eligible — including one just
		// transitioned above this same pass (tasks.Tasks already reflects
		// that mutation here), one canceled by an earlier sweep, and one
		// whose earlier removal attempt failed and is being retried. Step 2
		// always attempts to stop a non-empty session before removing it,
		// regardless of whether THIS pass or an earlier one did the
		// canceling — see reclaimCandidate's doc comment for why a
		// per-pass-derived "already stopped" flag is unsafe here.
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			if t.State != TaskCanceled {
				continue
			}
			if t.Session == "" && t.Worktree == "" {
				continue // already fully reclaimed
			}
			candidates = append(candidates, reclaimCandidate{
				taskID: t.TaskID, contextID: t.ContextID, projectDir: projectDirFor(t.Agent),
				worktree: t.Worktree, session: t.Session, state: t.State,
			})
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
				candidates = append(candidates, reclaimCandidate{
					taskID: t.TaskID, contextID: t.ContextID, projectDir: projectDirFor(t.Agent),
					worktree: t.Worktree, session: t.Session, state: t.State,
				})
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

	// --- Step 2: 鎖外。每個 session 一把鎖，鎖內才准動手，動不了就放棄。 ---
	//
	// stopOnly 的 session（TaskFailed 殘留、只停不拆）先處理。刻意不追蹤
	// 「哪些成功了」、也不對 row 做任何寫回：這個 stopTarget 本身就是一次性
	// 判定（見 stopTarget 的說明），Session 欄位留給鑑識原封不動——task 7
	// review round 3, important finding 證明了「只清 Session、不清
	// Worktree」會讓一列失去唯一能安全重新確認它的識別資訊，之後任何路徑
	// 想再動它的 worktree 都沒有 session 名可以鎖。不清欄位就沒有這個問題。
	for _, st := range stopOnly {
		stopSessionGuarded(ctx, root, sm, stopper, st)
	}

	// candidates 這邊，「要不要動手」與「動手」發生在同一把鎖之內，任何一個
	// 破壞性動作（停 driver、停 tmux session、刪 worktree、刪 sandbox
	// root）都不例外（task 7 review round 2, critical finding：只要有一項
	// 漏在鎖外或查核之前，這個 session 就可能是合法追問剛剛重新建起來的活
	// 東西，不是第 1 步選中的那個）。停 session 不再看任何「這一輪是不是剛
	// 好轉換」的旗標——每一次真的要動手拆除，都無條件先停一次 driver 與
	// tmux（對已經停過的呼叫本身無害），這樣「這一輪被鎖擋下、下一輪才真正
	// 動手」的重試序列，停止動作永遠跟拆除動作綁在同一次嘗試裡，不會漏掉
	// （task 7 review round 3, critical finding）。
	var succeeded []reclaimCandidate
	for _, c := range candidates {
		if c.session == "" {
			// 沒有 session 名可鎖：這代表這一列已經失去「用哪個 session 名
			// 確定性推導出這條 worktree」的識別資訊——理論上不會發生
			// （Worktree 一律搭配 Session 產生），而且沒有 session 名就沒有
			// 鎖可拿，連 candidateStillMatches 的「查完再動手」都會在查完
			// 之後、動手之中留一個沒有任何機制能關閉的窗口（task 7 review
			// round 3, important finding）。與其冒風險做一次形同沒做的檢
			// 查，直接跳過、等有人補上識別資訊或人工處理，絕不在沒有辦法
			// 安全重新確認的情況下刪任何東西。
			log.Printf("a2a: sweep: context %s 的 worktree %s 沒有 session 身分可以安全重新確認，本輪跳過（需要人工檢查）", c.contextID, c.worktree)
			continue
		}
		unlock, ok := tryLockSandboxSessionForTeardown(c.session)
		if !ok {
			log.Printf("a2a: sweep: context %s 的 session %s 目前正在使用中（建立或投遞中），本輪跳過，留給下一次 sweep", c.contextID, c.session)
			continue
		}
		if !candidateStillMatches(root, c) {
			log.Printf("a2a: sweep: context %s 在拆除前已換身分，跳過（不動它的 session/worktree）", c.contextID)
			unlock()
			continue
		}
		// 先停 driver 再停 tmux，最後才動磁碟：cycle 的順序是 collect →
		// sweep → drain → EnsureSandboxDrivers，所以 sweep 動手時 driver 還
		// 活著，它下一輪 RunWorkerOnce 的第一件事就是 Init(root)，會把即將
		// 刪掉的目錄樹重建回來（task 7, D4）。三者現在都在同一把鎖、同一次
		// 身分確認之後、每一次嘗試都無條件執行，缺一都不行。
		if stopper != nil {
			stopper.Stop(c.session)
		}
		_ = sm.Stop(ctx, c.session)
		ok2 := removeCandidate(ctx, root, sm, c)
		unlock()
		if ok2 {
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

// stopSessionGuarded 在同一把互斥鎖與身分重確認之下，停掉一個「只停不拆」
// 的 session（目前只有派送中崩潰這條路會用到：t.State 轉成 TaskFailed，但
// worktree/sandbox root 保留給鑑識，只需要把可能還殘留的 tmux session 與
// driver 收掉）。拿不到鎖，或鎖內重新確認發現身分已經變了，都直接放棄，留
// 給下一次 sweep —— 跟 candidates 那邊同一套規則，絕不在查核之前或鎖外做
// 任何破壞性動作。
func stopSessionGuarded(ctx context.Context, root string, sm SessionManager, stopper SandboxStopper, st stopTarget) {
	if st.session == "" {
		return
	}
	unlock, ok := tryLockSandboxSessionForTeardown(st.session)
	if !ok {
		log.Printf("a2a: sweep: context %s 的 session %s 目前正在使用中，本輪跳過停止，留給下一次 sweep", st.contextID, st.session)
		return
	}
	defer unlock()
	if !stopTargetStillMatches(root, st) {
		log.Printf("a2a: sweep: context %s 在停止前已換身分，跳過（不動它的 session）", st.contextID)
		return
	}
	if stopper != nil {
		stopper.Stop(st.session)
	}
	_ = sm.Stop(ctx, st.session)
}

// stopTargetStillMatches 是 stopSessionGuarded 專用的重確認：只比對
// TaskID / State / Session 三欄位（沒有 Worktree，因為這條路從不動
// worktree），與 candidateStillMatches 同一個道理。
func stopTargetStillMatches(root string, st stopTarget) bool {
	match := false
	_ = WithTasks(root, func(tasks *TaskStore) error {
		if t, ok := tasks.ByContext(st.contextID); ok {
			match = t.TaskID == st.taskID && t.State == st.state && t.Session == st.session
		}
		return errNothingSwept // 只讀不寫
	})
	return match
}

// candidateStillMatches 在真的動手之前，用一次短的 WithTasks 重新確認該
// contextId 的 row 仍是同一身分（TaskID / State / Worktree / Session 四欄位，
// 與第 3 步同一組比較）。第 3 步的比對只決定「要不要清欄位」—— 保住了帳，
// 沒保住磁碟；這一條才保住磁碟。呼叫方必須在持有
// tryLockSandboxSessionForTeardown(c.session) 成功取得的鎖期間呼叫這個函式
// 並緊接著呼叫 removeCandidate，否則「確認完到動手之間」仍有窗口。
func candidateStillMatches(root string, c reclaimCandidate) bool {
	match := false
	_ = WithTasks(root, func(tasks *TaskStore) error {
		if t, ok := tasks.ByContext(c.contextID); ok {
			match = t.TaskID == c.taskID && t.State == c.state &&
				t.Worktree == c.worktree && t.Session == c.session
		}
		return errNothingSwept // 只讀不寫
	})
	return match
}

// removeCandidate 執行實際的磁碟回收。任何一項失敗就回 false，該 candidate
// 留在原地由下一趟 sweep 重試。
func removeCandidate(ctx context.Context, root string, sm SessionManager, c reclaimCandidate) bool {
	ok := true
	if c.worktree != "" {
		if err := sm.RemoveWorkspace(ctx, c.projectDir, c.worktree); err != nil {
			log.Printf("a2a: sweep: failed to remove worktree %s for context %s (left in place, will retry next sweep): %v", c.worktree, c.contextID, err)
			ok = false
		}
	}
	if c.session != "" {
		if err := os.RemoveAll(SandboxRoot(root, c.session)); err != nil {
			log.Printf("a2a: sweep: failed to remove sandbox root for context %s (left in place, will retry next sweep): %v", c.contextID, err)
			ok = false
		}
		// 政策檔與沙盒同生共死。清不掉只 log,不影響回收判定 —— 下一趟
		// sweep 會重試,而一份指向已不存在 worktree 的政策檔本身無害
		// (gate 只在該 session 名的 hook 行程裡才會讀到它)。
		if err := RemoveSandboxPolicy(root, c.session); err != nil {
			log.Printf("a2a: sweep: 刪除 %s 的政策檔失敗(下一趟重試): %v", c.session, err)
		}
	}
	return ok
}
