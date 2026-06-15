package channelagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitCreatesQueueDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")

	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}

	for _, dir := range []string{
		"mock",
		"inbox/pending",
		"inbox/processing",
		"inbox/done",
		"inbox/failed",
		"outbox/pending",
		"outbox/sent",
		"outbox/failed",
		"state",
		"locks",
		"logs",
	} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
}

func TestWatcherCreatesPendingJobsAndSkipsSeenMessages(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	sourcePath := filepath.Join(root, "mock", "source_messages.json")
	messages := []SourceMessage{
		{Platform: "mock", ChannelID: "local", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12+08:00", Content: "one"},
		{Platform: "mock", ChannelID: "local", MessageID: "m2", AuthorID: "u2", CreatedAt: "2026-06-16T01:31:12+08:00", Content: "two"},
	}

	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AtomicWriteJSON(sourcePath, messages); err != nil {
		t.Fatalf("write mock source: %v", err)
	}

	created, err := RunWatcher(root, sourcePath)
	if err != nil {
		t.Fatalf("RunWatcher first: %v", err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2", created)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 2 {
		t.Fatalf("pending jobs = %d, want 2", got)
	}

	created, err = RunWatcher(root, sourcePath)
	if err != nil {
		t.Fatalf("RunWatcher second: %v", err)
	}
	if created != 0 {
		t.Fatalf("created on second run = %d, want 0", created)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 2 {
		t.Fatalf("pending jobs after second run = %d, want 2", got)
	}
}

func countJSONFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count
}
