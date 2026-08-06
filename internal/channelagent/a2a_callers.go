package channelagent

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type CallerStatus string

const (
	CallerPending  CallerStatus = "pending"
	CallerApproved CallerStatus = "approved"
	CallerRevoked  CallerStatus = "revoked"
)

// Caller is a registered external A2A peer. Registration is open, but a caller
// stays Pending — and cannot authenticate — until a human approves it.
type Caller struct {
	CallerID   string       `json:"caller_id"`
	Credential string       `json:"credential"`
	Status     CallerStatus `json:"status"`
	// GrantedCapabilities 是**路由標籤**,不是沙盒權限。它只在 dispatch 當下
	// 比對「這個呼叫方能不能叫這個 agent」;沙盒實際能做什麼完全由 GrantLevel
	// 決定(a2a_policy.go 的三個等級 + a2a_gate.go 的判定表)。宣告
	// ["docs-only"] 不會讓沙盒變得只能碰文件。
	GrantedCapabilities []string `json:"granted_capabilities"`
	// GrantLevel 是這個呼叫方的授權上限。有效等級 = min(請求的 level,
	// GrantLevel)。空值解讀為 readonly(見 EffectiveGrantLevel)。
	GrantLevel GrantLevel `json:"grant_level,omitempty"`
	// CallbackURL / CallbackToken 只能由 operator 經 CLI / admin API 設定，
	// 永遠不接受請求提供 —— 否則這台主機就成了 SSRF 跳板。
	CallbackURL   string `json:"callback_url,omitempty"`
	CallbackToken string `json:"callback_token,omitempty"`
}

type CallerStore struct {
	Callers []Caller `json:"callers"`
}

func CallersPath(root string) string { return filepath.Join(root, "callers.json") }

func LoadCallers(root string) (CallerStore, error) {
	var s CallerStore
	if err := ReadJSON(CallersPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CallerStore{}, nil
		}
		return CallerStore{}, err
	}
	return s, nil
}

// SaveCallers 用 0600：這份檔案帶明文 bearer 憑證。AtomicWriteJSON 的預設
// 0644 被 bindings.json 等共用，不能改，所以走 AtomicWriteJSONMode。
func SaveCallers(root string, s CallerStore) error {
	return AtomicWriteJSONMode(CallersPath(root), s, 0o600)
}

// callersMu 序列化 callers.json 的 read-modify-write，理由與 tasksMu 對
// tasks.json 完全相同（a2a_store.go）：AtomicWriteJSONMode 擋得住寫壞的檔
// 案，擋不住遺失更新——admin API 的核准/撤銷/改等級/設回呼各自「整檔讀 →
// 改一個欄位 → 整檔寫」，兩個併發請求交錯時後寫的那份會把前一份整個蓋掉。
// 最嚴重的形態是撤銷：HTTP 回 200 OK，callers.json 裡那個 caller 卻還是
// approved，還能繼續認證（round 14 review, Critical 2）。CLI 走 admin API、
// 不直接寫檔（cmd/claude-cron/a2a_cmd.go 的說明），所以行程內互斥就足夠。
//
// 鎖序與既有的兩把鎖完全不交會：這把鎖內只做 callers.json 的讀寫，絕不取
// session 鎖、絕不巢狀呼叫 WithTasks（RevokeCaller 因此把 terminateTasks 留
// 在 WithCallers 之外）。也不得在鎖內做慢動作（HTTP、DNS 解析、tmux、git）
// ——ValidateCallbackURL 會解析 DNS，呼叫端必須在進鎖之前先驗完。
var callersMu sync.Mutex

// WithCallers 在鎖內載入 callers.json、交給 fn 修改、再存檔。fn 回傳 error
// 時完全不寫檔。這把鎖不可重入：fn 內絕不可以再呼叫 WithCallers。
func WithCallers(root string, fn func(*CallerStore) error) error {
	callersMu.Lock()
	defer callersMu.Unlock()

	s, err := LoadCallers(root)
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return SaveCallers(root, s)
}

func (s *CallerStore) Register(id, credential string) error {
	if id == "" || credential == "" {
		return fmt.Errorf("caller id and credential are required")
	}
	for _, c := range s.Callers {
		if c.CallerID == id {
			return fmt.Errorf("caller %q already registered", id)
		}
	}
	s.Callers = append(s.Callers, Caller{CallerID: id, Credential: credential, Status: CallerPending})
	return nil
}

func (s *CallerStore) Approve(id string, caps []string) bool {
	for i := range s.Callers {
		if s.Callers[i].CallerID == id {
			s.Callers[i].Status = CallerApproved
			s.Callers[i].GrantedCapabilities = caps
			return true
		}
	}
	return false
}

func (s *CallerStore) Revoke(id string) bool {
	for i := range s.Callers {
		if s.Callers[i].CallerID == id {
			s.Callers[i].Status = CallerRevoked
			return true
		}
	}
	return false
}

// Authenticate resolves a credential to an approved caller. Pending and revoked
// callers never authenticate. Comparison is constant-time.
func (s CallerStore) Authenticate(credential string) (Caller, bool) {
	if credential == "" {
		return Caller{}, false
	}
	for _, c := range s.Callers {
		if c.Status != CallerApproved {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c.Credential), []byte(credential)) == 1 {
			return c, true
		}
	}
	return Caller{}, false
}

// Allows reports whether this caller was granted a capability. The grant list is
// the whole policy: there is no runtime prompt.
func (c Caller) Allows(capability string) bool {
	for _, g := range c.GrantedCapabilities {
		if g == capability {
			return true
		}
	}
	return false
}

// EffectiveGrantLevel 回傳這個呼叫方實際可用的上限等級。空值或無法辨識的值
// 一律退回 readonly —— 最小權限的地板,而不是無限制。既有 callers.json 沒有
// 這個欄位,所以這條規則同時是相容性路徑。
func (c Caller) EffectiveGrantLevel() GrantLevel {
	if ValidGrantLevel(c.GrantLevel) {
		return c.GrantLevel
	}
	return GrantReadOnly
}

// SetGrantLevel 設定呼叫方的授權上限。只由 operator 經 CLI / admin API 觸發。
func (s *CallerStore) SetGrantLevel(id string, l GrantLevel) bool {
	for i := range s.Callers {
		if s.Callers[i].CallerID == id {
			s.Callers[i].GrantLevel = l
			return true
		}
	}
	return false
}

// SetCallback 設定這個呼叫方的完成回呼目的地。呼叫端（admin API / CLI）必須
// 先跑過 ValidateCallbackURL —— 目的地在「設定當下與觸發當下」各驗一次。
func (s *CallerStore) SetCallback(id, url, token string) bool {
	for i := range s.Callers {
		if s.Callers[i].CallerID == id {
			s.Callers[i].CallbackURL = url
			s.Callers[i].CallbackToken = token
			return true
		}
	}
	return false
}
