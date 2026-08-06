package channelagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resultMsgID is the fixed LastMessageID every writeSandboxResult fixture's
// job_id is built from, so tasks that need CollectResults to actually accept
// their result set LastMessageID: resultMsgID.
const resultMsgID = "msg1"

func writeSandboxResult(t *testing.T, root, session, text string) {
	t.Helper()
	dir := pathIn(SandboxRoot(root, session), "outbox", "pending")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	job := OutputJob{Schema: 1, JobID: "20260101T000000Z-" + resultMsgID + "-abcdef012345", Send: true, Text: text}
	if err := AtomicWriteJSON(filepath.Join(dir, "r1.json"), job); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func TestCollectResultsCompletesTaskWhenResultAppears(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskWorking, LastMessageID: resultMsgID})
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
	pendingPath := filepath.Join(pathIn(SandboxRoot(root, session), "outbox", "pending"), "r1.json")

	if _, err := CollectResults(root, time.Now()); err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.Detail != "original" {
		t.Fatalf("terminal task was overwritten: %q", tk.Detail)
	}
	// The task was left untouched, so its outbox must be left untouched too:
	// the file must still be sitting in pending, not moved.
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("untouched task's outbox file was moved/removed: %v", err)
	}
}

// TestCollectResultsMovesConsumedResultFile pins the fix for a result file
// that is read but never consumed: every other outbox consumer in this
// codebase (sender.go) relocates a pending file after handling it, and
// CollectResults must do the same so a completed task's file can never be
// picked up again later.
func TestCollectResultsMovesConsumedResultFile(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskWorking, LastMessageID: resultMsgID})
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	writeSandboxResult(t, root, session, "done reviewing")

	pendingPath := filepath.Join(pathIn(SandboxRoot(root, session), "outbox", "pending"), "r1.json")
	sentPath := filepath.Join(pathIn(SandboxRoot(root, session), "outbox", "sent"), "r1.json")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("precondition: result file missing from pending: %v", err)
	}

	if _, err := CollectResults(root, time.Now()); err != nil {
		t.Fatalf("CollectResults: %v", err)
	}

	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("result file still present in pending (err=%v)", err)
	}
	if _, err := os.Stat(sentPath); err != nil {
		t.Fatalf("result file was not moved to outbox/sent: %v", err)
	}
}

// TestCollectResultsDoesNotResurrectOnContextReuse pins the actual
// vulnerability: session names are deterministic (SessionNameFor), and the
// server permits a different caller to reuse a contextId whose previous task
// reached a terminal state. That reused contextId maps to the same
// SandboxRoot. If the previous task's result file were left sitting in
// outbox/pending, CollectResults would see it immediately and mark the
// brand-new task completed — reporting success for work that never ran. This
// must fail against the pre-fix code (which reads but never moves the
// file) and pass once completion always relocates its result file.
func TestCollectResultsDoesNotResurrectOnContextReuse(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskWorking, LastMessageID: resultMsgID})
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	writeSandboxResult(t, root, session, "first task's result")

	// The first task completes normally.
	n1, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults (first): %v", err)
	}
	if n1 != 1 {
		t.Fatalf("first collect = %d, want 1", n1)
	}

	// The caller reuses contextId "c1" for a brand-new task. Upsert replaces
	// the now-terminal record; the new task maps to the exact same session
	// (and therefore the same SandboxRoot) because session names are
	// deterministic.
	reused, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	reused.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskWorking})
	if err := SaveTasks(root, reused); err != nil {
		t.Fatalf("SaveTasks (reuse): %v", err)
	}

	// The new task's sandbox has written nothing yet. If the first task's
	// result file is still sitting in pending (the bug), this call will
	// wrongly complete the new task off stale output.
	n2, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults (second): %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second collect = %d, want 0: new task must not complete off the first task's stale result", n2)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("reused-context task state = %s, want working (resurrected off a stale result file)", tk.State)
	}
}

// failed 沙盒依 forensics 規則保留，session 名又是 contextId 的確定性函式：
// 同一 caller 之後重用該 contextId 時，殘留在 outbox/pending 的舊結果檔會立刻
// 把新任務判為完成。
func TestCollectResultsIgnoresStaleResultFiles(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("a", "c1")
	sandbox := SandboxRoot(root, session)
	if err := Init(sandbox); err != nil {
		t.Fatal(err)
	}
	// 上一輪任務留下的結果檔，job_id 屬於一則早已不存在的訊息。
	if err := AtomicWriteJSON(pathIn(sandbox, "outbox", "pending", "old.json"), OutputJob{
		Schema: 1, JobID: "20260101T000000Z-stalemsg-abcdef012345", Send: true, Text: "stale answer",
	}); err != nil {
		t.Fatal(err)
	}

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t-new", Agent: "a", Session: session, State: TaskWorking,
		LastMessageID: session + "-1700000000000000000-7",
	})
	_ = SaveTasks(root, s)

	n, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	if n != 0 {
		t.Fatalf("promoted %d task(s) from a stale result file", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state = %q, want working", tk.State)
	}
}

func TestCollectResultsAcceptsItsOwnResultFile(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("a", "c1")
	sandbox := SandboxRoot(root, session)
	_ = Init(sandbox)

	msgID := session + "-1700000000000000000-7"
	if err := AtomicWriteJSON(pathIn(sandbox, "outbox", "pending", "mine.json"), OutputJob{
		Schema: 1, JobID: "20260806T101112Z-" + sanitize(msgID) + "-abcdef012345", Send: true, Text: "done",
	}); err != nil {
		t.Fatal(err)
	}
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", Session: session, State: TaskWorking,
		LastMessageID: msgID,
	})
	_ = SaveTasks(root, s)

	if n, err := CollectResults(root, time.Now()); err != nil || n != 1 {
		t.Fatalf("promoted = %d err = %v, want 1", n, err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCompleted || tk.Detail != "done" {
		t.Fatalf("task = %#v", tk)
	}
	// 搬檔仍然發生（下次不會再被讀到），但它在鎖外做。
	if _, err := os.Stat(pathIn(sandbox, "outbox", "pending", "mine.json")); err == nil {
		t.Fatal("the consumed result file must be moved out of pending")
	}
}

// 壞掉的結果檔不能每 10 秒被重讀一次直到永遠。
func TestCollectResultsQuarantinesUnreadableResultFiles(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("a", "c1")
	sandbox := SandboxRoot(root, session)
	_ = Init(sandbox)
	if err := os.WriteFile(pathIn(sandbox, "outbox", "pending", "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "a", Session: session, State: TaskWorking, LastMessageID: "m"})
	_ = SaveTasks(root, s)

	_, _ = CollectResults(root, time.Now())
	if _, err := os.Stat(pathIn(sandbox, "outbox", "failed", "broken.json")); err != nil {
		t.Fatalf("an unreadable result file must be moved to outbox/failed: %v", err)
	}
}
