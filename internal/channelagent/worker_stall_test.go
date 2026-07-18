package channelagent

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// idle pane (working=false) + no reply → bail with errStalled within ~stallWindow,
// NOT the full timeout. This is the hung-turn watchdog that releases claude.lock.
func TestWaitOutputStallsWhenIdleNoReply(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	start := time.Now()
	_, err := waitOutput(context.Background(), p, 30*time.Second, func() bool { return false }, time.Second, nil)
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
	_, err := waitOutput(context.Background(), p, 3*time.Second, func() bool { return true }, time.Second, nil)
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
	out, err := waitOutput(context.Background(), p, 10*time.Second, func() bool { return false }, time.Second, nil)
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
	_, err := waitOutput(context.Background(), p, 2*time.Second, nil, time.Second, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("nil working should fall through to timeout, got %v", err)
	}
}

// onProgress fires while the pane is working — the lock heartbeat. A working turn
// must get at least one heartbeat before the ctx timeout so its lock stays fresh.
func TestWaitOutputHeartbeatsWhileWorking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	var beats int32
	_, _ = waitOutput(context.Background(), p, 3*time.Second, func() bool { return true }, time.Second, func() {
		atomic.AddInt32(&beats, 1)
	})
	if atomic.LoadInt32(&beats) == 0 {
		t.Fatal("expected the lock heartbeat to fire while working, got 0")
	}
}
