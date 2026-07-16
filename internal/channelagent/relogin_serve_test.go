package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pasterInjector is a fakeInjector that also satisfies loginPaster, so the serve
// wiring (ResolvePendingReloginOnce) can type the code.
type pasterInjector struct {
	fakeInjector
	pasted string
}

func (p *pasterInjector) PasteLoginCode(_ context.Context, code string) error {
	p.pasted = code
	return nil
}

// End-to-end through RunServeOnce (the real serve.go wiring): a pending re-login
// + a `code:` reply in the inbox → the code is typed into the session out-of-band
// and the reply is consumed BEFORE the normal worker turn (never injected as a
// prompt). Proves serve.go's ResolvePendingReloginOnce hookup, not just the unit.
func TestServeConsumesReloginCode(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	// A re-login is pending (as the supervisor would have recorded it).
	recordReloginRequest(root, "relogin-serve-1", "https://claude.ai/oauth/authorize?s=1")

	// The user's reply carrying the code, already in the inbox.
	code := "authcode_9#state"
	job := InputJob{Schema: 1, JobID: "c1", RequestID: "r", InputHash: "h",
		Source: SourceMessage{Platform: "mock", ChannelID: "local", Content: "code: " + code}}
	if err := AtomicWriteJSON(pathIn(root, "inbox", "pending", "c1.json"), job); err != nil {
		t.Fatal(err)
	}

	inj := &pasterInjector{fakeInjector: fakeInjector{write: func(j InputJob, out string) error {
		// If this fires, the code reply was wrongly treated as a normal turn.
		t.Errorf("worker injected the code reply as a prompt (job %s) — should have been consumed out-of-band", j.JobID)
		return AtomicWriteJSON(out, OutputJob{Schema: 1, JobID: j.JobID, RequestID: j.RequestID, InputHash: j.InputHash, Send: true, Text: "x"})
	}}}

	_, err := RunServeOnce(context.Background(), root, PollIngester{Source: fakeSource{}}, inj, &recordingSender{}, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("RunServeOnce: %v", err)
	}
	if inj.pasted != code {
		t.Fatalf("code not pasted via serve wiring: got %q want %q", inj.pasted, code)
	}
	if hasPendingRelogin(root) {
		t.Error("pending re-login should be cleared after the code was pasted")
	}
	if _, err := os.ReadFile(filepath.Join(root, "inbox", "done", "c1.json")); err != nil {
		t.Errorf("code reply should be archived to inbox/done: %v", err)
	}
}
