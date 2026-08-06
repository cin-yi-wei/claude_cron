package channelagent

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func SaveCallers(root string, s CallerStore) error {
	return AtomicWriteJSON(CallersPath(root), s)
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
