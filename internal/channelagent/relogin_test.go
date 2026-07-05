package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractLoginURL(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want string
	}{
		{"claude.ai oauth", "Browser didn't open? Visit:\n  https://claude.ai/oauth/authorize?code=1&client_id=x\nPaste code here:", "https://claude.ai/oauth/authorize?code=1&client_id=x"},
		{"real v2.1.201 claude.com/cai host", "Browser didn't open? Use the url below to sign in (c to copy)\n\nhttps://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge_method=S256&state=rVBxK9ANCB64\n\nPaste code here if prompted >", "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge_method=S256&state=rVBxK9ANCB64"},
		{"console host", "open https://console.anthropic.com/oauth/authorize?foo=bar to continue", "https://console.anthropic.com/oauth/authorize?foo=bar"},
		{"none in normal pane", "❯ hello world\n  just chatting about https://example.com/oauth-guide", ""},
		{"idle no url", "❯ \n? for shortcuts", ""},
	}
	for _, c := range cases {
		if got := extractLoginURL(c.pane); got != c.want {
			t.Errorf("%s: extractLoginURL = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPaneAwaitingPasteCode(t *testing.T) {
	if !paneAwaitingPasteCode("...\nPaste code here if prompted") {
		t.Error("should detect 'Paste code here'")
	}
	if paneAwaitingPasteCode("❯ normal prompt\nno login here") {
		t.Error("false positive on normal pane")
	}
}

func TestParseLoginCode(t *testing.T) {
	ok := []struct{ in, want string }{
		{"code: abc123#xyz", "abc123#xyz"},
		{"CODE:  tok_9", "tok_9"},
		{"  code：中文冒號token  ", "中文冒號token"}, // full-width colon
	}
	for _, c := range ok {
		got, k := parseLoginCode(c.in)
		if !k || got != c.want {
			t.Errorf("parseLoginCode(%q) = %q,%v want %q,true", c.in, got, k, c.want)
		}
	}
	no := []string{"y", "hello code is nice", "restart hackmd", "code:", "code: has spaces here"}
	for _, s := range no {
		if _, k := parseLoginCode(s); k {
			t.Errorf("parseLoginCode(%q) should be ok=false", s)
		}
	}
}

// fakePaster records the code it was asked to paste.
type fakePaster struct {
	Injector
	pasted string
}

func (f *fakePaster) PasteLoginCode(_ context.Context, code string) error { f.pasted = code; return nil }

func TestResolvePendingReloginOnce(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}

	// no pending → no-op
	if consumed, err := ResolvePendingReloginOnce(root, &fakePaster{}); err != nil || consumed {
		t.Fatalf("no pending should be no-op, got consumed=%v err=%v", consumed, err)
	}

	// record a pending re-login + queue a code reply
	if !recordReloginRequest(root, "relogin-unit-1", "https://claude.ai/oauth/authorize?u=1") {
		t.Fatal("recordReloginRequest should post first time")
	}
	job := InputJob{Schema: 1, JobID: "j1", Source: SourceMessage{Content: "code: mytoken#state"}}
	if err := AtomicWriteJSON(pathIn(root, "inbox", "pending", "j1.json"), job); err != nil {
		t.Fatal(err)
	}

	fp := &fakePaster{}
	consumed, err := ResolvePendingReloginOnce(root, fp)
	if err != nil || !consumed {
		t.Fatalf("should consume code reply, got consumed=%v err=%v", consumed, err)
	}
	if fp.pasted != "mytoken#state" {
		t.Fatalf("pasted %q, want mytoken#state", fp.pasted)
	}
	if hasPendingRelogin(root) {
		t.Error("pending should be cleared after success")
	}
	// reply archived to done
	if _, err := oldestJSON(pathIn(root, "inbox", "pending")); err == nil {
		if p, _ := oldestJSON(pathIn(root, "inbox", "pending")); p != "" {
			t.Error("reply should have left pending")
		}
	}
	if _, err := os.ReadFile(filepath.Join(root, "inbox", "done", "j1.json")); err != nil {
		t.Errorf("reply should be archived to done: %v", err)
	}
}

func TestResolvePendingReloginOnce_NonCodeLeftAlone(t *testing.T) {
	root := t.TempDir()
	_ = Init(root)
	recordReloginRequest(root, "relogin-unit-2", "https://claude.ai/oauth/authorize?u=2")
	job := InputJob{Schema: 1, JobID: "j2", Source: SourceMessage{Content: "隨便聊天不是 code"}}
	_ = AtomicWriteJSON(pathIn(root, "inbox", "pending", "j2.json"), job)
	fp := &fakePaster{}
	consumed, err := ResolvePendingReloginOnce(root, fp)
	if err != nil || consumed {
		t.Fatalf("non-code msg must be left alone, got consumed=%v err=%v", consumed, err)
	}
	if fp.pasted != "" {
		t.Error("must not paste for a non-code message")
	}
	if !hasPendingRelogin(root) {
		t.Error("pending must remain (still waiting for the code)")
	}
}

func TestLoginMethodMenuClassifiesAsLogin(t *testing.T) {
	menu := "  Login\n  Claude Code can be used with your Claude subscription or billed based on API\n  Select login method:\n  ❯ 1. Claude account with subscription · Pro, Max, Team, or Enterprise\n    2. Anthropic Console account · API usage billing\n    3. 3rd-party platform\n  Esc to cancel"
	if got := classifyScreen(menu); got != ScreenLogin {
		t.Fatalf("login-method menu should classify as login, got %q", got)
	}
	if !paneAwaitingLoginMethod(menu) {
		t.Fatal("paneAwaitingLoginMethod should detect the menu")
	}
	if paneAwaitingLoginMethod("❯ just a normal 1. list\n2. here") {
		t.Error("must not trip on an ordinary numbered list")
	}
	var i interface{} = TmuxInjector{}
	if _, ok := i.(loginMethodSelector); !ok {
		t.Fatal("TmuxInjector must implement loginMethodSelector")
	}
}

func TestExtractLoginURL_HardWrappedRows(t *testing.T) {
	full := "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge_method=S256&state=rVBxK9ANCB64"
	// simulate Ink absolute-positioned rows: URL split across pane rows (each a
	// separate line, NOT a soft-wrap), preceded by hint, followed by blank + prompt.
	r1 := full[:79]
	r2 := full[79:158]
	r3 := full[158:]
	cases := map[string]string{
		"3-row hard wrap, no indent": "Browser didn't open? Use the url below to sign in (c to copy)\n\n" + r1 + "\n" + r2 + "\n" + r3 + "\n\nPaste code here if prompted >",
		"2-row with leading indent":  "sign in below\n\n   " + full[:79] + "\n   " + full[79:] + "\n\nPaste code here if prompted >",
		"single wide line":           "Use the url below\n\n" + full + "\n\nPaste code here if prompted >",
	}
	for name, pane := range cases {
		if got := extractLoginURL(pane); got != full {
			t.Errorf("%s:\n got len %d: %q\nwant len %d: %q", name, len(got), got, len(full), full)
		}
	}
	// must NOT wrongly glue an unrelated no-space token below a complete URL that
	// is followed by a blank line
	if got := extractLoginURL("x\n\n" + full + "\n\nSOMETHINGELSE"); got != full {
		t.Errorf("blank-line stop failed: got %q", got)
	}
}

func TestPostLoginScreensClassifyAsLogin(t *testing.T) {
	cont := "  Login\n\n  Logged in as conray@jvd.tw\n  Login successful. Press Enter to continue…"
	if classifyScreen(cont) != ScreenLogin {
		t.Error("'Press Enter to continue' should classify as login")
	}
	mg := "  Managed settings require approval\n  Settings requiring approval:\n   · hooks\n  ❯ 1. Yes, I trust these settings\n    2. No, exit Claude Code"
	if classifyScreen(mg) != ScreenLogin {
		t.Error("managed-settings gate should classify as login")
	}
	// detectors on lowercased ANSI-stripped text
	if !paneAwaitingLoginContinue(strings.ToLower(cont)) {
		t.Error("paneAwaitingLoginContinue miss")
	}
	if !paneAwaitingManagedSettings(strings.ToLower(mg)) {
		t.Error("paneAwaitingManagedSettings miss")
	}
	// no false positives on a normal idle pane
	if paneAwaitingLoginContinue("❯ \n? for shortcuts") || paneAwaitingManagedSettings("❯ hello") {
		t.Error("false positive on idle pane")
	}
	// injector implements both new capabilities
	var i interface{} = TmuxInjector{}
	if _, ok := i.(loginContinuer); !ok {
		t.Error("TmuxInjector must implement loginContinuer")
	}
	if _, ok := i.(managedSettingsTruster); !ok {
		t.Error("TmuxInjector must implement managedSettingsTruster")
	}
}

func TestPasteCodeScreenClassifiesAsLogin(t *testing.T) {
	pane := "  Login\n  Browser didn't open? Use the url below to sign in (c to copy)\nhttps://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=xyz\n  Paste code here if prompted >\n  Esc to cancel"
	if classifyScreen(pane) != ScreenLogin {
		t.Fatalf("paste-code/URL screen must classify as login, got %v", classifyScreen(pane))
	}
	if classifyScreen("random\nhttps://claude.com/cai/oauth/authorize?code=true&x=1\nmore") != ScreenLogin {
		t.Error("live OAuth URL on pane must classify as login")
	}
	if classifyScreen("❯ \n? for shortcuts") == ScreenLogin {
		t.Error("idle pane wrongly classified as login")
	}
}
