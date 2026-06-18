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
// treats it as stale and steals it. No legitimate hold lasts this long (the
// longest is the control assistant's inject-wait + send, ~2min), so a lock older
// than this means the previous holder hung or died without releasing. Overridable
// in tests. This is the backstop for the 2026-06-18 incident where a wedged serve
// held control-dc/locks/claude.lock for 42min and blocked all replies.
var staleLockTimeout = 5 * time.Minute

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
