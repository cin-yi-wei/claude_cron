package channelagent

import (
	"path/filepath"
	"testing"
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
