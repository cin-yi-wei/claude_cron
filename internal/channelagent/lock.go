package channelagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type FileLock struct {
	path string
	file *os.File
}

// staleLockTimeout bounds how long a lock may be held (by AGE, since its last
// heartbeat) before a new acquirer treats it as stale and steals it.
//
// The holder pid is always serve itself (always alive), so the pid-alive check
// below never reclaims a hung turn — only this age path does. Historically the
// lock mtime was written once at acquire and never refreshed, so a genuinely
// long-but-working turn and a hung turn were indistinguishable; the timeout had
// to exceed the whole turn cap (cfg.Claude.Timeout, 900s/15min) or a working turn
// got its lock STOLEN mid-flight — two goroutines then "held" the lock and on
// release one removed the other's file, the "held by live pid" churn (2026-06-27
// incident with a 5min timeout). That forced 20min, so a hung turn cost 20min.
//
// Now the holder HEARTBEATS the lock (FileLock.Touch) from waitOutput's stall
// ticker whenever the session is observed working — the exact signal waitOutput
// already uses to decide a stall. So a working turn refreshes its mtime every
// ~2s and its age stays near zero no matter how long it runs (never wrongly
// stolen), while a holder that stops making progress lets the mtime age out.
// This decouples "long" from "hung", so the timeout can be well under the turn
// cap. It is safe against the 2026-06-27 regression: any turn that survives today
// is working() during its whole run (waitOutput already errStalls a non-working
// turn in ~90s and releases the lock), so it heartbeats; this age path therefore
// only reclaims a holder wedged OUTSIDE waitOutput's coverage (e.g. stuck in a
// cold-session Inject), where 8min comfortably clears the worst-case ~4.5min
// cold-boot-retry Inject while still being far tighter than the old 20min. A dead
// holder pid is still reclaimed instantly below. Overridable in tests.
var staleLockTimeout = 8 * time.Minute

// AcquireLock creates an exclusive lock file at path. If the file already exists
// it is stolen when the previous holder is gone — either its PID is no longer
// alive, or the lock is older than staleLockTimeout (holder hung). Otherwise a
// live, recent holder yields an "acquire lock ... held by" error.
func AcquireLock(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		if stale, why := lockIsStale(path); stale {
			// Steal it: remove the stale file and recreate. A concurrent stealer
			// races here; whoever wins O_EXCL holds it, the loser errors cleanly.
			_ = os.Remove(path)
			file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		} else {
			return nil, fmt.Errorf("acquire lock %s: %s", path, why)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &FileLock{path: path, file: file}, nil
}

// lockIsStale reports whether an existing lock file can be stolen, with a reason
// string (used in the error when it is NOT stale). Stale when: the file is
// unreadable/corrupt, its holder PID is not alive, or it is older than
// staleLockTimeout.
func lockIsStale(path string) (bool, string) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		// Vanished between O_EXCL and here → treat as stealable (retry create).
		return true, "gone"
	}
	if time.Since(info.ModTime()) > staleLockTimeout {
		return true, "stale (age)"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true, "unreadable"
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return true, "corrupt pid"
	}
	if !processAlive(pid) {
		return true, "holder dead"
	}
	return false, fmt.Sprintf("held by live pid %d", pid)
}

// processAlive reports whether pid refers to a live process. Signal 0 probes
// existence without affecting the target; EPERM means alive but owned by another
// user, ESRCH means dead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// Touch refreshes the lock file's mtime to now — the heartbeat that keeps a
// working turn's lock from being age-stolen (see staleLockTimeout). No-op on a
// nil lock. A failure is returned but callers treat it as best-effort: a missed
// heartbeat only risks an early steal, which the caller's own ctx/turn handling
// already tolerates.
func (l *FileLock) Touch() error {
	if l == nil || l.path == "" {
		return nil
	}
	now := time.Now()
	return os.Chtimes(l.path, now, now)
}

func (l *FileLock) Release() error {
	if l == nil {
		return nil
	}
	var closeErr error
	if l.file != nil {
		closeErr = l.file.Close()
		l.file = nil
	}
	removeErr := os.Remove(l.path)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
