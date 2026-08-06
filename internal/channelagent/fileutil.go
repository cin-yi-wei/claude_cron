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

// AtomicWriteFile 是 A2A 動機下重寫的版本（把固定的 <path>.tmp 改成
// os.CreateTemp 產生的獨一無二暫存檔名，見下方註解），但這個函式本身不在
// cfg.A2A.Enabled 底下——它是 AtomicWriteJSON/AtomicWriteJSONMode 共用的
// 底層實作，activity.json / state / bindings.json 等每一個 cc- binding 的
// 熱寫入路徑全部經過它，關掉 A2A 也繞不開這裡。
//
// 確認過（不是照審查者的話直接相信）：這個改動在這台機器目前的 umask 下對
// cc- binding 是無害的，理由跟 umask 本身完全無關——
//  1. os.CreateTemp 一律以 0600 建立暫存檔，且這個呼叫**不傳** mode 參數
//     給它（Go 文件明載：CreateTemp 永遠是 0600，忽略呼叫端想要的權限）。
//     0600 對 group/other 已經是零位，umask 只會「進一步清掉」創建時的位
//     元，不會「加回」被 umask 清掉的位元——所以不管這台機器的 umask 是
//     022、002 還是更嚴格的 077，算出來的結果都還是 0600，umask 在這一步
//     完全不影響結果。
//  2. 真正決定「發布出去的檔案」最終權限的是下面 file.Chmod(mode) 這一
//     行——chmod(2) 是絕對設值，跟 umask（只影響 open/creat 的建立時預設
//     權限）無關。所以不管 umask 是什麼，rename 之前 Chmod 已經把權限改成
//     呼叫端要求的那個值；umask 唯一可能影響的窗口（CreateTemp 到 Chmod
//     之間，暫存檔還是 0600）永遠比任何呼叫端要求的 mode（0644 或 0600）
//     更嚴格或相等，絕不會讓暫存檔在這段窗口內比最終權限更寬鬆。
//  3. 結論：umask 在這條路徑上唯一能影響的是 os.MkdirAll(dir, 0o755) 建立
//     新目錄時的權限（這行本身不是這次重寫新加的），跟這次重寫要修的並發
//     覆寫問題（獨一無二暫存檔名）完全無關；此函式改動的核心行為（暫存檔
//     命名、Chmod、Sync、Rename）在任何合理 umask 下都不變。
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
