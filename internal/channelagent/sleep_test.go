package channelagent

import (
	"os"
	"testing"
	"time"
)

func TestShouldSleep(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	// No activity signal at all → don't sleep (leave it alone).
	if shouldSleep(root, "/no/such/worktree", 30*time.Minute) {
		t.Fatal("no-activity binding should not sleep")
	}
	// A processed reply 1h ago, nothing pending → idle → sleep.
	sent := pathIn(root, "outbox", "sent", "j1.json")
	if err := AtomicWriteJSON(sent, OutputJob{Schema: 1, JobID: "j1", Send: true, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-1 * time.Hour)
	_ = os.Chtimes(sent, old, old)
	if !shouldSleep(root, "/no/such/worktree", 30*time.Minute) {
		t.Fatal("1h-idle binding should sleep")
	}
	// Recent activity → not idle.
	if shouldSleep(root, "/no/such/worktree", 2*time.Hour) {
		t.Fatal("within-timeout binding should not sleep")
	}
	// Queued input → never sleep even if idle.
	if err := AtomicWriteJSON(pathIn(root, "inbox", "pending", "p1.json"), InputJob{Schema: 1, JobID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if shouldSleep(root, "/no/such/worktree", 30*time.Minute) {
		t.Fatal("binding with queued input should not sleep")
	}
	// timeout<=0 disables.
	_ = os.Remove(pathIn(root, "inbox", "pending", "p1.json"))
	if shouldSleep(root, "/no/such/worktree", 0) {
		t.Fatal("timeout<=0 must disable sleep")
	}
}
