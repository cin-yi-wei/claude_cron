package channelagent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type blockingIngester struct {
	started *int32
}

func (b blockingIngester) Run(ctx context.Context) error {
	atomic.AddInt32(b.started, 1)
	<-ctx.Done()
	return ctx.Err()
}

func TestPushManagerEnsureStartsOnce(t *testing.T) {
	m := NewPushManager(context.Background())
	defer m.StopAll()
	var started int32
	ing := blockingIngester{started: &started}

	m.Ensure("a", ing, nil)
	m.Ensure("a", ing, nil) // duplicate — must not start a second goroutine
	waitFor(t, func() bool { return atomic.LoadInt32(&started) == 1 })

	if !m.Running("a") {
		t.Fatal("a should be running")
	}
	if got := atomic.LoadInt32(&started); got != 1 {
		t.Fatalf("started = %d, want 1", got)
	}
}

func TestPushManagerReconcileStopsRemoved(t *testing.T) {
	m := NewPushManager(context.Background())
	defer m.StopAll()
	var s1, s2 int32
	m.Ensure("a", blockingIngester{started: &s1}, nil)
	m.Ensure("b", blockingIngester{started: &s2}, nil)
	waitFor(t, func() bool { return m.Running("a") && m.Running("b") })

	m.Reconcile(map[string]bool{"a": true}) // b removed
	waitFor(t, func() bool { return !m.Running("b") })

	if !m.Running("a") {
		t.Fatal("a should still run")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
