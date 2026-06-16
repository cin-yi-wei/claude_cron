package channelagent

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRunServeOnceProcessesAndSends(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	source := fakeSource{messages: []SourceMessage{{
		Platform: "mock", ChannelID: "local", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12Z", Content: "hi",
	}}}
	injector := fakeInjector{write: func(job InputJob, outputPath string) error {
		return AtomicWriteJSON(outputPath, OutputJob{
			Schema:    1,
			JobID:     job.JobID,
			RequestID: job.RequestID,
			InputHash: job.InputHash,
			Send:      true,
			Text:      "reply",
		})
	}}
	sender := &recordingSender{}

	result, err := RunServeOnce(context.Background(), root, PollIngester{Source: source}, injector, sender, time.Second)
	if err != nil {
		t.Fatalf("RunServeOnce: %v", err)
	}
	if result.Created != 1 || !result.Processed || result.Sent != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "reply" {
		t.Fatalf("sent = %#v", sender.sent)
	}
}
