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
