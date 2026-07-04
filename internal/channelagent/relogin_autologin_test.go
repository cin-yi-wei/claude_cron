package channelagent

import "testing"

// autoLoginAllowed rate-limits /login auto-typing per binding: first call true,
// immediate second call false (within cooldown).
func TestAutoLoginAllowedRateLimits(t *testing.T) {
	b := "relogin-autologin-1"
	if !autoLoginAllowed(b) {
		t.Fatal("first auto-login should be allowed")
	}
	if autoLoginAllowed(b) {
		t.Fatal("second auto-login within cooldown must be blocked")
	}
	// a different binding is independent
	if !autoLoginAllowed("relogin-autologin-2") {
		t.Fatal("different binding should be allowed")
	}
}

// TmuxInjector must satisfy loginTyper (so the supervisor's auto-/login path
// engages on a real injector).
func TestTmuxInjectorIsLoginTyper(t *testing.T) {
	var i interface{} = TmuxInjector{Session: "x", Root: "y"}
	if _, ok := i.(loginTyper); !ok {
		t.Fatal("TmuxInjector should implement loginTyper (SendLogin)")
	}
	if _, ok := i.(loginPaster); !ok {
		t.Fatal("TmuxInjector should implement loginPaster (PasteLoginCode)")
	}
}
