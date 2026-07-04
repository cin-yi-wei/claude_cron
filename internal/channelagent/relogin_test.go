package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractLoginURL(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want string
	}{
		{"claude.ai oauth", "Browser didn't open? Visit:\n  https://claude.ai/oauth/authorize?code=1&client_id=x\nPaste code here:", "https://claude.ai/oauth/authorize?code=1&client_id=x"},
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
