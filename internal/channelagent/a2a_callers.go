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
	CallerID            string       `json:"caller_id"`
	Credential          string       `json:"credential"`
	Status              CallerStatus `json:"status"`
	GrantedCapabilities []string     `json:"granted_capabilities"`
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
