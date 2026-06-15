package channelagent

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalQueueFlow(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	sourcePath := filepath.Join(root, "mock", "source_messages.json")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AtomicWriteJSON(sourcePath, []SourceMessage{{
		Platform: "mock", ChannelID: "local", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12+08:00", Content: "hi",
	}}); err != nil {
		t.Fatalf("write source: %v", err)
	}

	created, err := RunWatcher(root, sourcePath)
	if err != nil {
		t.Fatalf("RunWatcher: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}

	processed, err := RunWorkerOnce(context.Background(), root, fakeInjector{
		write: func(job InputJob, outputPath string) error {
			return AtomicWriteJSON(outputPath, OutputJob{
				Schema:    1,
				JobID:     job.JobID,
				RequestID: job.RequestID,
				InputHash: job.InputHash,
				Send:      true,
				Text:      "hello from claude",
			})
		},
	}, time.Second)
	if err != nil {
		t.Fatalf("RunWorkerOnce: %v", err)
	}
	if !processed {
		t.Fatal("processed = false, want true")
	}

	sender := &recordingSender{}
	sent, err := RunSenderOnce(context.Background(), root, sender)
	if err != nil {
		t.Fatalf("RunSenderOnce: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "hello from claude" {
		t.Fatalf("sent outputs = %#v", sender.sent)
	}
	if got := countJSONFiles(t, filepath.Join(root, "outbox", "sent")); got != 1 {
		t.Fatalf("sent outbox jobs = %d, want 1", got)
	}
}
