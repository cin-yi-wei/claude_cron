package channelagent

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockSoakRealtime is a real-wall-clock soak of the lock heartbeat at the
// PRODUCTION timescale (the actual 8min staleLockTimeout), the isolated
// validation the user asked for. Gated behind CC_LOCK_SOAK=1 (runs ~17min) so it
// never slows the normal suite. It does NOT drive a real Claude session (a
// rendering session can't be launched from the control plane) — it exercises the
// exact AcquireLock/Touch mechanism the worker relies on, over real minutes.
//
//	CC_LOCK_SOAK=1 go test ./internal/channelagent/ -run TestLockSoakRealtime -timeout 25m -v
func TestLockSoakRealtime(t *testing.T) {
	if os.Getenv("CC_LOCK_SOAK") != "1" {
		t.Skip("set CC_LOCK_SOAK=1 to run the ~17min real-time lock soak")
	}
	path := filepath.Join(t.TempDir(), "agent.lock")
	// Use the real production timeout — no override.
	t.Logf("soak starting: staleLockTimeout=%s", staleLockTimeout)

	held, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Phase 1: heartbeat every 30s for 9min — PAST the 8min lease, proving a turn
	// that outruns the timeout is never stolen while it keeps working.
	stop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if err := held.Touch(); err != nil {
					t.Errorf("heartbeat Touch failed: %v", err)
				}
			}
		}
	}()

	var stolen int32
	robber := make(chan struct{})
	robberDone := make(chan struct{})
	go func() {
		defer close(robberDone)
		tk := time.NewTicker(10 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-robber:
				return
			case <-tk.C:
				if l, err := AcquireLock(path); err == nil {
					atomic.AddInt32(&stolen, 1)
					l.Release()
				}
			}
		}
	}()

	time.Sleep(9 * time.Minute)
	if n := atomic.LoadInt32(&stolen); n != 0 {
		t.Fatalf("lock stolen %d times during the 9min heartbeated hold — mid-flight steal at prod scale", n)
	}
	t.Logf("phase 1 ok: survived 9min (>8min lease) with 0 steals")

	// Phase 2: stop heartbeating (turn hangs); confirm reclaim within ~1 lease + margin.
	close(stop)
	<-hbDone
	deadline := time.Now().Add(staleLockTimeout + 2*time.Minute)
	stole := false
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Second)
		if l, err := AcquireLock(path); err == nil {
			l.Release()
			stole = true
			break
		}
	}
	close(robber)
	<-robberDone
	_ = held
	if !stole {
		t.Fatalf("hung lock not reclaimed within %s after heartbeat stopped", staleLockTimeout+2*time.Minute)
	}
	t.Logf("phase 2 ok: hung lock reclaimed after heartbeat stopped")
}
