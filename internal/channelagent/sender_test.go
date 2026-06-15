package channelagent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type recordingSender struct {
	fail bool
	sent []OutputJob
}

func (s *recordingSender) Send(_ context.Context, output OutputJob) error {
	if s.fail {
		return errors.New("send failed")
	}
	s.sent = append(s.sent, output)
	return nil
}

func TestSenderSendsUnsentOutputAndMovesToSent(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	output := seedOutputJob(t, root, "job-1", true)
	sender := &recordingSender{}

	sent, err := RunSenderOnce(context.Background(), root, sender)
	if err != nil {
		t.Fatalf("RunSenderOnce: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent count = %d, want 1", sent)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != output.Text {
		t.Fatalf("sender got %#v, want one output", sender.sent)
	}
	assertExists(t, filepath.Join(root, "outbox", "sent", output.JobID+".json"))
}

func TestSenderSkipsAlreadySentOutputHash(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	output := seedOutputJob(t, root, "job-1", true)
	hash, err := HashOutput(output)
	if err != nil {
		t.Fatalf("HashOutput: %v", err)
	}
	if err := writeOutHashState(filepath.Join(root, "state", "last_out_hashes.json"), outHashState{Hashes: map[string]string{output.JobID: hash}}); err != nil {
		t.Fatalf("writeOutHashState: %v", err)
	}
	sender := &recordingSender{}

	sent, err := RunSenderOnce(context.Background(), root, sender)
	if err != nil {
		t.Fatalf("RunSenderOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("sent count = %d, want 0", sent)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sender was called for duplicate hash: %#v", sender.sent)
	}
	assertExists(t, filepath.Join(root, "outbox", "sent", output.JobID+".json"))
}

func TestSenderDoesNotRecordHashWhenSendFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	output := seedOutputJob(t, root, "job-1", true)
	sender := &recordingSender{fail: true}

	_, err := RunSenderOnce(context.Background(), root, sender)
	if err == nil {
		t.Fatal("RunSenderOnce succeeded despite sender failure")
	}
	assertExists(t, filepath.Join(root, "outbox", "failed", output.JobID+".json"))

	state, err := readOutHashState(filepath.Join(root, "state", "last_out_hashes.json"))
	if err != nil {
		t.Fatalf("readOutHashState: %v", err)
	}
	if state.Hashes[output.JobID] != "" {
		t.Fatalf("hash recorded after failed send: %q", state.Hashes[output.JobID])
	}
}

func TestSenderTreatsSendFalseAsHandledWithoutCallingAdapter(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	output := seedOutputJob(t, root, "job-1", false)
	sender := &recordingSender{}

	sent, err := RunSenderOnce(context.Background(), root, sender)
	if err != nil {
		t.Fatalf("RunSenderOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("sent count = %d, want 0", sent)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sender called for send=false output: %#v", sender.sent)
	}
	assertExists(t, filepath.Join(root, "outbox", "sent", output.JobID+".json"))
}

func seedOutputJob(t *testing.T, root, jobID string, send bool) OutputJob {
	t.Helper()
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	output := OutputJob{
		Schema:    1,
		JobID:     jobID,
		RequestID: "req-1",
		InputHash: "hash-1",
		Send:      send,
		Text:      "reply",
	}
	if !send {
		output.Text = ""
	}
	if err := AtomicWriteJSON(filepath.Join(root, "outbox", "pending", jobID+".json"), output); err != nil {
		t.Fatalf("write output job: %v", err)
	}
	return output
}
