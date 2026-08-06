package channelagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

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
