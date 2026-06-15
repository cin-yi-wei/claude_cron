package channelagent

import (
	"context"
	"path/filepath"
	"testing"
)

type fakeSource struct {
	messages []SourceMessage
	err      error
}

func (s fakeSource) Fetch(context.Context) ([]SourceMessage, error) {
	return s.messages, s.err
}

func TestRunWatcherWithSourceCreatesJobs(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	source := fakeSource{messages: []SourceMessage{{
		Platform: "discord", ChannelID: "c1", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12Z", Content: "hi",
	}}}

	created, err := RunWatcherWithSource(context.Background(), root, source)
	if err != nil {
		t.Fatalf("RunWatcherWithSource: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
}

func TestMockFileSourceFetchesMessages(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	sourcePath := filepath.Join(root, "mock", "source_messages.json")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	want := []SourceMessage{{Platform: "mock", ChannelID: "local", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12Z", Content: "hi"}}
	if err := AtomicWriteJSON(sourcePath, want); err != nil {
		t.Fatalf("write source: %v", err)
	}

	got, err := MockFileSource{Path: sourcePath}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0].MessageID != "m1" {
		t.Fatalf("messages = %#v", got)
	}
}
