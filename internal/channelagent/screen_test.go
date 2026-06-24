package channelagent

import "testing"

func TestClassifyScreen(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want ScreenState
	}{
		{"glitch", "● court\n<invoke name=\"Read\">\n<parameter name=\"file_path\">x</parameter>", ScreenGlitch},
		{"working_spinner", "✻ Cooking… (1m 4s · ↓ 3.5k tokens)", ScreenWorking},
		{"confirm", "Do you want to proceed?\n❯ 1. Yes\n  2. No", ScreenConfirm},
		{"idle", "some output\n────────\n❯ \n────────", ScreenIdle},
		{"prose_bypass_no_false_positive", "我在解釋 bypass permissions 的設計，不該誤判", ScreenUnknown},
	}
	for _, c := range cases {
		if got := classifyScreen(c.pane); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestClassifyScreenStripsANSI(t *testing.T) {
	pane := "\x1b[0;32m● Cooking… (2s · ↓ 10 tokens)\x1b[0m"
	if got := classifyScreen(pane); got != ScreenWorking {
		t.Fatalf("ansi-wrapped working = %q", got)
	}
}

func TestClassifyScreenLogin(t *testing.T) {
	for _, p := range []string{
		"● Please run /login · API Error: 401 Invalid authentication credentials",
		"Invalid authentication credentials",
		"You are not logged in.",
	} {
		if got := classifyScreen(p); got != ScreenLogin {
			t.Errorf("login pane %q => %q", p, got)
		}
	}
	// prose mentioning login must not trigger (no distinctive phrase)
	if classifyScreen("我等下要 login 一下") == ScreenLogin {
		t.Fatal("prose false-positive on login")
	}
}

// A transient network 401 that Claude prefixes with "Please run /login" must NOT
// be classified as a login screen — auth is fine, only the socket blipped. Doing
// so loop-kills a healthy authenticated session (the line replays on --resume).
func TestClassifyScreenTransient401NotLogin(t *testing.T) {
	transient := []string{
		"● Please run /login · API Error: 401 The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()",
		"Please run /login · API Error: 401 Connection error",
		"● Please run /login · API Error: 401 fetch failed: network timeout",
	}
	for _, p := range transient {
		if got := classifyScreen(p); got == ScreenLogin {
			t.Errorf("transient 401 must not be login, got ScreenLogin for %q", p)
		}
	}
	// A genuine login (no transient-network markers) still classifies as login.
	if got := classifyScreen("● Please run /login to continue"); got != ScreenLogin {
		t.Errorf("genuine /login prompt => %q, want login", got)
	}
}
