package channelagent

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileLockRejectsSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock first: %v", err)
	}
	defer first.Release()

	second, err := AcquireLock(path)
	if err == nil {
		second.Release()
		t.Fatal("AcquireLock second succeeded while first lock is held")
	}
}

func TestFileLockReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock first: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	defer second.Release()
}

func TestAcquireLockStealsDeadHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	// Pre-create a lock owned by a PID that is not alive.
	if err := writeFileString(path, "2147483646\n"); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("should steal lock from dead holder, got: %v", err)
	}
	defer l.Release()
}

func TestAcquireLockStealsByAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	old := staleLockTimeout
	staleLockTimeout = 50 * time.Millisecond
	defer func() { staleLockTimeout = old }()

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Don't release; let it age past the (tiny) timeout.
	time.Sleep(80 * time.Millisecond)
	second, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("should steal aged lock even with live holder, got: %v", err)
	}
	defer second.Release()
	_ = first
}

func TestAcquireLockKeepsLiveRecentHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	defer first.Release()
	// Same process (alive) + recent → must NOT be stolen.
	if l, err := AcquireLock(path); err == nil {
		l.Release()
		t.Fatal("stole a live, recent lock")
	}
}

// TestStaleLockTimeoutFitsHeartbeatDesign: with the lock heartbeat (FileLock.Touch
// from waitOutput while the session is working), a working turn keeps its mtime
// fresh no matter how long it runs, so the age threshold no longer has to exceed
// the whole turn cap. It MUST still clear the worst-case NON-heartbeated hold — a
// cold-session Inject (waitSessionReady up to sessionBootDelay, retried a few
// times, ~4.5min worst case) — or such a boot would be stolen mid-flight
// (guards a re-run of the 2026-06-27 regression for the pre-waitOutput window).
func TestStaleLockTimeoutFitsHeartbeatDesign(t *testing.T) {
	const worstNonHeartbeatedHold = 5 * time.Minute
	if staleLockTimeout < worstNonHeartbeatedHold {
		t.Fatalf("staleLockTimeout (%s) must clear the worst-case non-heartbeated hold (%s)", staleLockTimeout, worstNonHeartbeatedHold)
	}
}

// TestLockHeartbeatSurvivesConcurrentAcquirers is the isolated validation of the
// 2026-06-27 safety property: a long-but-working turn that heartbeats its lock is
// NEVER stolen by concurrent acquirers, no matter how far it outruns the timeout;
// and once it STOPS heartbeating (hangs), the lock IS reclaimed after the timeout.
// Deterministic, no Claude session needed. Run under -race.
func TestLockHeartbeatSurvivesConcurrentAcquirers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	old := staleLockTimeout
	staleLockTimeout = 100 * time.Millisecond
	defer func() { staleLockTimeout = old }()

	held, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Phase 1: heartbeat every 25ms for 1s — 10× the 100ms timeout. A concurrent
	// acquirer hammering every 15ms must NEVER succeed (no mid-flight steal).
	stop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		tk := time.NewTicker(25 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				_ = held.Touch()
			}
		}
	}()

	var stolen int32
	robber := make(chan struct{})
	robberDone := make(chan struct{})
	go func() {
		defer close(robberDone)
		tk := time.NewTicker(15 * time.Millisecond)
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

	time.Sleep(1 * time.Second)
	if n := atomic.LoadInt32(&stolen); n != 0 {
		t.Fatalf("lock stolen %d times while heartbeating — mid-flight steal regression", n)
	}

	// Phase 2: stop the heartbeat (simulate the turn hanging). The lock must now
	// age out and become stealable within a few timeouts.
	close(stop)
	<-hbDone
	stole := false
	for i := 0; i < 40; i++ { // up to ~600ms >> 100ms timeout
		time.Sleep(15 * time.Millisecond)
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
		t.Fatal("hung lock (no heartbeat) was never reclaimed after the timeout")
	}
}

// TestFileLockTouchResetsAge proves the heartbeat: a lock aged past the timeout
// becomes non-stealable again after Touch, so a working turn that heartbeats is
// never age-stolen.
func TestFileLockTouchResetsAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	old := staleLockTimeout
	staleLockTimeout = 60 * time.Millisecond
	defer func() { staleLockTimeout = old }()

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	defer first.Release()
	time.Sleep(90 * time.Millisecond) // age past the (tiny) timeout
	if err := first.Touch(); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	// Freshly heartbeated → a second acquirer must NOT steal it.
	if l, err := AcquireLock(path); err == nil {
		l.Release()
		t.Fatal("stole a freshly-touched (heartbeated) lock")
	}
}

func writeFileString(path, s string) error {
	return AtomicWriteFile(path, []byte(s), 0o644)
}

// AcquireLock 先 O_EXCL 建檔、再寫 PID。中間那個瞬間別的行程讀到的是空檔，
// 若一律判成 corrupt 就會把一個活著的持有者剛拿到的鎖偷走——共用的
// registry.lock 上就是兩個 serve 同時改 bindings.json。
func TestAcquireLockDoesNotStealAHolderMidWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")

	// 模擬「已建立、PID 還沒寫」：空檔，mtime 是現在。
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := AcquireLock(path); err == nil {
		_ = l.Release()
		t.Fatal("stole a lock whose holder had not yet written its pid")
	}

	// 夠舊還是空的 → 真的壞掉，可以偷。
	old := time.Now().Add(-2 * lockWriteGrace)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("an aged empty lock must be stealable: %v", err)
	}
	_ = l.Release()
}
