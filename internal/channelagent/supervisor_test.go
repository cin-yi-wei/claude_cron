package channelagent

import (
	"context"
	"path/filepath"
	"testing"
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

	if err := RunControlOnce(context.Background(), root, deps, &reg, src, sender); err != nil {
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
	if err := RunControlOnce(context.Background(), root, deps, &reg2, src, sender2); err != nil {
		t.Fatalf("RunControlOnce 2: %v", err)
	}
	if len(sender2.sent) != 0 {
		t.Fatalf("message reprocessed, sent=%d", len(sender2.sent))
	}
}
