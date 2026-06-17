package channelagent

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type stubSource struct{ msgs []SourceMessage }

func (s stubSource) Fetch(_ context.Context) ([]SourceMessage, error) { return s.msgs, nil }

type capSender struct{ sent []string }

func (c *capSender) Send(_ context.Context, o OutputJob) error {
	c.sent = append(c.sent, o.Text)
	return nil
}

func TestRunControlOnceExecutesCommandAndReplies(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	src := stubSource{msgs: []SourceMessage{{
		Platform: "discord", ChannelID: "ctl", MessageID: "m1",
		AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z",
		Content: "/bind proj-a " + t.TempDir() + " ticket-1",
	}}}
	sender := &capSender{}

	if err := RunControlOnce(context.Background(), root, ControlBinding(root).Root, deps, &reg, src, sender, ControlPlane{}); err != nil {
		t.Fatalf("RunControlOnce: %v", err)
	}
	if _, ok := reg.Get("proj-a"); !ok {
		t.Fatal("binding not created")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(sender.sent))
	}

	sender2 := &capSender{}
	reg2, _ := LoadRegistry(root)
	if err := RunControlOnce(context.Background(), root, ControlBinding(root).Root, deps, &reg2, src, sender2, ControlPlane{}); err != nil {
		t.Fatalf("RunControlOnce 2: %v", err)
	}
	if len(sender2.sent) != 0 {
		t.Fatalf("message reprocessed, sent=%d", len(sender2.sent))
	}
}

func TestRunControlAssistantProcessesQueuedJob(t *testing.T) {
	oldRun := runExternalCommand
	defer func() { runExternalCommand = oldRun }()
	runExternalCommand = func(_ context.Context, _ string, _ ...string) error { return nil }

	root := filepath.Join(t.TempDir(), ".channel-agent")
	controlRoot := ControlBinding(root).Root
	if err := Init(controlRoot); err != nil {
		t.Fatalf("Init: %v", err)
	}
	msg := SourceMessage{Platform: "discord", ChannelID: "ctl", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z", Content: "hi"}
	if err := enqueueControlJob(controlRoot, msg); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	injector := fakeInjector{write: func(job InputJob, outputPath string) error {
		return AtomicWriteJSON(outputPath, OutputJob{Schema: 1, JobID: job.JobID, RequestID: job.RequestID, InputHash: job.InputHash, Send: true, Text: "你好"})
	}}
	sender := &capSender{}

	if err := runControlAssistant(context.Background(), ControlBinding(root), injector, sender, time.Second); err != nil {
		t.Fatalf("runControlAssistant: %v", err)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "你好" {
		t.Fatalf("expected reply 你好, got %#v", sender.sent)
	}
}
