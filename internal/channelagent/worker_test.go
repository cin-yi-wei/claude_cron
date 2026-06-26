package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeInjector struct {
	write func(job InputJob, outputPath string) error
}

func (f fakeInjector) Inject(_ context.Context, job InputJob, outputPath string) error {
	if f.write == nil {
		return nil
	}
	return f.write(job, outputPath)
}

// glitchInjector writes no reply (simulating a glitched turn) and reports
// glitched=true, so a no-reply timeout should requeue instead of failing.
type glitchInjector struct{ glitched bool }

func (g glitchInjector) Inject(context.Context, InputJob, string) error { return nil }
func (g glitchInjector) LooksGlitched(context.Context) bool             { return g.glitched }

func TestWorkerRequeuesOnGlitchTimeout(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	job := seedPendingJob(t, root, "m1")

	// No reply + glitched session → job goes back to pending (Attempt incremented),
	// NOT to failed.
	_, err := RunWorkerOnce(context.Background(), root, glitchInjector{glitched: true}, 80*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	assertExists(t, filepath.Join(root, "inbox", "pending", job.JobID+".json"))
	assertNotExists(t, filepath.Join(root, "inbox", "failed", job.JobID+".json"))
}

func TestWorkerFailsOnNonGlitchTimeout(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	job := seedPendingJob(t, root, "m1")

	// No reply + NOT glitched (e.g. a long-running task) → failed as before.
	_, err := RunWorkerOnce(context.Background(), root, glitchInjector{glitched: false}, 80*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	assertExists(t, filepath.Join(root, "inbox", "failed", job.JobID+".json"))
}

func TestWorkerAcceptsValidOutputAndMovesInputToDone(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	job := seedPendingJob(t, root, "m1")

	processed, err := RunWorkerOnce(context.Background(), root, fakeInjector{
		write: func(job InputJob, outputPath string) error {
			return AtomicWriteJSON(outputPath, OutputJob{
				Schema:    1,
				JobID:     job.JobID,
				RequestID: job.RequestID,
				InputHash: job.InputHash,
				Send:      true,
				Text:      "reply",
			})
		},
	}, time.Second)
	if err != nil {
		t.Fatalf("RunWorkerOnce: %v", err)
	}
	if !processed {
		t.Fatal("processed = false, want true")
	}
	assertExists(t, filepath.Join(root, "inbox", "done", job.JobID+".json"))
	assertExists(t, filepath.Join(root, "outbox", "pending", job.JobID+".json"))
	assertNotExists(t, filepath.Join(root, "inbox", "processing", job.JobID+".json"))
}

func TestWorkerRejectsMismatchedOutputAndMovesInputToFailed(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	job := seedPendingJob(t, root, "m1")

	processed, err := RunWorkerOnce(context.Background(), root, fakeInjector{
		write: func(job InputJob, outputPath string) error {
			return AtomicWriteJSON(outputPath, OutputJob{
				Schema:    1,
				JobID:     job.JobID,
				RequestID: job.RequestID,
				InputHash: "wrong",
				Send:      true,
				Text:      "reply",
			})
		},
	}, time.Second)
	if err == nil {
		t.Fatal("RunWorkerOnce succeeded with mismatched input hash")
	}
	if !processed {
		t.Fatal("processed = false, want true")
	}
	assertExists(t, filepath.Join(root, "inbox", "failed", job.JobID+".json"))
	assertNotExists(t, filepath.Join(root, "inbox", "done", job.JobID+".json"))
}

func TestWorkerRecoversOrphanedProcessingJob(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	job := seedPendingJob(t, root, "m1")
	// Simulate a worker killed mid-job: the job is left in processing/.
	if err := moveFile(
		filepath.Join(root, "inbox", "pending", job.JobID+".json"),
		filepath.Join(root, "inbox", "processing", job.JobID+".json"),
	); err != nil {
		t.Fatalf("seed processing: %v", err)
	}

	processed, err := RunWorkerOnce(context.Background(), root, fakeInjector{
		write: func(job InputJob, outputPath string) error {
			return AtomicWriteJSON(outputPath, OutputJob{
				Schema:    1,
				JobID:     job.JobID,
				RequestID: job.RequestID,
				InputHash: job.InputHash,
				Send:      true,
				Text:      "reply",
			})
		},
	}, time.Second)
	if err != nil {
		t.Fatalf("RunWorkerOnce: %v", err)
	}
	if !processed {
		t.Fatal("processed = false, want true (orphan should be requeued and processed)")
	}
	assertExists(t, filepath.Join(root, "inbox", "done", job.JobID+".json"))
}

// TestResolvePendingDecisionWithoutLock proves the deadlock fix: a y/n reply is
// resolved while claude.lock is held (simulating the turn blocked in the gate
// hook). The decision file must be written, the pending request cleared, and the
// reply archived — none of which the lock-holding worker side-route could do.
func TestResolvePendingDecisionWithoutLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// A permission is pending (a turn is blocked in the gate hook).
	const permID = "mcp__openobserve__search_sql-20260623T074512000"
	if err := AtomicWriteJSON(filepath.Join(root, "permissions", "pending", permID+".json"),
		map[string]string{"id": permID, "tool": "mcp__openobserve__search_sql"}); err != nil {
		t.Fatalf("seed pending perm: %v", err)
	}
	// Hold claude.lock the whole time — exactly the deadlock condition.
	lock, err := AcquireLock(filepath.Join(root, "locks", "claude.lock"))
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer lock.Release()

	job := seedDecisionJob(t, root, "y")

	consumed, err := ResolvePendingDecisionOnce(root)
	if err != nil {
		t.Fatalf("ResolvePendingDecisionOnce: %v", err)
	}
	if !consumed {
		t.Fatal("consumed = false, want true (the y reply should resolve the pending perm)")
	}
	// Decision recorded so the gate hook unblocks with allow.
	var d struct {
		Allow    bool `json:"allow"`
		Remember bool `json:"remember"`
	}
	if err := ReadJSON(filepath.Join(root, "permissions", "decisions", permID+".json"), &d); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if !d.Allow {
		t.Fatalf("decision allow = false, want true")
	}
	// Pending request cleared + reply archived to done (not re-injected later).
	assertNotExists(t, filepath.Join(root, "permissions", "pending", permID+".json"))
	assertExists(t, filepath.Join(root, "inbox", "done", job.JobID+".json"))
	assertNotExists(t, filepath.Join(root, "inbox", "pending", job.JobID+".json"))
}

// TestResolvePendingDecisionScansBehindNonDecision proves the fix for the betby
// deadlock: the y/n reply is found even when a non-decision message (e.g. a
// pasted command) sits AHEAD of it in the queue.
func TestResolvePendingDecisionScansBehindNonDecision(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const permID = "Bash-20260626T100650334"
	if err := AtomicWriteJSON(filepath.Join(root, "permissions", "pending", permID+".json"),
		map[string]string{"id": permID, "tool": "Bash"}); err != nil {
		t.Fatalf("seed pending perm: %v", err)
	}
	// Oldest is a non-decision paste; the "y" is queued behind it.
	paste := seedDecisionJob(t, root, "echo hello")
	yes := seedDecisionJob(t, root, "y")
	// Ensure ordering by name: seedDecisionJob keys MessageID off content, and
	// sortedJSON sorts by JobID. Verify the y is resolved regardless of order.

	consumed, err := ResolvePendingDecisionOnce(root)
	if err != nil {
		t.Fatalf("ResolvePendingDecisionOnce: %v", err)
	}
	if !consumed {
		t.Fatal("consumed = false, want true (y behind a paste must still resolve)")
	}
	var d struct {
		Allow bool `json:"allow"`
	}
	if err := ReadJSON(filepath.Join(root, "permissions", "decisions", permID+".json"), &d); err != nil || !d.Allow {
		t.Fatalf("decision not allow: %v %+v", err, d)
	}
	// The y is consumed (archived); the non-decision paste stays in pending.
	assertExists(t, filepath.Join(root, "inbox", "done", yes.JobID+".json"))
	assertExists(t, filepath.Join(root, "inbox", "pending", paste.JobID+".json"))
}

// TestResolvePendingDecisionIgnoresNonDecision leaves a normal message for the
// worker when a permission is pending but the message is not a y/n.
func TestResolvePendingDecisionIgnoresNonDecision(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const permID = "Bash-20260623T074512000"
	if err := AtomicWriteJSON(filepath.Join(root, "permissions", "pending", permID+".json"),
		map[string]string{"id": permID, "tool": "Bash"}); err != nil {
		t.Fatalf("seed pending perm: %v", err)
	}
	job := seedDecisionJob(t, root, "do the thing please")
	consumed, err := ResolvePendingDecisionOnce(root)
	if err != nil {
		t.Fatalf("ResolvePendingDecisionOnce: %v", err)
	}
	if consumed {
		t.Fatal("consumed = true, want false (a non-decision message must be left for the worker)")
	}
	assertExists(t, filepath.Join(root, "inbox", "pending", job.JobID+".json"))
}

// seedDecisionJob writes a pending inbox message with the given content.
func seedDecisionJob(t *testing.T, root, content string) InputJob {
	t.Helper()
	source := SourceMessage{
		Platform:  "mock",
		ChannelID: "local",
		MessageID: "m-" + content,
		AuthorID:  "u1",
		CreatedAt: "2026-06-23T15:45:12+08:00",
		Content:   content,
	}
	hash, err := HashSource(source)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	job := InputJob{
		Schema:    1,
		JobID:     buildJobID(source, hash),
		RequestID: buildRequestID(source, hash),
		InputHash: hash,
		Source:    source,
		CreatedAt: source.CreatedAt,
	}
	if err := AtomicWriteJSON(filepath.Join(root, "inbox", "pending", job.JobID+".json"), job); err != nil {
		t.Fatalf("write decision job: %v", err)
	}
	return job
}

func seedPendingJob(t *testing.T, root, messageID string) InputJob {
	t.Helper()
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	source := SourceMessage{
		Platform:  "mock",
		ChannelID: "local",
		MessageID: messageID,
		AuthorID:  "u1",
		CreatedAt: "2026-06-16T01:30:12+08:00",
		Content:   "hello",
	}
	hash, err := HashSource(source)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	job := InputJob{
		Schema:    1,
		JobID:     buildJobID(source, hash),
		RequestID: buildRequestID(source, hash),
		InputHash: hash,
		Source:    source,
		CreatedAt: source.CreatedAt,
	}
	if err := AtomicWriteJSON(filepath.Join(root, "inbox", "pending", job.JobID+".json"), job); err != nil {
		t.Fatalf("write pending job: %v", err)
	}
	return job
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist, stat err=%v", path, err)
	}
}

func TestRequeueOrFail(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	name := "j.json"
	proc := pathIn(root, "inbox", "processing", name)
	// Attempt 0 → requeued (attempt becomes 1, < 3).
	if err := AtomicWriteJSON(proc, InputJob{Schema: 1, JobID: "j", Attempt: 0}); err != nil {
		t.Fatal(err)
	}
	requeueOrFail(root, proc, name, InputJob{Schema: 1, JobID: "j", Attempt: 0})
	if countJSON(pathIn(root, "inbox", "pending")) != 1 || countJSON(pathIn(root, "inbox", "processing")) != 0 {
		t.Fatal("attempt 0 should requeue to pending")
	}
	var rq InputJob
	_ = ReadJSON(pathIn(root, "inbox", "pending", name), &rq)
	if rq.Attempt != 1 {
		t.Fatalf("requeued attempt = %d, want 1", rq.Attempt)
	}
	// Attempt at cap-1 → failed.
	if err := AtomicWriteJSON(proc, InputJob{Schema: 1, JobID: "j"}); err != nil {
		t.Fatal(err)
	}
	requeueOrFail(root, proc, name, InputJob{Schema: 1, JobID: "j", Attempt: maxJobAttempts - 1})
	if countJSON(pathIn(root, "inbox", "failed")) != 1 {
		t.Fatal("exhausted attempts should move to failed")
	}
}
