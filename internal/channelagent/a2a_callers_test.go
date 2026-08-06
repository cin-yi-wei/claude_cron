package channelagent

import "testing"

func TestCallerRegisterStartsPending(t *testing.T) {
	var s CallerStore
	if err := s.Register("peer-a", "secret-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c, ok := s.Authenticate("secret-1")
	if ok {
		t.Fatalf("pending caller must not authenticate, got %#v", c)
	}
}

func TestCallerApproveThenAuthenticate(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	if !s.Approve("peer-a", []string{"read", "write"}) {
		t.Fatal("Approve returned false")
	}
	c, ok := s.Authenticate("secret-1")
	if !ok {
		t.Fatal("approved caller should authenticate")
	}
	if c.CallerID != "peer-a" {
		t.Fatalf("CallerID = %q", c.CallerID)
	}
	if !c.Allows("read") || !c.Allows("write") {
		t.Fatalf("granted caps missing: %#v", c.GrantedCapabilities)
	}
	if c.Allows("admin") {
		t.Fatal("ungranted capability must be denied")
	}
}

func TestCallerRevokeBlocksAuthentication(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	s.Approve("peer-a", []string{"read"})
	if !s.Revoke("peer-a") {
		t.Fatal("Revoke returned false")
	}
	if _, ok := s.Authenticate("secret-1"); ok {
		t.Fatal("revoked caller must not authenticate")
	}
}

func TestCallerAuthenticateRejectsUnknownCredential(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	s.Approve("peer-a", []string{"read"})
	if _, ok := s.Authenticate("wrong"); ok {
		t.Fatal("unknown credential must not authenticate")
	}
	if _, ok := s.Authenticate(""); ok {
		t.Fatal("empty credential must not authenticate")
	}
}

func TestCallerEffectiveGrantLevelDefaultsToReadOnly(t *testing.T) {
	// 既有 callers.json 沒有 grant_level 欄位:解讀為最小權限的地板,
	// 不是「無限制」,也不是「拒絕」。
	if got := (Caller{Status: CallerApproved}).EffectiveGrantLevel(); got != GrantReadOnly {
		t.Fatalf("EffectiveGrantLevel() = %q, want %q", got, GrantReadOnly)
	}
	if got := (Caller{Status: CallerApproved, GrantLevel: GrantFull}).EffectiveGrantLevel(); got != GrantFull {
		t.Fatalf("EffectiveGrantLevel() = %q, want %q", got, GrantFull)
	}
	// 檔案裡出現不認得的值 → 退回地板,絕不放大。
	if got := (Caller{Status: CallerApproved, GrantLevel: "root"}).EffectiveGrantLevel(); got != GrantReadOnly {
		t.Fatalf("EffectiveGrantLevel() = %q for a bogus level, want %q", got, GrantReadOnly)
	}
}

func TestSetGrantLevel(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret")
	if !s.SetGrantLevel("peer-a", GrantDevelop) {
		t.Fatal("SetGrantLevel on an existing caller must report success")
	}
	if s.Callers[0].GrantLevel != GrantDevelop {
		t.Fatalf("grant level = %q", s.Callers[0].GrantLevel)
	}
	if s.SetGrantLevel("ghost", GrantDevelop) {
		t.Fatal("SetGrantLevel on an unknown caller must report failure")
	}
}

func TestCallerStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	s.Approve("peer-a", []string{"read"})
	if err := SaveCallers(root, s); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}
	got, err := LoadCallers(root)
	if err != nil {
		t.Fatalf("LoadCallers: %v", err)
	}
	if _, ok := got.Authenticate("secret-1"); !ok {
		t.Fatal("approved caller lost across round-trip")
	}
}
