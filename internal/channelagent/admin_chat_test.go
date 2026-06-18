package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatHubPubSub(t *testing.T) {
	hub := NewChatHub()
	ch, unsub := hub.Subscribe("k")
	defer unsub()
	if n := hub.Subscribers("k"); n != 1 {
		t.Fatalf("subscribers = %d, want 1", n)
	}
	hub.Publish("k", ChatEvent{Role: "assistant", Text: "hi"})
	ev := <-ch
	if ev.Text != "hi" || ev.Role != "assistant" {
		t.Fatalf("event = %#v", ev)
	}
	unsub()
	if n := hub.Subscribers("k"); n != 0 {
		t.Fatalf("after unsub subscribers = %d, want 0", n)
	}
}

func TestChatHubNonBlocking(t *testing.T) {
	hub := NewChatHub()
	_, unsub := hub.Subscribe("k") // never drained
	defer unsub()
	// Far more than the buffer (32); must not block/panic — excess is dropped.
	for i := 0; i < 100; i++ {
		hub.Publish("k", ChatEvent{Text: "x"})
	}
}

func TestWebSenderPublishes(t *testing.T) {
	hub := NewChatHub()
	ch, unsub := hub.Subscribe("calc")
	defer unsub()
	s := WebSender{Hub: hub, Key: "calc"}
	if err := s.Send(context.Background(), OutputJob{Text: "reply", Send: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := <-ch; ev.Text != "reply" || ev.Role != "assistant" {
		t.Fatalf("event = %#v", ev)
	}
}

func TestChatSendIngestsAndEchoes(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "webx")
	if err := Init(bRoot); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "webx", ChannelID: "webx", TmuxSession: "cc-webx", Root: bRoot, Platform: PlatformWeb})

	// Subscribe to catch the user echo.
	ch, unsub := DefaultChatHub.Subscribe("webx")
	defer unsub()

	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/webx/send", strings.NewReader(`{"text":"hello session"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// A job must now sit in the binding inbox.
	if n := countJSON(pathIn(bRoot, "inbox", "pending")); n != 1 {
		t.Fatalf("inbox pending = %d, want 1", n)
	}
	// And the user message must have been echoed to subscribers.
	select {
	case ev := <-ch:
		if ev.Role != "user" || ev.Text != "hello session" {
			t.Fatalf("echo = %#v", ev)
		}
	default:
		t.Fatal("no user echo published")
	}
}

func TestChatSendRejectsNonWebBinding(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "dcx", ChannelID: "c1", TmuxSession: "cc-dcx"}) // discord default
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/dcx/send", strings.NewReader(`{"text":"x"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-web send status = %d, want 404", rec.Code)
	}
}

func TestChatHistoryReplaysThread(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "webx")
	if err := Init(bRoot); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "webx", ChannelID: "webx", TmuxSession: "cc-webx", Root: bRoot, Platform: PlatformWeb})

	// A processed user message (inbox/done) and its sent reply (outbox/sent).
	in := InputJob{Schema: 1, JobID: "j1", Source: SourceMessage{Platform: PlatformWeb, Content: "hi there", CreatedAt: "2026-06-18T00:00:00Z"}}
	if err := AtomicWriteJSON(pathIn(bRoot, "inbox", "done", "j1.json"), in); err != nil {
		t.Fatal(err)
	}
	out := OutputJob{Schema: 1, JobID: "j1", Send: true, Text: "hello back"}
	if err := AtomicWriteJSON(pathIn(bRoot, "outbox", "sent", "j1.json"), out); err != nil {
		t.Fatal(err)
	}
	// A non-send reply must be excluded.
	if err := AtomicWriteJSON(pathIn(bRoot, "outbox", "sent", "j2.json"), OutputJob{Schema: 1, JobID: "j2", Send: false, Text: "skipped"}); err != nil {
		t.Fatal(err)
	}

	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/chat/webx/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d", rec.Code)
	}
	var got []ChatEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("history len = %d, want 2 (non-send excluded): %#v", len(got), got)
	}
	roles := got[0].Role + "," + got[1].Role
	if roles != "user,assistant" {
		t.Fatalf("order/roles = %q, want user,assistant", roles)
	}
	if got[0].Text != "hi there" || got[1].Text != "hello back" {
		t.Fatalf("texts = %#v", got)
	}
}

func TestChatAuthQueryToken(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "webx")
	_ = Init(bRoot)
	seedBinding(t, root, Binding{Name: "webx", ChannelID: "webx", TmuxSession: "cc-webx", Root: bRoot, Platform: PlatformWeb})

	h := AdminHandler{Root: root, Token: "secret"}
	// No token → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat/webx/send", strings.NewReader(`{"text":"x"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}
	// Query token → allowed (covers SSE which can't send a header).
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/chat/webx/send?token=secret", strings.NewReader(`{"text":"x"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("query-token status = %d, want 200", rec2.Code)
	}
}
