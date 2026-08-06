package channelagent

import (
	"context"
	"errors"
	"fmt"
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
//
// 認領之後、呼叫 Start 之前（鎖外），每一列都重讀 callers.json / agents.json
// 重新驗證一次（task 8, I1）：核准在 message/send 那一刻查過，不代表十次
// cycle 之後、backlog 真的被排空那一刻仍然成立。這是每次呼叫都做的，不是
// 只在第一次派送時做——撤銷要對已經排隊、還沒起沙盒的工作生效，不能只擋
// 未來的新請求。
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

	// 撤銷必須對已排隊的工作生效，不只對新請求生效。每次呼叫都重讀
	// callers.json / agents.json：一個被 operator 撤銷的呼叫方，它先前灌進
	// 佇列的 backlog 不可以繼續被排空成新沙盒。拒絕的 row 轉 TaskFailed 並
	// 寫明 Detail + 一筆稽核 —— 不是靜默 continue（那正是 I1 的形態）。
	callers, cerr := LoadCallers(root)
	agents, aerr := LoadAgents(root)
	// drainRejectReason 的頭兩個分支（cerr/aerr 非 nil）回的是包住 store
	// 讀檔錯誤的字串，可能夾帶 callers.json/agents.json 的絕對路徑；其餘分
	// 支都是固定字面文字加上呼叫方自己的 caller/agent 名稱，不含 host 路
	// 徑。這兩個變數對整個迴圈是常數，於是同一批呼叫全部共用同一個安全判
	// 定——cerr/aerr 一旦非 nil，這一輪產生的每個 reason 字串都走的是同一
	// 個不安全分支。
	reasonSafe := cerr == nil && aerr == nil
	started := 0
	for _, t := range claimed {
		if reason := drainRejectReason(callers, agents, cerr, aerr, t); reason != "" {
			failDrainedTask(root, t, reason, reasonSafe)
			continue
		}
		if err := ex.Start(ctx, t, t.Prompt); err != nil {
			continue // executor 已經把失敗記在 row 上了
		}
		started++
	}
	return started, nil
}

// drainRejectReason 回傳這一列不該被派送的理由，可派送則回 ""。
func drainRejectReason(callers CallerStore, agents AgentStore, cerr, aerr error, t A2ATask) string {
	if cerr != nil {
		return "caller store unavailable: " + cerr.Error()
	}
	if aerr != nil {
		return "agent store unavailable: " + aerr.Error()
	}
	var caller Caller
	found := false
	for _, c := range callers.Callers {
		if c.CallerID == t.CallerID {
			caller, found = c, true
			break
		}
	}
	if !found || caller.Status != CallerApproved {
		return "caller " + t.CallerID + " is no longer approved (revoked or removed)"
	}
	a, ok := agents.Get(t.Agent)
	if !ok {
		return "agent " + t.Agent + " no longer exists"
	}
	if !a.Enabled {
		return "agent " + t.Agent + " is disabled"
	}
	// row 記錄的等級高於該 caller 目前的授權 → 拒絕。降級（例如 full 改成
	// develop）也算：一個排隊中的 full 任務不該在授權被降之後仍以 full 起跑。
	if grantRank(t.Level) > grantRank(caller.EffectiveGrantLevel()) {
		return "task level " + string(t.Level) + " exceeds the caller's current grant"
	}
	return ""
}

// revokeReasonForRunningTask 跟 drainRejectReason 幾乎一樣，只多一層保護，
// 只用在撤銷偵測（正在跑的 TaskWorking / TaskDispatching row），DrainQueue
// 的新派送閘門不走這條、繼續用原本嚴格的 drainRejectReason（round 10 review,
// Important，D10-5）：
//
// LoadAgents 現在會把名字不合法、或 channel_id 撞到某個 binding 的 agent
// 濾掉。那個過濾對「要不要接受新派送」是對的——不該讓一個名字有問題的 agent
// 建立新沙盒；但對「要不要撤銷已經在跑的任務」是不對的——一次手誤（打錯字、
// channel_id 恰好跟某個新建的 binding 撞了）不該跟操作者刻意把 agent
// 刪除/停用有一樣的後果。
//
// round 10 review 第二輪，Important：第一版一撞見「filtered 版本找不到這個
// agent 名字」就整段放過，等於把過濾造成的假陽性豁免權套用到 drainRejectReason
// 回的**任何**理由上——包含跟 agent 過濾完全無關的 caller 撤銷、caller 等級
// 被降。用未過濾的 rawAgents 重跑一次完整的 drainRejectReason（caller
// 狀態、agent 存在、enabled、grant level 全部重查一遍），只有在「用未過濾
// 視角看，這一列什麼問題都沒有」的時候才算數——那才真的只剩下「agent 被過
// 濾」這一個原因，其他任何真正的撤銷理由（caller 被撤銷、agent 被明確停用、
// 等級被降）都會在 rawReason 裡冒出來，於是豁免不成立，維持原本的撤銷判定。
// rawErr != nil（讀檔失敗）時無法重跑這個判定，保守起見維持 drainRejectReason
// 原本的判定（fail closed，跟 cerr/aerr 那條既有規則一致的方向）。
func revokeReasonForRunningTask(callers CallerStore, agents, rawAgents AgentStore, rawErr error, t A2ATask) string {
	reason := drainRejectReason(callers, agents, nil, nil, t)
	if reason == "" {
		return ""
	}
	if rawErr == nil {
		if rawReason := drainRejectReason(callers, rawAgents, nil, nil, t); rawReason == "" {
			log.Printf("a2a: sweep: agent %q 在 agents.json 裡存在、caller 跟等級也都沒問題，只是這次驗證沒通過（名稱不合法或 channel_id 跟某個 binding 撞了）——不對它正在跑的任務做撤銷，這是設定檔問題，不是操作者刻意移除；context %s 保持原樣，等 operator 修好設定", t.Agent, t.ContextID)
			return ""
		}
	}
	return reason
}

// safe 是 reason 本身(不含 cur.Detail 裡任何已經存在的舊內容)的安全性；
// 合成規則見 appendDetail 的說明（既有內容是空字串時不參與 AND，避免把
// 一段本來乾淨的固定字面 reason 錯殺成不安全）。
func failDrainedTask(root string, t A2ATask, reason string, safe bool) {
	_ = WithTasks(root, func(tasks *TaskStore) error {
		cur, ok := tasks.ByContext(t.ContextID)
		if !ok || !CanTransition(cur.State, TaskFailed) {
			return errNothingToDrain
		}
		cur.State = TaskFailed
		cur.Detail, cur.DetailSafe = appendDetail(cur.Detail, cur.DetailSafe, reason, safe)
		cur.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		tasks.Upsert(cur)
		return nil
	})
	_ = AppendAudit(root, AuditEntry{
		At:        time.Now().UTC().Format(time.RFC3339),
		CallerID:  t.CallerID,
		Agent:     t.Agent,
		ContextID: t.ContextID,
		TaskID:    t.TaskID,
		Summary:   reason,
		Outcome:   "drain_rejected",
	})
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

const (
	// MaxTaskRows 是終止狀態 row 的保留上限（依 CompletedAt 由新到舊）。
	MaxTaskRows = 500
	// TaskRetention 是終止狀態 row 的保留期。
	TaskRetention = 14 * 24 * time.Hour
)

// PruneTasks 修剪 tasks.json：終止狀態的 row 依 CompletedAt 由新到舊保留前
// MaxTaskRows 筆，且丟棄超過 TaskRetention 者。非終止的 row 永不丟棄 ——
// 它們還在跑，丟掉就等於製造孤兒沙盒。
//
// 終止不代表可以丟。一列即使終止，只要還有東西靠它才能被找到，就不符合刪除
// 資格 —— 具體是下面任何一項為真：
//   - Worktree != ""：worktree 還沒被 SweepTimeouts 回收（forensics 保留、
//     RetainAfterComplete 的優惠期、或單純還沒輪到），SweepTimeouts 的候選
//     清單完全靠掃 tasks.Tasks 產生，這一列從檔案裡消失，worktree 就永遠
//     不會再被任何機制看見，變成沒有主人的孤兒。
//   - SessionStopPending：session 是否真的停過還沒被確認，下一輪 sweep 需要
//     靠這一列重新嘗試（見 A2ATask.SessionStopPending 的說明）。
//
// round 11 review, Important 1：Session != "" 刻意不在這個清單裡，即使它
// 表面上跟 Worktree 對稱。handleRPC 在 accept 那一刻就把 Session 填進 row
// （SessionNameFor 是 contextId 的確定性函式，跟沙盒有沒有真的起來完全無
// 關），但 Worktree 只在 SandboxExecutor.Start 真的跑到「拿到 agent、驗證
// 過 grant level」那一步之後才被賦值、persist 進 row（a2a_executor.go:176，
// 早於任何真正的磁碟/tmux side effect）。因此：
//   - 任何一列如果真的起過 tmux session（不管現在死活），它的 Worktree 在
//     那之前就已經被 persist 寫進 row 了——「session 真的活過」蘊含
//     「Worktree != ""」，只看 Worktree 不會漏掉任何有真實磁碟/session
//     足跡的 row。
//   - 反過來，Session != "" 但 Worktree == "" 的 row 只可能是「Start 從未
//     真正跑起來」的一列：DrainQueue 認領後因 caller 被撤銷/agent 被停用
//     由 failDrainedTask 直接判 failed（從未呼叫 Start）、或 Start 內部在
//     還沒設定 Worktree 之前就失敗（unknown/disabled agent、grant level
//     不合法）。這種 row 從來沒有任何磁碟或 tmux 東西存在過，把它的 Session
//     當成「還有東西要靠它才找得到」是誤判——原本的檢查讓它永遠非空、永遠
//     擋住 PruneTasks，tasks.json 又變回無上限成長（正是這條規則要修的問
//     題，只是換了一個欄位；一個被撤銷 caller 排隊中的 N 個 contextId 會
//     全部變成永久 row）。
//
// 只有這兩項都不成立 —— 也就是 SweepTimeouts 已經把這一列的磁碟/session
// 回收乾淨，或者這一列從來就沒有東西需要回收 —— 才會真的進入排名/保留期
// 判斷。這樣的 row 因此只跳過這一輪，不是永遠豁免：等它被 sweep 收乾淨之
// 後，之後某一次 PruneTasks 呼叫會再看到它並判定資格。
//
// TaskWorking/TaskDispatching（非終止）的 row 完全不進終止排名，所以「結果
// 還沒被 CollectResults 收下」不需要另外檢查：CollectResults 把 row 轉終
// 態、寫入 Detail 是同一次鎖內動作（a2a_result.go），在那之前 row 根本不是
// 終止狀態，PruneTasks 看不到它。
//
// contextId 由呼叫方指定、1-128 字元，所以沒有上限時 row 數完全由對方決定，
// 而每個 handler 的擁有權檢查都排在一次單調成長的 O(N) 整檔讀寫後面。
// 每個 A2A cycle 結束時呼叫一次。回傳丟棄的筆數。
func PruneTasks(root string, now time.Time) (int, error) {
	dropped := 0
	err := WithTasks(root, func(tasks *TaskStore) error {
		type row struct {
			idx  int
			done time.Time // 零值（缺漏／無法解析）排在最舊
		}
		var terminal []row
		for i, t := range tasks.Tasks {
			if !isTerminal(t.State) {
				continue
			}
			d, _ := parseRFC3339(t.CompletedAt)
			terminal = append(terminal, row{i, d})
		}
		sort.Slice(terminal, func(a, b int) bool { return terminal[a].done.After(terminal[b].done) })

		drop := map[int]bool{}
		for rank, r := range terminal {
			t := tasks.Tasks[r.idx]
			// 還沒被 sweep 回收乾淨、或還在等 session-stop 確認：不管排名或
			// 保留期，一律留著，交給下一次。Session 本身刻意不在這個判斷
			// 裡——見上面函式說明 round 11 review, Important 1。
			if t.Worktree != "" || t.SessionStopPending {
				continue
			}
			if rank >= MaxTaskRows {
				drop[r.idx] = true
				continue
			}
			if !r.done.IsZero() && now.Sub(r.done) > TaskRetention {
				drop[r.idx] = true
			}
		}
		if len(drop) == 0 {
			return errNothingSwept
		}
		kept := tasks.Tasks[:0]
		for i, t := range tasks.Tasks {
			if drop[i] {
				dropped++
				continue
			}
			kept = append(kept, t)
		}
		tasks.Tasks = kept
		return nil
	})
	if err != nil && !errors.Is(err, errNothingSwept) {
		return 0, err
	}
	return dropped, nil
}

// LivenessGrace 是一列進入 dispatching 之後，多久才開始檢查它的 tmux
// session 還在不在。DispatchedAt 起算——剛起來的 session 有 tmux server 尚
// 未就緒的窗口（EnsureWorkspace + Sessions.Start 最長 90 秒），沒有寬限期
// 會把還在合法開機視窗內的健康派送誤殺。working 的 row 不套用這個寬限期：
// SandboxExecutor.Start 只在 Sessions.Start 真的成功（tmux session 已經存
// 在）之後才會把 row 寫成 TaskWorking，所以 working 這個狀態本身就是「
// session 已經存在」的證明，用 StartedAt（提交/排隊時刻，跟這個 session
// 何時真正起來完全無關——round 9 review, Minor 2）算寬限期反而會誤判：一個
// 排隊 30 分鐘才被撿走、剛開機 1 秒的健康 session，StartedAt 早就過了
// LivenessGrace，會被誤判成「已經過了寬限期、該檢查了」而立刻檢查——這裡
// 剛好是安全的（因為根本不需要保護），但語意是錯的，所以刻意不比照
// dispatching 套用 StartedAt。
const LivenessGrace = 2 * time.Minute

// VanishedConfirmStrikes 是連續幾次「tmux 回報這個 session 真的不存在」才
// 真正判定 vanished、轉 TaskFailed。單一次取樣不夠：一次 fork EAGAIN（這台
// 機器有 OOM 史）、tmux 執行檔暫時找不到、或 sweep 自己的 ctx 在 serve 關機
// 時被取消，都可能讓某一輪的判定不可靠——round 9 review, Critical：這些情
// 況現在都會讓 TmuxSessionAlive 回傳非 nil 的 error（見該函式），不會累計
// 到這個計數上；只有「真的問到了、tmux 明確回報沒有這個 session」才會累
// 計。
//
// 2 次在真實時間上大約是一個 A2ACycleInterval（預設 10 秒）：第一次取樣
// 把 strikes 記到 1，下一輪 sweep（約 10 秒後）再取樣一次才會真的動手，
// 兩次取樣之間的真實間隔大約就是這 10 秒，不是 2 個週期。round 10 review,
// Minor：既然 ctx 取消、fork 失敗、執行檔找不到這些「問不到答案」的情況都
// 已經被排除在計數之外（見上），這個門檻真正要擋的只剩下「tmux 兩次都真
// 的跑起來、兩次都明確回報沒有這個 session，但其實搞錯了」這一種極窄的
// 剩餘情況——用一次額外的 ~10 秒延遲換掉單次快照判定帶來的誤殺風險，是合
// 理的取捨；調高這個值代價是拉長 vanished 偵測的真實延遲（每加 1 就多約
// 一個 A2ACycleInterval），調低則是放寬到單次取樣，兩者都要一起考慮。
const VanishedConfirmStrikes = 2

// appendDetail 把新的 reason 接在既有 Detail 後面（用「; 」分隔），不覆寫掉
// 卡住之前就已經留在 Detail 裡的線索（常見的是 task 4 的 TrustFolder 失敗
// 警告）。HardTimeout 取消與派送中崩潰兩條既有路徑都遵守這個慣例（見
// TestSweepPreservesPriorDetailOnHardTimeout / …OnStaleDispatch，commit
// b8c5ab0），存活偵測與撤銷偵測這兩條新路徑必須一致——否則卡住前的原因會
// 在最後一步斷掉（round 9 review, Important）。
//
// existingSafe 是接續前既有 Detail 的安全旗標，reasonSafe 是這次要接上去
// 的 reason 本身的安全性；回傳的 safe 是合成後的旗標，呼叫方必須把它寫回
// DetailSafe。existing 是空字串時沒有任何「舊內容」可以不安全，是 AND 的
// 單位元（vacuously safe）——只有 existing 真的非空時，existingSafe 才實
// 際參與這次合成。少了這一條，第一次寫入的固定字面 reason 會被預設值
// false 的 existingSafe 錯殺成不安全（round-11-review 第二輪 Minor 1/2：
// 一列從沒動過的 Detail 是空字串，接上「hard timeout exceeded」這種完全
// 乾淨的固定字面文字，結果卻被回應層擋下來，換成一句「internal error」，
// 呼叫方連自己的任務是被兩小時逾時砍掉的都看不到）。
//
// reasonSafe 沒有預設值可省略，逼著每個呼叫點都要決定它接上去的到底是固
// 定字面文字（true）還是包著 err.Error() 的字串（false）——不能讓「反正
// 目前接的都是字面文字」這個事實只活在程式碼恰好沒人接過別的東西的巧合
// 裡（round-11-review 第二輪 Minor 3）。
func appendDetail(existing string, existingSafe bool, reason string, reasonSafe bool) (detail string, safe bool) {
	safe = (existing == "" || existingSafe) && reasonSafe
	if existing == "" {
		return reason, safe
	}
	return existing + "; " + reason, safe
}

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
// Between step 1 and step 2 (task 8, I7) sits a third, independent lock-out-
// lock-in round trip: every TaskWorking/TaskDispatching row whose session was
// claimed at least LivenessGrace ago is checked with sm.Alive (shelling out to
// tmux, so outside the lock, same as everything else that touches a session)
// and, if its session has actually vanished, transitioned straight to
// TaskFailed with a re-verified identity check — the same forensics-preserving
// terminal state a HardTimeout cancel does NOT get, because a crashed sandbox
// is a failure, not an operator cancellation. This is deliberately a separate
// round trip rather than folded into step 1's single pass: step 1 never shells
// out to anything, and Alive must run outside the tasksMu lock like every
// other tmux call in this file.
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
						// 保留卡住前就已經留在 Detail 裡的線索,跟下面
						// TaskCanceled 那條路一樣的理由(task 4):不能讓
						// sweep 自己的 reason 蓋掉它(review round 2, minor
						// 2)。"dispatch stalled..." 是固定字面文字
						// (newSafe=true)；appendDetail 正確處理 t.Detail
						// 目前是空字串的情況(round-11-review 第二輪 Minor
						// 2：不會因為 t.DetailSafe 的預設值是 false 就把這
						// 段本來乾淨的文字錯殺成不安全)。
						t.Detail, t.DetailSafe = appendDetail(t.Detail, t.DetailSafe, "dispatch stalled (no sandbox came up)", true)
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
				// reason（"hard timeout exceeded" 或「start time unreadable
				// ...」）都是固定字面文字（newSafe=true）：呼叫方應該能讀到
				// 自己的任務是被兩小時逾時砍掉的，不該被硬逾時本身的固定訊
				// 息換成一句「internal error」（round-11-review 第二輪 Minor
				// 2）。
				t.Detail, t.DetailSafe = appendDetail(t.Detail, t.DetailSafe, reason, true)
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

	// --- 存活偵測（I7）。挑清單在鎖內，tmux 呼叫在鎖外，落帳再回鎖內。 ---
	//
	// dispatching 用 DispatchedAt 套 LivenessGrace（還在合法開機視窗內就不
	// 查）；working 立刻查——見 LivenessGrace 的說明，working 這個狀態本身
	// 已經證明 session 存在過，不需要、也不該用 StartedAt（提交時刻）當寬
	// 限期基準。
	var liveCheck []A2ATask
	_ = WithTasks(root, func(tasks *TaskStore) error {
		for _, t := range tasks.Tasks {
			if t.State != TaskWorking && t.State != TaskDispatching {
				continue
			}
			if t.Session == "" {
				continue
			}
			if t.State == TaskDispatching {
				at, ok := parseRFC3339(t.DispatchedAt)
				if !ok || now.Sub(at) < LivenessGrace {
					continue
				}
			}
			liveCheck = append(liveCheck, t)
		}
		return errNothingSwept // 只讀不寫
	})
	// alive==false 才累計 strike；err!=nil（round 9 review, Critical：ctx
	// 取消、fork EAGAIN、tmux 執行檔找不到……這些「問不到答案」的情況，
	// TmuxSessionAlive 現在會誠實回傳非 nil 的 error）完全不碰計數——不遞
	// 增也不清零，等下一輪用一次真正問到答案的取樣再說。alive==true 則清
	// 零：這是「連續」次數，不是「累計」次數。
	var toIncrement []A2ATask
	var toReset []A2ATask
	for _, t := range liveCheck {
		alive, err := sm.Alive(ctx, t.Session)
		if err != nil {
			log.Printf("a2a: sweep: 檢查 %s 存活失敗，這一趟先當它還活著（不計入、不清零 strike）: %v", t.Session, err)
			continue
		}
		if alive {
			if t.VanishedStrikes != 0 {
				toReset = append(toReset, t)
			}
			continue
		}
		toIncrement = append(toIncrement, t)
	}
	if len(toIncrement) > 0 || len(toReset) > 0 {
		_ = WithTasks(root, func(tasks *TaskStore) error {
			changed := false
			for _, v := range toReset {
				cur, ok := tasks.ByContext(v.ContextID)
				if !ok || cur.TaskID != v.TaskID || cur.State != v.State || cur.Session != v.Session {
					continue
				}
				if cur.VanishedStrikes != 0 {
					cur.VanishedStrikes = 0
					tasks.Upsert(cur)
					changed = true
				}
			}
			for _, v := range toIncrement {
				cur, ok := tasks.ByContext(v.ContextID)
				// 身分必須還是同一個，否則就是拆除窗口內的重新提交。
				if !ok || cur.TaskID != v.TaskID || cur.State != v.State || cur.Session != v.Session {
					continue
				}
				cur.VanishedStrikes++
				if cur.VanishedStrikes >= VanishedConfirmStrikes {
					cur.State = TaskFailed
					// "sandbox session vanished" 是固定字面文字（newSafe=true）。
					cur.Detail, cur.DetailSafe = appendDetail(cur.Detail, cur.DetailSafe, "sandbox session vanished", true)
					cur.CompletedAt = now.UTC().Format(time.RFC3339)
					cur.VanishedStrikes = 0 // row 已經是終態，不必再留計數
					// round 10 review, Important：不在這裡直接塞進 stopOnly
					// 一次性名單——鎖忙的那一刻恰好可能是「session 其實還
					// 活著」的假陽性場景（一個合法的 DeliverFollowUp 正在
					// 投遞），不能假設之後不會再有機會。改成設一個耐久標
					// 記，交給下面統一、每輪都會重新掃描的可重試機制（見
					// A2ATask.SessionStopPending）。
					cur.SessionStopPending = true
				}
				tasks.Upsert(cur)
				changed = true
			}
			if !changed {
				return errNothingSwept
			}
			return nil
		})
	}

	// --- 撤銷偵測，跑得到的一半（D6）。DrainQueue 的重驗證只碰得到還在佇列
	// 裡、還沒認領成功的 row——一旦一列真的變成 TaskWorking（或還在
	// TaskDispatching 途中），它再也不會回到 DrainQueue 的認領迴圈，caller
	// 被撤銷、agent 被停用完全不會反映到它身上，只能等它自然做完或撐到兩
	// 小時硬逾時。這裡補上對稱的一半：重讀 callers.json / agents.json，套
	// 用跟 DrainQueue 完全一樣的 drainRejectReason，任何驗證失敗的
	// TaskWorking / TaskDispatching row 先把政策檔覆寫成 revoked——這是
	// 唯一真正讓沙盒下一次工具呼叫就被擋的動作，比等 session 被拆掉快得
	// 多——再交給既有的單一拆除路徑（下面的 stopOnly + stopSessionGuarded，
	// 跟派送中崩潰那條路完全同一段程式碼）停掉 driver 與 tmux，不新開第二
	// 條拆除路徑。CallerID 為空的 row 視為不受 A2A caller/agent 授權模型
	// 管轄（合成/內部用途；真正經 message/send 派送的任務一律有非空
	// CallerID），略過——否則會誤殺大量跟撤銷完全無關、本來就沒有登記
	// caller 的既有沙盒逾時/回收測試。
	var authCheck []A2ATask
	_ = WithTasks(root, func(tasks *TaskStore) error {
		for _, t := range tasks.Tasks {
			if t.State != TaskWorking && t.State != TaskDispatching {
				continue
			}
			if t.Session == "" || t.CallerID == "" {
				continue
			}
			authCheck = append(authCheck, t)
		}
		return errNothingSwept // 只讀不寫
	})
	if len(authCheck) > 0 {
		callers, cerr := LoadCallers(root)
		agents, aerr := LoadAgents(root)
		// round 10 review, Important（D10-5）：LoadAgents 現在會把名字不合法、
		// 或 channel_id 撞到某個 binding 的 agent 濾掉（見 a2a_agents.go）。
		// 那個過濾對「要不要接受新派送」是對的，但對「要不要撤銷已經在跑的
		// 任務」是不對的——一次手誤（打錯字、channel_id 恰好跟某個新建的
		// binding 撞了）不該跟操作者刻意把 agent 刪除/停用有一樣的後果（連
		// 坐撤銷這個 agent 名下每一個正在跑的沙盒）。這裡額外讀一份未過濾
		// 的版本，只用來分辨下面 revokeReasonForRunningTask 裡「agents.json
		// 真的沒有這個名字」跟「有，只是這次驗證沒通過」兩種情況。
		rawAgents, rawErr := LoadAgentsRaw(root)
		// round 9 review, Critical：讀 callers.json / agents.json 失敗，對
		// 「還沒起沙盒」的排隊工作該當拒絕（fail closed，DrainQueue 走的
		// 路，drainRejectReason 對它維持原樣）；但對「已經在跑」的沙盒，套
		// 用同一條規則等於把一次暫時性讀檔失敗（operator 手動編輯
		// callers.json 途中，寫到一半的半份 JSON 是正常事件，report 裡已
		// 經明說這是手動編輯）放大成「每一列都判定 caller 不存在 →
		// 全部撤銷」——一次沒寫完的 vim 存檔就能把全部健康中的沙盒都拆
		// 光。整段跳過，留給下一次 sweep 重試，絕不把讀檔失敗本身當成拒絕
		// 理由套用到任何一列 working/dispatching row 上。
		if cerr != nil || aerr != nil {
			log.Printf("a2a: sweep: 撤銷偵測讀取 callers.json / agents.json 失敗，本輪整段跳過（下一趟重試，不動任何一列在跑的沙盒）: callers=%v agents=%v", cerr, aerr)
			authCheck = nil
		}
		for _, t := range authCheck {
			reason := revokeReasonForRunningTask(callers, agents, rawAgents, rawErr, t)
			if reason == "" {
				continue
			}
			unlock, ok := tryLockSandboxSessionForTeardown(t.Session)
			if !ok {
				log.Printf("a2a: sweep: context %s 的 session %s 目前正在使用中，本輪跳過撤銷，留給下一次 sweep", t.ContextID, t.Session)
				continue
			}
			// 重新確認身分：authCheck 是在上面釋放鎖之前讀到的快照，鎖內動手
			// 前必須再讀一次，否則可能撤銷一個剛好合法重新建起來的新任務
			// （跟 candidateStillMatches / stopTargetStillMatches 同一個道
			// 理，直接借用後者的欄位比對——TaskID/State/Session 三欄）。
			pre := stopTarget{taskID: t.TaskID, contextID: t.ContextID, session: t.Session, state: t.State}
			if !stopTargetStillMatches(root, pre) {
				log.Printf("a2a: sweep: context %s 在撤銷前已換身分，跳過（不動它的 session）", t.ContextID)
				unlock()
				continue
			}
			// 能力先收回，任何更慢的事之後才做（fail-safe 的核心順序）：
			// 覆寫政策檔失敗就整段放棄，row 保持原樣、留給下一次 sweep 重
			// 試——絕不能先把 row 轉成 failed、卻讓沙盒帶著原本沒被動過的
			// 政策檔繼續跑，那樣它反而變得比修之前更難被發現還在運作。
			if err := RevokeSandboxPolicy(root, t.Session); err != nil {
				log.Printf("a2a: sweep: 撤銷 %s 的政策檔失敗，本輪放棄（下一趟重試）: %v", t.Session, err)
				unlock()
				continue
			}
			_ = WithTasks(root, func(tasks *TaskStore) error {
				cur, ok := tasks.ByContext(t.ContextID)
				if !ok || cur.TaskID != t.TaskID || cur.State != t.State || cur.Session != t.Session {
					return errNothingSwept
				}
				cur.State = TaskFailed
				// reason 來自 revokeReasonForRunningTask，永遠以 nil,nil 呼叫
				// drainRejectReason（見該函式），因此永遠落在固定字面文字 +
				// 呼叫方自己的 caller/agent 名稱那幾個分支（newSafe=true）,
				// 絕不會是包住 cerr/aerr 的那兩個不安全分支。
				cur.Detail, cur.DetailSafe = appendDetail(cur.Detail, cur.DetailSafe, reason, true)
				cur.CompletedAt = now.UTC().Format(time.RFC3339)
				// round 10 review, Important：跟存活偵測同一個道理——鎖忙
				// 的那一刻不代表之後不會再有機會，改成設耐久標記交給下面
				// 統一的可重試掃描，不在這裡直接塞一次性的 stopOnly。
				cur.SessionStopPending = true
				tasks.Upsert(cur)
				return nil
			})
			unlock()
		}
	}

	// --- 可重試的 session-stop（round 10 review, Important）。存活偵測與
	// 撤銷偵測轉終態時只設 SessionStopPending，不直接塞進一次性的
	// stopOnly——這裡才是真正動手、而且每一輪都會重新掃描的地方。跟
	// dispatch-stall 那條既有的一次性 stopTarget 不一樣：那條路徑窄範圍成
	// 立「鎖忙代表另一個合法呼叫正在用同一個 session 名，之後身分本來就會
	// 變」的自我化解論證，這裡不成立——鎖忙的那一刻（最典型是一個合法的
	// DeliverFollowUp 正在投遞）恰好是「這個 session 其實還活著」的假陽性
	// 場景，row 已經轉成終態、不會再變身分，之後也不會有任何其他機制回頭
	// 看它。SessionStopPending 因此設計成完全從「這一列現在的樣子」就能重
	// 新推導出來的耐久狀態：只要它還是 true，就代表這個 session 還沒被確
	// 認停過，任何一輪都該再試一次；stopSessionGuarded 本身已經有鎖
	// +身分重確認，這裡只需要「試過、且真的在有效鎖下動手了」才清旗標
	// ——不論 sm.Stop/stopper.Stop 本身回報成功或失敗（best-effort，整份檔
	// 案一貫的取捨）。完全不動 Session/Worktree：停 session 從來不代表可
	// 以回收磁碟，鑑識保留規則不受影響。
	var pendingStop []A2ATask
	_ = WithTasks(root, func(tasks *TaskStore) error {
		for _, t := range tasks.Tasks {
			if t.State == TaskFailed && t.SessionStopPending && t.Session != "" {
				pendingStop = append(pendingStop, t)
			}
		}
		return errNothingSwept // 只讀不寫
	})
	var stopped []A2ATask
	for _, t := range pendingStop {
		st := stopTarget{taskID: t.TaskID, contextID: t.ContextID, session: t.Session, state: t.State}
		if stopSessionGuarded(ctx, root, sm, stopper, st) {
			stopped = append(stopped, t)
		}
	}
	if len(stopped) > 0 {
		_ = WithTasks(root, func(tasks *TaskStore) error {
			changed := false
			for _, t := range stopped {
				cur, ok := tasks.ByContext(t.ContextID)
				if !ok || cur.TaskID != t.TaskID || cur.State != t.State || cur.Session != t.Session {
					continue
				}
				if cur.SessionStopPending {
					cur.SessionStopPending = false
					tasks.Upsert(cur)
					changed = true
				}
			}
			if !changed {
				return errNothingSwept
			}
			return nil
		})
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
// 的 session：worktree/sandbox root 保留給鑑識，只需要把可能還殘留的 tmux
// session 與 driver 收掉。用在派送中崩潰的一次性 stopTarget，也用在
// vanished/revoked 兩條可重試路徑的每一次嘗試（見 A2ATask.SessionStopPending
// 的說明）。拿不到鎖，或鎖內重新確認發現身分已經變了，都直接放棄，留給下
// 一次 sweep —— 跟 candidates 那邊同一套規則，絕不在查核之前或鎖外做任何
// 破壞性動作。
//
// 回傳值只代表「這一輪有沒有真的在一把有效、身分核對過的鎖底下嘗試過」，
// 不代表 sm.Stop/stopper.Stop 本身有沒有回報成功——那個 error 一律
// best-effort 丟棄（整個檔案的一貫慣例：對一個可能已經不在的 session 呼叫
// Stop 本身無害，不值得為了它的回傳值另外設計重試）。呼叫方（可重試路徑）
// 用這個回傳值決定要不要清掉 SessionStopPending：拿不到鎖或身分不符才是
// 真正需要下一輪再試的情況。
func stopSessionGuarded(ctx context.Context, root string, sm SessionManager, stopper SandboxStopper, st stopTarget) bool {
	if st.session == "" {
		return false
	}
	unlock, ok := tryLockSandboxSessionForTeardown(st.session)
	if !ok {
		log.Printf("a2a: sweep: context %s 的 session %s 目前正在使用中，本輪跳過停止，留給下一次 sweep", st.contextID, st.session)
		return false
	}
	defer unlock()
	if !stopTargetStillMatches(root, st) {
		log.Printf("a2a: sweep: context %s 在停止前已換身分，跳過（不動它的 session）", st.contextID)
		return false
	}
	if stopper != nil {
		stopper.Stop(st.session)
	}
	_ = sm.Stop(ctx, st.session)
	return true
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

// terminateTasks 是 caller revoke / agent disable / task cancel 共用的終止流程。
// 全部在 serve 行程內一次完成，順序刻意如下：
//
//  1. WithTasks：所有 match 且非終止的 row → TaskCanceled + detail + CompletedAt；
//     收集它們的 session。
//  2. 鎖外，且**在停任何東西之前**：把每個 session 的政策檔覆寫成 revoked。
//     這樣 in-flight 的工具呼叫在 session 真的死掉之前就已經開始被 gate 拒絕。
//  3. 停 driver（Stop 阻塞到 goroutine 真的結束），再停 tmux session。
//  4. worktree 回收交給下一趟 sweep —— 它已經會回收 canceled 且仍持有
//     session/worktree 的 row，這裡重做一次只會跟 sweep 搶同一組路徑。
//
// 回傳被終止的 row 數。
func terminateTasks(ctx context.Context, root string, match func(A2ATask) bool, detail string, sm SessionManager, stopper SandboxStopper) (int, error) {
	var sessions []string
	n := 0
	err := WithTasks(root, func(tasks *TaskStore) error {
		now := time.Now().UTC().Format(time.RFC3339)
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			if isTerminal(t.State) || !match(t) {
				continue
			}
			t.State = TaskCanceled
			t.Detail = detail
			t.CompletedAt = now
			tasks.Tasks[i] = t
			if t.Session != "" {
				sessions = append(sessions, t.Session)
			}
			n++
		}
		if n == 0 {
			return errNothingSwept
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothingSwept) {
		return 0, err
	}
	for _, s := range sessions {
		if rerr := RevokeSandboxPolicy(root, s); rerr != nil {
			log.Printf("a2a: 撤銷 %s 的政策檔失敗（session 仍會被停掉）: %v", s, rerr)
		}
	}
	for _, s := range sessions {
		if stopper != nil {
			stopper.Stop(s)
		}
		if sm != nil {
			_ = sm.Stop(ctx, s)
		}
	}
	return n, nil
}

// RevokeCaller 撤銷一個呼叫方，並讓撤銷對已排隊與執行中的工作生效。
func RevokeCaller(ctx context.Context, root, id string, sm SessionManager, stopper SandboxStopper) (int, error) {
	callers, err := LoadCallers(root)
	if err != nil {
		return 0, err
	}
	if !callers.Revoke(id) {
		return 0, fmt.Errorf("unknown caller %q", id)
	}
	if err := SaveCallers(root, callers); err != nil {
		return 0, err
	}
	n, err := terminateTasks(ctx, root, func(t A2ATask) bool { return t.CallerID == id }, "caller revoked", sm, stopper)
	_ = AppendAudit(root, AuditEntry{
		At:       time.Now().UTC().Format(time.RFC3339),
		CallerID: id,
		Summary:  fmt.Sprintf("revoked by operator; %d in-flight task(s) canceled", n),
		Outcome:  "revoked",
	})
	return n, err
}

// DisableAgent 停用一個 agent，語意與 RevokeCaller 相同。
func DisableAgent(ctx context.Context, root, name string, sm SessionManager, stopper SandboxStopper) (int, error) {
	agents, err := LoadAgents(root)
	if err != nil {
		return 0, err
	}
	a, ok := agents.Get(name)
	if !ok {
		return 0, fmt.Errorf("unknown agent %q", name)
	}
	a.Enabled = false
	agents.Remove(name)
	if err := agents.Add(a); err != nil {
		return 0, err
	}
	if err := SaveAgents(root, agents); err != nil {
		return 0, err
	}
	n, err := terminateTasks(ctx, root, func(t A2ATask) bool { return t.Agent == name }, "agent disabled", sm, stopper)
	_ = AppendAudit(root, AuditEntry{
		At:      time.Now().UTC().Format(time.RFC3339),
		Agent:   name,
		Summary: fmt.Sprintf("disabled by operator; %d in-flight task(s) canceled", n),
		Outcome: "agent_disabled",
	})
	return n, err
}

// CancelTask 取消單一 contextId。取消由 operator 執行 —— 刻意不做
// tasks/cancel RPC，呼叫方自助取消屬獨立範圍決策（規格第五節）。
func CancelTask(ctx context.Context, root, contextID string, sm SessionManager, stopper SandboxStopper) error {
	n, err := terminateTasks(ctx, root, func(t A2ATask) bool { return t.ContextID == contextID }, "canceled by operator", sm, stopper)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no active task for contextId %q", contextID)
	}
	return nil
}
