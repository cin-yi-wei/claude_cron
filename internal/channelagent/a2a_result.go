package channelagent

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResultFor reports the sandbox's reply text, if it has written one. Completion
// is detected by the same outbox-file convention every worker already uses —
// never by scraping the tmux pane.
func ResultFor(root string, task A2ATask) (string, bool) {
	dir := pathIn(SandboxRoot(root, task.Session), "outbox", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var job OutputJob
		if err := ReadJSON(filepath.Join(dir, e.Name()), &job); err != nil {
			continue
		}
		return job.Text, true
	}
	return "", false
}

// CollectResults promotes working tasks to completed when their sandbox has
// produced a result. Returns how many tasks were completed.
func CollectResults(root string, now time.Time) (int, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range tasks.Tasks {
		t := tasks.Tasks[i]
		if !CanTransition(t.State, TaskCompleted) {
			continue
		}
		text, ok := ResultFor(root, t)
		if !ok {
			continue
		}
		t.State = TaskCompleted
		t.Detail = text
		t.CompletedAt = now.UTC().Format(time.RFC3339)
		tasks.Tasks[i] = t
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, SaveTasks(root, tasks)
}
