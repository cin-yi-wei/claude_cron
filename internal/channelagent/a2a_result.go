package channelagent

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errNothingCollected signals, from inside a WithTasks callback, that no task
// was promoted this pass. Returning it makes WithTasks discard the (empty)
// mutation and skip the save entirely — CollectResults must still write
// nothing when it promotes nothing.
var errNothingCollected = errors.New("a2a: no results to collect")

// collectResultsAfterScanForTest, when non-nil, runs synchronously right
// after stage 1 finishes (found is populated) and before stage 2 acquires
// tasksMu. Always nil outside tests — this exists purely as a deterministic
// synchronization seam: stage 1 never touches tasksMu, so there is no lock a
// test could hold to force a specific interleaving between "stage 1 already
// matched a file against the row's LastMessageID as it stood then" and "a
// concurrent DeliverFollowUp advances that same row's LastMessageID before
// stage 2 applies". A real goroutine race for this exact window would be
// nondeterministic (stage 1 needs no lock, so nothing blocks it relative to
// an unsynchronized concurrent mutation); this hook removes that gamble.
var collectResultsAfterScanForTest func()

// unreadableResultGrace bounds how fresh an unreadable result file must be
// before pendingResultFile will quarantine it to outbox/failed. The write
// contract (BuildClaudePrompt, adapters.go) is "write <path>.tmp, then
// rename" — a half-written file should never be visible at its final name.
// But that contract lives entirely in a prompt string, not in code: an agent
// that ignores it and writes the final filename directly can leave a
// genuinely half-written file sitting there for an instant. Quarantining on
// first sight (round 10 review, Important, D10-2) would destroy that
// in-flight write's result the moment the write racing with our read loses;
// the pre-fix code merely re-read (and re-failed) it every cycle forever,
// which was safe but noisy. Gating on mtime age keeps the "never destroy a
// write in flight" property while still bounding the retry-forever case: a
// file that is STILL unreadable after this long is genuinely broken, not
// slow, and is quarantined so it stops being re-attempted every cycle.
//
// round 10 review 第二輪，記錄不修：這個寬限期量的是檔案的 mtime，而 mtime
// 是沙盒自己(它的檔案系統操作)寫出來的，不是任何人驗證過的時間戳——一個
// 沙盒可以透過持續碰這個檔案(哪怕只是每隔幾秒 touch 一下)讓自己這份壞掉
// 的結果永遠不被隔離(這正是修這個之前的行為),或者反過來刻意把 mtime 往
// 過去調,讓自己這份可能其實還沒寫完的結果被提早摧毀。兩種效果都只影響
// 「這個沙盒自己的 outbox/pending 目錄」,不會波及其他 session,所以接受
// 這個限制——但下一個讀到這段的人不該假設 mtime 是可信的外部證據。
const unreadableResultGrace = 5 * time.Second

// resultBelongsToTask 比對結果檔是否真的來自這個任務最後一次注入的訊息。
// buildJobID（watcher.go:113）的格式是
// <sanitize(CreatedAt)>-<sanitize(MessageID)>-<inputHash[:12]>，所以
// sanitize(LastMessageID) 必定是 job_id 的子字串。
//
// LastMessageID 為空的 row 一律不接受任何結果檔：那是還沒注入過的任務，
// 不可能有結果，而寬鬆放行正是殘留檔完成新任務的那條路。
//
// 呼叫方必須在鎖內、緊接在真正動手（Upsert 成 completed）之前，用當下最新
// 的 row 重新呼叫這個函式——不能只信任鎖外掃描那一刻讀到的 LastMessageID
// （round 10 review, Important，D10-1）：DeliverFollowUp 會在同一個 row 上
// 把 LastMessageID 從舊訊息換成新訊息，掃描與上鎖之間的窗口內，一份屬於舊
// 訊息的結果檔可能仍然通得過「掃描當時」的比對，卻通不過「上鎖當下」的比
// 對——後者才是唯一該算數的判定，否則呼叫方會拿到「上一個問題」的答案，而
// 「這一個問題」的真正回覆從此再也配不上任何 LastMessageID，永遠卡住。
func resultBelongsToTask(job OutputJob, task A2ATask) bool {
	if task.LastMessageID == "" {
		return false
	}
	return strings.Contains(job.JobID, sanitize(task.LastMessageID))
}

// pendingResultFile locates the sandbox's result file in outbox/pending, if
// any, and returns its path and job_id alongside the decoded text.
// Deliberately returns jobID (not just text) so CollectResults's stage 2 can
// re-run resultBelongsToTask against the row's CURRENT LastMessageID under
// the lock, rather than trusting the match this lock-free scan already made
// against a possibly-stale snapshot (see resultBelongsToTask's doc comment).
func pendingResultFile(root string, task A2ATask, now time.Time) (path string, text string, jobID string, ok bool) {
	dir := pathIn(SandboxRoot(root, task.Session), "outbox", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		var job OutputJob
		if err := ReadJSON(p, &job); err != nil {
			// D10-2：不再無條件搬去 outbox/failed。寫入合約是先寫 .tmp 再
			// rename（adapters.go 的 BuildClaudePrompt），但那個合約只活在
			// 提示詞裡，不是程式碼保證的——一個沒照合約走、直接寫最終檔名
			// 的 agent，可能剛好在我們讀到一半時撞見一份還沒寫完的半成品。
			// 用 mtime 給一段寬限期：剛剛才被動過的壞檔先當「可能還在
			// 寫」，這一輪不動它，留給下一輪重新判斷；只有超過寬限期還讀不
			// 動，才真的判定壞掉並隔離，避免每輪重讀到永遠（原本 continue
			// 的行為）。
			if info, statErr := e.Info(); statErr == nil && now.Sub(info.ModTime()) < unreadableResultGrace {
				continue
			}
			log.Printf("a2a: 無法解讀結果檔 %s，移往 outbox/failed: %v", p, err)
			_ = moveFile(p, filepath.Join(pathIn(SandboxRoot(root, task.Session), "outbox", "failed"), e.Name()))
			continue
		}
		if !resultBelongsToTask(job, task) {
			continue
		}
		return p, job.Text, job.JobID, true
	}
	return "", "", "", false
}

// ResultFor reports the sandbox's reply text, if it has written one. Completion
// is detected by the same outbox-file convention every worker already uses —
// never by scraping the tmux pane.
func ResultFor(root string, task A2ATask) (string, bool) {
	_, text, _, ok := pendingResultFile(root, task, time.Now())
	return text, ok
}

// CollectResults promotes working tasks to completed when their sandbox has
// produced a result. Returns how many tasks were completed.
//
// 三段式：掃描與讀檔在鎖外，WithTasks 內只做純記憶體的狀態轉移，搬檔在鎖後。
// a2a_store.go:17-19 明文規定 callback 內不得做慢工，而這裡的成本與 row 數
// 同步成長，且 tasksMu 被 handler、executor、sweep 共用 —— sweep 已經刻意把
// LoadAgents 提到鎖外（a2a_lifecycle.go），這裡比照辦理。
//
// Session names are deterministic (SessionNameFor), and a contextId whose
// previous task reached a terminal state may later be reused by a different
// caller — mapping to the same SandboxRoot, where the earlier task's result
// file may still sit in outbox/pending. resultBelongsToTask (via
// LastMessageID) is what keeps that stale file from ever satisfying a later
// task under the same session; every result file is still moved out of
// pending (into outbox/sent, mirroring sender.go's convention) the moment it
// completes the task it belongs to, so it can never be read a second time.
func CollectResults(root string, now time.Time) (int, error) {
	// --- 第 1 段：鎖外掃描。快照過期沒關係，第 2 段會逐列重新確認身分。 ---
	snapshot, err := LoadTasks(root)
	if err != nil {
		return 0, err
	}
	type foundResult struct {
		contextID, taskID, session, path, text, jobID string
	}
	var found []foundResult
	for _, t := range snapshot.Tasks {
		if !CanTransition(t.State, TaskCompleted) {
			continue
		}
		path, text, jobID, ok := pendingResultFile(root, t, now)
		if !ok {
			continue
		}
		found = append(found, foundResult{t.ContextID, t.TaskID, t.Session, path, text, jobID})
	}
	if len(found) == 0 {
		return 0, nil
	}
	// 測試用的同步點：production 永遠是 nil，呼叫零成本。stage 1 完全不碰
	// tasksMu，所以「掃描找到 m1、還沒進 stage 2 之前，同一個 row 的
	// LastMessageID 被 DeliverFollowUp 換成 m2」這個窗口，沒有任何鎖可以讓
	// 測試掛上去強制排序——用真的 goroutine 賭時序，會變成賭 scheduler。
	// 這個 hook 就是那個窗口唯一站得住的同步點（見
	// TestCollectResultsRejectsResultForASupersededMessage）。
	if collectResultsAfterScanForTest != nil {
		collectResultsAfterScanForTest()
	}

	// --- 第 2 段：鎖內，純記憶體。 ---
	var promoted []foundResult
	err = WithTasks(root, func(tasks *TaskStore) error {
		for _, f := range found {
			cur, ok := tasks.ByContext(f.contextID)
			// 身分必須沒變：掃描與這裡之間，同一個 contextId 可能已被合法地
			// 重新提交成另一個任務。
			if !ok || cur.TaskID != f.taskID || cur.Session != f.session {
				continue
			}
			if !CanTransition(cur.State, TaskCompleted) {
				continue
			}
			// round 10 review, Important（D10-1）：重新比對，不是重新信任。
			// 掃描（第 1 段）當時 f.path 通過的是 cur 那時候的
			// LastMessageID；DeliverFollowUp 可能在掃描之後、上鎖之前，把
			// 同一個 contextId 的 LastMessageID 從舊訊息換成新訊息（同一個
			// TaskID、同一個 Session，三個身分欄位完全沒變，卻已經是另一個
			// 問題的回覆窗口了）。只用 job_id（不重新讀檔——job_id 在第 1
			// 段就已經解出來，這裡純粹是記憶體比對）對 cur 當下最新的
			// LastMessageID 重跑一次 resultBelongsToTask；比對不過，代表這
			// 份結果屬於一個已經被追問取代的舊訊息，放著不動、不搬檔、不
			// completed——它的真正主人（那則舊訊息本身）已經不會再被任何
			// row 認領，而這個 contextId 現在等的是新訊息的回覆，那份回覆
			// 之後會用新的 LastMessageID 通過這裡的比對。
			// round 10 review 第二輪，記錄不修：這裡 continue 之後，f.path
			// 這份「屬於被追問取代的舊訊息」的檔案不會被隔離、也不會被搬
			// 走——它會一直留在 outbox/pending 裡，每一輪 CollectResults
			// 都會重新 ReadJSON 一次它（直到整個沙盒被 sweep 回收），跟被
			// 隔離的壞檔不同。成本是每個被取代的追問各留一份檔案，接受這
			// 個限制：它不會累積到跨 session，也不會讓任何 row 卡住。
			if !resultBelongsToTask(OutputJob{JobID: f.jobID}, cur) {
				continue
			}
			cur.State = TaskCompleted
			cur.Detail = f.text
			cur.CompletedAt = now.UTC().Format(time.RFC3339)
			tasks.Upsert(cur)
			promoted = append(promoted, f)
		}
		if len(promoted) == 0 {
			return errNothingCollected
		}
		return nil
	})
	if errors.Is(err, errNothingCollected) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	// --- 第 3 段：鎖後搬檔。失敗只 log，不回退已完成的判定。 ---
	for _, f := range promoted {
		sentPath := filepath.Join(pathIn(SandboxRoot(root, f.session), "outbox", "sent"), filepath.Base(f.path))
		if mErr := moveFile(f.path, sentPath); mErr != nil {
			log.Printf("a2a: task %s completed but moving result file %s to %s failed: %v", f.contextID, f.path, sentPath, mErr)
		}
	}
	return len(promoted), nil
}
