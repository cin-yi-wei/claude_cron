package channelagent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestPollIngesterWritesInbox(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	source := fakeSource{messages: []SourceMessage{{
		Platform: "discord", ChannelID: "c1", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12Z", Content: "hi",
	}}}

	var ingester Ingester = PollIngester{Source: source}
	created, err := ingester.Ingest(context.Background(), root)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
}

func TestPollIngesterPropagatesSourceError(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	want := errors.New("fetch boom")
	_, err := PollIngester{Source: fakeSource{err: want}}.Ingest(context.Background(), root)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
