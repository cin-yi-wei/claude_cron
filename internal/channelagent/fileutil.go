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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
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
	return syncDir(filepath.Dir(path))
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
