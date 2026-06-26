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

// staleLockTimeout bounds how long a lock may be held before a new acquirer
// treats it (by AGE) as stale and steals it. It MUST exceed the longest
// legitimate hold, which is one worker/control turn: AcquireLock is held across
// Inject + waitOutput, so the hold can last up to the full claude turn timeout
// (cfg.Claude.Timeout, 900s/15min in prod). The old value (5min) was SHORTER than
// that, so any turn running longer than 5min (e.g. a control deploy turn that
// builds + restarts) had its lock STOLEN mid-flight by the next serve cycle —
// two goroutines then "held" control-dc/claude.lock, and on release one removed
// the other's file, producing the "held by live pid" churn / apparent stuck-lock
// (2026-06-27 incident). 20min leaves margin over the 900s turn cap. A holder
// whose PROCESS died is still reclaimed instantly by the pid-alive check below —
// the age path only governs a still-live but genuinely-hung holder. Overridable
// in tests. Also the backstop for the 2026-06-18 wedged-serve incident.
var staleLockTimeout = 20 * time.Minute

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
