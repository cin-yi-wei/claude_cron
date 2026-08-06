package channelagent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// AuditEntry is one delegation event. The log is append-only JSONL: an
// externally reachable system needs a durable record of who asked for what.
type AuditEntry struct {
	At        string `json:"at"`
	CallerID  string `json:"caller_id"`
	Agent     string `json:"agent"`
	ContextID string `json:"context_id"`
	TaskID    string `json:"task_id,omitempty"`
	Summary   string `json:"summary"`
	Outcome   string `json:"outcome"`
	// CredentialFP 是憑證的 SHA-256 前 8 個 hex 字元，用於把同一組失敗嘗試
	// 串起來。**絕不記憑證本身。**
	CredentialFP string `json:"credential_fp,omitempty"`
	// RemoteAddr 是來源位址（只取 host）。
	RemoteAddr string `json:"remote_addr,omitempty"`
}

const (
	// maxSummaryRunes 截斷呼叫方原文。Summary 目前只受 1 MiB body cap 限制，
	// 而 ReadAudit 的 per-line 上限會被超長行整份打壞。
	maxSummaryRunes = 512
	// AuditMaxBytes 是單一 log 檔的上限，超過就 rename 成 <name>.1（只留一代）。
	AuditMaxBytes = 32 << 20
	// maxAuditLineBytes 是讀取時願意接受的單行上限，超過的行整行跳過。
	maxAuditLineBytes = 1 << 20
)

func AuditPath(root string) string { return filepath.Join(root, "a2a-audit.jsonl") }

// truncateRunes 依 rune 數截斷並加上明確的截斷標記。永遠不靜默縮短。
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…（截斷）"
}

// truncateBytes 依位元組截斷，並退回到最近的 rune 邊界，避免切出半個字。
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…（截斷）"
}

// appendRotatingLine 以單次 O_APPEND write 追加一行，並在檔案超過 AuditMaxBytes
// 時先 rename 成 <path>.1（只留一代）再 append。a2a-audit.jsonl 與
// a2a-gate.jsonl 共用這個機制。
func appendRotatingLine(path string, line []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= AuditMaxBytes {
		// rename 失敗不阻止寫入：留一份過大的 log 遠比丟掉這一筆紀錄好。
		if rErr := os.Rename(path, path+".1"); rErr != nil {
			fmt.Fprintf(os.Stderr, "a2a: rotate %s 失敗（繼續追加）: %v\n", path, rErr)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

func AppendAudit(root string, e AuditEntry) error {
	e.Summary = truncateRunes(e.Summary, maxSummaryRunes)
	blob, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return appendRotatingLine(AuditPath(root), append(blob, '\n'), 0o600)
}

// ReadAudit 用 bufio.Reader 而非 Scanner：一行壞掉或超長不得讓整份 log 讀不
// 出來。超過 maxAuditLineBytes 的行整行跳過（讀到換行為止），解不開的行跳過。
func ReadAudit(root string) ([]AuditEntry, error) {
	f, err := os.Open(AuditPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []AuditEntry
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rErr := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 && len(line) <= maxAuditLineBytes {
				var e AuditEntry
				if json.Unmarshal(line, &e) == nil {
					out = append(out, e)
				}
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				return out, nil
			}
			return out, rErr
		}
	}
}

// credentialFingerprint 回傳憑證的 SHA-256 前 8 個 hex 字元。
func credentialFingerprint(credential string) string {
	if credential == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])[:8]
}
