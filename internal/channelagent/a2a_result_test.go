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

// 壞掉的結果檔不能每 10 秒被重讀一次直到永遠——但也不能一撞見就隔離
// （round 10 review, Important, D10-2）：寫入合約是先寫 .tmp 再 rename
// （adapters.go 的 BuildClaudePrompt），但那個合約只活在提示詞裡，不是程式
// 碼保證的；一個沒照合約走、直接寫最終檔名的 agent，可能剛好在被讀到的那
// 一刻還沒寫完。第一次撞見必須放過，只有超過寬限期還讀不動，才真的判定壞
// 掉並隔離。
func TestCollectResultsQuarantinesUnreadableResultFiles(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("a", "c1")
	sandbox := SandboxRoot(root, session)
	_ = Init(sandbox)
	brokenPath := pathIn(sandbox, "outbox", "pending", "broken.json")
	if err := os.WriteFile(brokenPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "a", Session: session, State: TaskWorking, LastMessageID: "m"})
	_ = SaveTasks(root, s)

	// 第一次撞見:檔案剛剛才寫(mtime = 現在),必須放過,不能立刻隔離——
	// 否則一份其實正在被寫、寫完就會完整的檔案,會被這一輪的誤判活活摧毀。
	_, _ = CollectResults(root, time.Now())
	if _, err := os.Stat(brokenPath); err != nil {
		t.Fatalf("a fresh unreadable result file must survive the first pass (not be quarantined immediately): %v", err)
	}
	if _, err := os.Stat(pathIn(sandbox, "outbox", "failed", "broken.json")); err == nil {
		t.Fatal("must not quarantine an unreadable result file on first sight")
	}

	// 把 mtime 往回調到寬限期之前,模擬「這份檔案已經壞掉很久了,不是正在寫」。
	old := time.Now().Add(-unreadableResultGrace - time.Second)
	if err := os.Chtimes(brokenPath, old, old); err != nil {
		t.Fatal(err)
	}

	_, _ = CollectResults(root, time.Now())
	if _, err := os.Stat(pathIn(sandbox, "outbox", "failed", "broken.json")); err != nil {
		t.Fatalf("a genuinely broken result file past the grace period must be moved to outbox/failed: %v", err)
	}
	if _, err := os.Stat(brokenPath); !os.IsNotExist(err) {
		t.Fatalf("quarantined file must be gone from pending, stat err = %v", err)
	}
}

// TestCollectResultsRejectsResultForASupersededMessage pins the D10-1 fix
// (round 10 review, Important): CollectResults's lock-free scan (stage 1)
// can match a result file against the row's LastMessageID as it stood at
// scan time, but DeliverFollowUp can advance that same row's LastMessageID
// in place (same TaskID, same Session — a genuine follow-up, not a
// resubmission) before stage 2 ever reacquires tasksMu to apply what stage 1
// found. Without re-running resultBelongsToTask under the lock against the
// row's CURRENT LastMessageID, the stale match would be applied: the caller
// would receive the answer to the message it already superseded, and the
// real answer to the one it actually asked would land on a now-terminal row
// and never be collectable again.
//
// The window between stage 1 and stage 2 has no lock a test could hold to
// force this interleaving deterministically — stage 1 never touches
// tasksMu, so a real goroutine racing an unsynchronized mutation against it
// would itself be a timing gamble (nothing blocks stage 1 relative to that
// mutation; whichever the scheduler happens to run first decides the
// outcome). Instead this uses collectResultsAfterScanForTest, a seam that
// runs synchronously between stage 1 and stage 2 inside the SAME call to
// CollectResults, no goroutine involved: by the time it fires, stage 1 has
// deterministically already matched the m1 file against LastMessageID="m1"
// (that's the only way `found` becomes non-empty and this hook ever gets
// called at all); the hook then advances the row to LastMessageID="m2" —
// same TaskID, same Session, exactly what DeliverFollowUp does in place —
// before stage 2 gets to run.
func TestCollectResultsRejectsResultForASupersededMessage(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("a", "c1")
	sandbox := SandboxRoot(root, session)
	if err := Init(sandbox); err != nil {
		t.Fatal(err)
	}

	// m1 的結果先出現——這是 CollectResults 鎖外掃描（stage 1）會找到並通過
	// 比對的那一份。
	if err := AtomicWriteJSON(pathIn(sandbox, "outbox", "pending", "r1.json"), OutputJob{
		Schema: 1, JobID: "20260101T000000Z-m1-abcdef012345", Send: true, Text: "answer-to-m1",
	}); err != nil {
		t.Fatal(err)
	}
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "a", Session: session, State: TaskWorking, LastMessageID: "m1"})
	if err := SaveTasks(root, s); err != nil {
		t.Fatal(err)
	}

	hookRan := false
	collectResultsAfterScanForTest = func() {
		hookRan = true
		// 模擬 DeliverFollowUp 把同一個 row 的 LastMessageID 從 m1 換成
		// m2:TaskID、Session 都沒變。這一刻 stage 1 已經確定跑完(否則這
		// 個 hook 不會被呼叫),stage 2 還沒開始——這正是要測的窗口。
		advanced, err := LoadTasks(root)
		if err != nil {
			t.Fatal(err)
		}
		cur, ok := advanced.ByContext("c1")
		if !ok {
			t.Fatal("task c1 missing")
		}
		cur.LastMessageID = "m2"
		advanced.Upsert(cur)
		if err := SaveTasks(root, advanced); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { collectResultsAfterScanForTest = nil }()

	n, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	if !hookRan {
		t.Fatal("test hook never ran — stage 1 did not find the m1 match, this test proves nothing")
	}
	if n != 0 {
		t.Fatalf("promoted = %d, want 0: a result for the superseded message m1 must not complete a task now waiting on m2", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state = %s, want working: m1's stale result must not have completed the task", tk.State)
	}
	if tk.Detail == "answer-to-m1" {
		t.Fatal("task.Detail was overwritten with the superseded message's answer")
	}
	if _, err := os.Stat(pathIn(sandbox, "outbox", "pending", "r1.json")); err != nil {
		t.Fatalf("m1's result file must stay in pending, unconsumed: %v", err)
	}

	// m2 的真正回覆後來才出現，必須仍然收得到——不能因為上面那次誤判就永
	// 遠卡住。
	collectResultsAfterScanForTest = nil // 第二輪不再需要這個 hook
	if err := AtomicWriteJSON(pathIn(sandbox, "outbox", "pending", "r2.json"), OutputJob{
		Schema: 1, JobID: "20260101T000100Z-m2-abcdef012345", Send: true, Text: "answer-to-m2",
	}); err != nil {
		t.Fatal(err)
	}
	n, err = CollectResults(root, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("second collect = %d err = %v, want 1", n, err)
	}
	got, _ = LoadTasks(root)
	tk, _ = got.ByContext("c1")
	if tk.State != TaskCompleted || tk.Detail != "answer-to-m2" {
		t.Fatalf("task = %#v, want completed with m2's answer", tk)
	}
}

// TestCollectResultsScanRunsWithoutTasksMu proves the actual mechanism the
// three-stage split exists for, not just an outward symptom of it: stage 1's
// directory scan and quarantine of an unreadable result file need no lock at
// all. tasksMu is held for the whole test (any WithTasks call anywhere would
// stall on it for as long as the test wants); if the split were ever
// reverted back into a single WithTasks callback — even while keeping every
// other change in this task, including LastMessageID — the quarantine below
// would itself be stuck behind that same lock and this test would time out
// waiting for it.
func TestCollectResultsScanRunsWithoutTasksMu(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("a", "c1")
	sandbox := SandboxRoot(root, session)
	if err := Init(sandbox); err != nil {
		t.Fatal(err)
	}
	brokenPath := pathIn(sandbox, "outbox", "pending", "broken.json")
	if err := os.WriteFile(brokenPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-unreadableResultGrace - time.Second)
	if err := os.Chtimes(brokenPath, old, old); err != nil {
		t.Fatal(err)
	}
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "a", Session: session, State: TaskWorking, LastMessageID: "m"})
	if err := SaveTasks(root, s); err != nil {
		t.Fatal(err)
	}

	tasksMu.Lock()
	done := make(chan struct{})
	go func() {
		_, _ = CollectResults(root, time.Now())
		close(done)
	}()

	failedPath := pathIn(sandbox, "outbox", "failed", "broken.json")
	deadline := time.Now().Add(2 * time.Second)
	quarantined := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(failedPath); err == nil {
			quarantined = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 不論上面等到還是逾時都先放手、等 goroutine 收尾，絕不留一個還在跑、
	// 之後可能跟下一個測試搶同一個 package 級 tasksMu 的殘留 goroutine。
	tasksMu.Unlock()
	<-done

	if !quarantined {
		t.Fatal("stage 1's quarantine never happened while tasksMu was held externally by this test — some I/O must still be running inside the lock")
	}
}
