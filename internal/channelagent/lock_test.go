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

func writeFileString(path, s string) error {
	return AtomicWriteFile(path, []byte(s), 0o644)
}
