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

// ResultFor reports the sandbox's reply text, if it has written one. Completion
// is detected by the same outbox-file convention every worker already uses —
// never by scraping the tmux pane.
func ResultFor(root string, task A2ATask) (string, bool) {
	_, text, ok := pendingResultFile(root, task)
	return text, ok
}

// resultBelongsToTask 比對結果檔是否真的來自這個任務最後一次注入的訊息。
// buildJobID（watcher.go:113）的格式是
// <sanitize(CreatedAt)>-<sanitize(MessageID)>-<inputHash[:12]>，所以
// sanitize(LastMessageID) 必定是 job_id 的子字串。
//
// LastMessageID 為空的 row 一律不接受任何結果檔：那是還沒注入過的任務，
// 不可能有結果，而寬鬆放行正是殘留檔完成新任務的那條路。
func resultBelongsToTask(job OutputJob, task A2ATask) bool {
	if task.LastMessageID == "" {
		return false
	}
	return strings.Contains(job.JobID, sanitize(task.LastMessageID))
}

// pendingResultFile locates the sandbox's result file in outbox/pending, if
// any, and returns its path alongside the decoded text. Kept separate from
// ResultFor so CollectResults can consume (move) the exact file it read,
// rather than re-scanning the directory and risking a different file.
func pendingResultFile(root string, task A2ATask) (path string, text string, ok bool) {
	dir := pathIn(SandboxRoot(root, task.Session), "outbox", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		var job OutputJob
		if err := ReadJSON(p, &job); err != nil {
			// 不再靜默 continue：一個壞檔會每 10 秒被重讀一次直到永遠。留一行
			// log 並搬去 outbox/failed（沿用 sender.go 的慣例）。
			log.Printf("a2a: 無法解讀結果檔 %s，移往 outbox/failed: %v", p, err)
			_ = moveFile(p, filepath.Join(pathIn(SandboxRoot(root, task.Session), "outbox", "failed"), e.Name()))
			continue
		}
		if !resultBelongsToTask(job, task) {
			continue
		}
		return p, job.Text, true
	}
	return "", "", false
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
		contextID, taskID, session, path, text string
	}
	var found []foundResult
	for _, t := range snapshot.Tasks {
		if !CanTransition(t.State, TaskCompleted) {
			continue
		}
		path, text, ok := pendingResultFile(root, t)
		if !ok {
			continue
		}
		found = append(found, foundResult{t.ContextID, t.TaskID, t.Session, path, text})
	}
	if len(found) == 0 {
		return 0, nil
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
