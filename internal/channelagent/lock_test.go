package channelagent

import (
	"path/filepath"
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
