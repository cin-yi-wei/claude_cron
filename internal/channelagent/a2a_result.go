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
			continue
		}
		return p, job.Text, true
	}
	return "", "", false
}

// CollectResults promotes working tasks to completed when their sandbox has
// produced a result. Returns how many tasks were completed.
//
// Session names are deterministic (SessionNameFor), and a contextId whose
// previous task reached a terminal state may later be reused by a different
// caller — mapping to the same SandboxRoot, where the earlier task's result
// file may still sit in outbox/pending. To keep that stale file from ever
// satisfying a later task under the same session, every result file is moved
// out of pending (into outbox/sent, mirroring sender.go's convention) the
// moment it completes the task it belongs to.
func CollectResults(root string, now time.Time) (int, error) {
	n := 0
	err := WithTasks(root, func(tasks *TaskStore) error {
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			if !CanTransition(t.State, TaskCompleted) {
				continue
			}
			path, text, ok := pendingResultFile(root, t)
			if !ok {
				continue
			}
			t.State = TaskCompleted
			t.Detail = text
			t.CompletedAt = now.UTC().Format(time.RFC3339)
			tasks.Tasks[i] = t
			n++

			// The task genuinely completed; a failure to relocate the result
			// file must not undo or block that. Log it rather than erroring.
			sentPath := filepath.Join(pathIn(SandboxRoot(root, t.Session), "outbox", "sent"), filepath.Base(path))
			if err := moveFile(path, sentPath); err != nil {
				log.Printf("a2a: task %s completed but moving result file %s to %s failed: %v", t.ContextID, path, sentPath, err)
			}
		}
		if n == 0 {
			return errNothingCollected
		}
		return nil
	})
	if errors.Is(err, errNothingCollected) {
		return 0, nil
	}
	return n, err
}
