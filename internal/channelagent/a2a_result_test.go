package channelagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSandboxResult(t *testing.T, root, session, text string) {
	t.Helper()
	dir := pathIn(SandboxRoot(root, session), "outbox", "pending")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	job := OutputJob{Schema: 1, JobID: "r1", Send: true, Text: text}
	if err := AtomicWriteJSON(filepath.Join(dir, "r1.json"), job); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func TestCollectResultsCompletesTaskWhenResultAppears(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskWorking})
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	writeSandboxResult(t, root, session, "done reviewing")

	n, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	if n != 1 {
		t.Fatalf("collected = %d, want 1", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCompleted {
		t.Fatalf("state = %s, want completed", tk.State)
	}
	if tk.Detail != "done reviewing" {
		t.Fatalf("detail = %q", tk.Detail)
	}
	if tk.CompletedAt == "" {
		t.Fatal("CompletedAt not set")
	}
}

func TestCollectResultsIgnoresTaskWithoutResult(t *testing.T) {
	root := t.TempDir()
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskWorking})
	_ = SaveTasks(root, tasks)

	n, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	if n != 0 {
		t.Fatalf("collected = %d, want 0", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state changed to %s without a result", tk.State)
	}
}

func TestCollectResultsSkipsTerminalTasks(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskCompleted, Detail: "original"})
	_ = SaveTasks(root, tasks)
	writeSandboxResult(t, root, session, "late result")

	if _, err := CollectResults(root, time.Now()); err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.Detail != "original" {
		t.Fatalf("terminal task was overwritten: %q", tk.Detail)
	}
}
