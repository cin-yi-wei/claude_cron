package channelagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleTempAge bounds how old a leftover <base>.tmp-* sibling must be before
// sweepStaleTemp will remove it. A unique-per-call temp name (see
// AtomicWriteFile) no longer self-recycles the way the old fixed ".tmp" name
// did, so a write that never reaches its rename (SIGKILL, OOM) now orphans a
// distinct file forever instead of getting overwritten by the next write to
// the same path. The sweep only targets files clearly past any plausible
// write duration — a FRESH sibling is very likely a concurrent writer's temp
// file still being written to, and deleting it out from under that writer
// would corrupt (or simply fail) their write.
var staleTempAge = 10 * time.Minute

// sweepStaleTemp best-effort removes orphaned <base>.tmp-* files in dir left
// behind by a past AtomicWriteFile call that crashed before its rename.
// Harmless to readers either way (nothing ever pointed at these), so this
// only matters for disk usage — never let a failure here fail the caller's
// actual write; every error is swallowed.
func sweepStaleTemp(dir, base string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + ".tmp-"
	cutoff := time.Now().Add(-staleTempAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func AtomicWriteJSON(path string, v any) error {
	return AtomicWriteJSONMode(path, v, 0o644)
}

// AtomicWriteJSONMode 與 AtomicWriteJSON 相同，但可指定檔案權限。沙盒政策檔與
// callers.json 需要 0600（前者帶授權等級、後者帶明文 bearer 憑證），而
// AtomicWriteJSON 的預設 0644 被 bindings.json / triggers.json 等共用，不能改。
func AtomicWriteJSONMode(path string, v any, mode os.FileMode) error {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return AtomicWriteFile(path, payload, mode)
}

func AtomicWriteFile(path string, payload []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Best-effort: clears out any old orphaned temp file from a past crashed
	// write before adding our own. See sweepStaleTemp's doc for why this is
	// safe to do unconditionally on every write.
	sweepStaleTemp(dir, filepath.Base(path))

	// The temp file MUST have a unique name, not a fixed path+".tmp": two
	// concurrent writers to the same path (e.g. two sandboxes both calling
	// EnsureFolderTrusted against the shared ~/.claude.json) would otherwise
	// both O_TRUNC the same temp file and write from independent offsets,
	// interleaving their payloads into invalid content before either rename
	// even runs. os.CreateTemp in the SAME directory as path guarantees the
	// final os.Rename stays on one filesystem (so it's still atomic) while
	// giving every call its own file. os.CreateTemp always creates with mode
	// 0600 regardless of what's asked for, so the requested mode is applied
	// explicitly via Chmod before the rename publishes it.
	file, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return syncDir(dir)
}

func ReadJSON(path string, v any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
