package channelagent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// idle pane (working=false) + no reply → bail with errStalled within ~stallWindow,
// NOT the full timeout. This is the hung-turn watchdog that releases claude.lock.
func TestWaitOutputStallsWhenIdleNoReply(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	start := time.Now()
	_, err := waitOutput(context.Background(), p, 30*time.Second, func() bool { return false }, time.Second)
	if !errors.Is(err, errStalled) {
		t.Fatalf("want errStalled, got %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("stall detection too slow: %v (should be ~stallWindow, not full timeout)", d)
	}
}

// working pane (spinner / confirm) must NEVER be cut short as a stall — a long
// legitimate turn keeps running. With working=true it should hit the ctx timeout,
// never errStalled.
func TestWaitOutputWorkingNeverStalls(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	_, err := waitOutput(context.Background(), p, 3*time.Second, func() bool { return true }, time.Second)
	if errors.Is(err, errStalled) {
		t.Fatal("a working turn must not be treated as stalled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ctx deadline, got %v", err)
	}
}

// a reply that lands wins even if the pane looks idle (reply-race): no false stall.
func TestWaitOutputReplyBeatsStall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = AtomicWriteJSON(p, OutputJob{Schema: 1, Send: true, Text: "hi"})
	}()
	out, err := waitOutput(context.Background(), p, 10*time.Second, func() bool { return false }, time.Second)
	if err != nil {
		t.Fatalf("reply should win, got %v", err)
	}
	if out.Text != "hi" {
		t.Fatalf("got %q", out.Text)
	}
}

// nil working fn = stall detection disabled → old full-timeout behavior preserved.
func TestWaitOutputNilWorkingDisablesStall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	_, err := waitOutput(context.Background(), p, 2*time.Second, nil, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("nil working should fall through to timeout, got %v", err)
	}
}
