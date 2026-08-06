package channelagent

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// maxAuditFieldRunes 界限 CallerID/Agent/ContextID/TaskID 這幾個「本應是
	// 短識別碼」的欄位。round 11 review, Important 2：這幾個欄位原本只有
	// Summary 會截斷，其餘直接寫入 AuditEntry 原文。message/send 的成功路
	// 徑上 contextId 已經被 a2aContextIDRe 驗證到 <=128 字元，但
	// auditBadRequest 的五個分支裡，contextId 格式錯誤／未知 agent 這兩支
	// 用的正是「格式不合法」的原始值——可以是任意長度（受限於 1 MiB 的
	// body cap）。已認證的呼叫方靠反覆送出一個幾百 KB 的不合法 contextId
	// 或 agent 名稱，幾十次請求就能把 32 MiB 的稽核 log rotate 到底、連上
	// 一代 .1 都覆寫掉——等於一個已核准呼叫方就能摧毀整份稽核歷史。這裡選
	// 在 AppendAudit 裡集中截斷，而不是只修兩個呼叫點：任何未來新增的呼叫
	// 點也會自動受這個上限保護，不必逐一記得截斷。
	maxAuditFieldRunes = 200
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
//
// round 11 review, Important 3：舊實作對整個 cut 前綴呼叫 utf8.ValidString，
// 只要輸入裡「切點以前的任何位置」曾出現一個壞位元組（例如 Detail =
// err.Error() 夾帶的原始 git/tmux 輸出，可能含任意二進位內容），就會一路
// 逐位元組往回退到空字串為止——這是 O(n) 次 O(n) 驗證疊出來的 O(n²)，而且
// 在持有 tasksMu 全域鎖的 Upsert 裡跑；用一個開頭 0xFF 加 64 KiB 的輸入測
// 過，結果是整段 64 KiB 被吃光，只剩下截斷標記本身，且燒了 ~17ms 的 CPU。
// 這個函式真正該做的只是「切點本身有沒有落在一個多位元組字元中間」，不是
// 「這段字串的其他地方有沒有壞位元組」（後者不是也修不完，且與這次的切點
// 無關）。改成只往回看切點附近最多 utf8.UTFMax（4）個位元組：
// DecodeLastRuneInString 回報「最後這幾個位元組解不出一個完整、合法的
// rune」時才退一個位元組再試，最多試 utf8.UTFMax 次就停手——不論輸入多長，
// 這段工作量都是常數。
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for i := 0; i < utf8.UTFMax && len(cut) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size != 1 {
			break // 切點乾淨（合法 rune），或剩下的壞位元組跟這次切點無關，不再處理
		}
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
	// round 11 review, Important 2：這四個欄位本應是短識別碼，但
	// auditBadRequest 的幾個分支寫入的是「格式驗證失敗」的原始呼叫方輸入，
	// 沒有任何長度保證。集中在這裡截斷，任何呼叫端（現有或未來）都自動受
	// 保護，不必逐一記得處理。
	e.CallerID = truncateRunes(e.CallerID, maxAuditFieldRunes)
	e.Agent = truncateRunes(e.Agent, maxAuditFieldRunes)
	e.ContextID = truncateRunes(e.ContextID, maxAuditFieldRunes)
	e.TaskID = truncateRunes(e.TaskID, maxAuditFieldRunes)
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

// auditKeyPath 是這個 root 專屬、只給稽核指紋用的 HMAC 金鑰檔案路徑。
func auditKeyPath(root string) string { return filepath.Join(root, "a2a-audit-key") }

var (
	auditKeyMu    sync.Mutex
	auditKeyCache = map[string][]byte{}
)

// loadOrCreateAuditKey 回傳 root 專屬的 32 位元組 HMAC 金鑰，第一次呼叫時
// 視需要建立在磁碟上並快取在記憶體，之後每一次呼叫都直接命中快取，不再碰
// 磁碟。
//
// round 11 review, Minor 5：credentialFingerprint 原本是不加鹽的裸
// SHA-256，讓這份 log 變成一個 32-bit 的「猜測確認器」——任何讀得到 log 的
// 人，把自己猜的憑證算一次 SHA-256 去比對指紋，就能一組一組驗證某個特定
// 字串是不是曾經被送來的憑證。換成 HMAC、金鑰只落在磁碟上（不進版控、不
// 進任何 HTTP 回應、不寫進 log 本身），log 內容就不再足以驗證任何猜測——
// 沒有這把金鑰，連「這兩筆指紋是不是同一個憑證」都驗不出來，只能看出「同
// 一把金鑰下這兩筆是否相同」，而金鑰本身不外流。
//
// 用 O_EXCL 建立（跟 lock.go 的 AcquireLock 同一個手法），不是「先讀不到才
// 寫」：兩個行程同時第一次啟動、都讀不到既有金鑰檔時，只有一個能真的建立
// 成功，輸的那個改讀贏家剛寫好的檔案——不會出現兩個行程各自產生不同金鑰、
// 之後同一組憑證在兩邊算出不同指紋，讓「串起同一組失敗嘗試」這個功能本身
// 失效的情況。
func loadOrCreateAuditKey(root string) []byte {
	auditKeyMu.Lock()
	defer auditKeyMu.Unlock()
	if k, ok := auditKeyCache[root]; ok {
		return k
	}
	path := auditKeyPath(root)
	if k := readAuditKeyFile(path); k != nil {
		auditKeyCache[root] = k
		return k
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 讀不到隨機源是機器層級的嚴重異常。退化成一把只在這
		// 個行程生命週期內有效、不落地的金鑰，好過讓整個 handleRPC 崩掉
		// ——這把金鑰只用來擋猜測確認，不是加密機密，弱一點的隨機性可以
		// 接受，總比功能整個掛掉好。
		auditKeyCache[root] = buf
		return buf
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		f, cErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if cErr == nil {
			_, _ = f.WriteString(hex.EncodeToString(buf))
			_ = f.Close()
		} else if errors.Is(cErr, os.ErrExist) {
			// 輸掉建立競賽:改讀贏家剛寫好的檔案，不要用自己剛產生的這把
			// ——否則兩個行程會各自快取不同的金鑰。
			if k := readAuditKeyFile(path); k != nil {
				auditKeyCache[root] = k
				return k
			}
			// 贏家還沒寫完（極窄的競賽視窗），讀不到：退而求其次先用自己
			// 這把，下一次呼叫會重新嘗試讀檔案。
		}
	}
	auditKeyCache[root] = buf
	return buf
}

// readAuditKeyFile 讀取並解碼既有的金鑰檔，格式不對或讀不到都回 nil（不是
// 錯誤——呼叫方會視同「還沒有金鑰」處理）。
func readAuditKeyFile(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(k) == 0 {
		return nil
	}
	return k
}

// credentialFingerprint 回傳憑證的 HMAC-SHA256（以 root 專屬金鑰）前 8 個
// hex 字元。**絕不記憑證本身**，也絕不用不加鹽的雜湊（見上方 Minor 5 的
// 說明）。
func credentialFingerprint(root, credential string) string {
	if credential == "" {
		return ""
	}
	key := loadOrCreateAuditKey(root)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(credential))
	return hex.EncodeToString(mac.Sum(nil))[:8]
}
