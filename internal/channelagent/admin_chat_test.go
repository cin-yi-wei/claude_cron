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

func TestChatSendAnyBinding(t *testing.T) {
	// Any platform is chattable from the browser (replies are teed to the hub).
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "dcx")
	if err := Init(bRoot); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "dcx", ChannelID: "c1", TmuxSession: "cc-dcx", Root: bRoot}) // discord default
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/dcx/send", strings.NewReader(`{"text":"x"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("discord send status = %d, want 200 (any binding chattable)", rec.Code)
	}
	if n := countJSON(pathIn(bRoot, "inbox", "pending")); n != 1 {
		t.Fatalf("inbox pending = %d, want 1", n)
	}
	// A truly missing binding still 404s.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/chat/ghost/send", strings.NewReader(`{"text":"x"}`)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("missing binding send status = %d, want 404", rec2.Code)
	}
}

func TestTeeSenderPublishes(t *testing.T) {
	hub := NewChatHub()
	ch, unsub := hub.Subscribe("dcx")
	defer unsub()
	var got OutputJob
	inner := senderFunc(func(_ context.Context, o OutputJob) error { got = o; return nil })
	tee := TeeSender{Inner: inner, Hub: hub, Key: "dcx"}
	if err := tee.Send(context.Background(), OutputJob{Text: "hi", Send: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Text != "hi" {
		t.Fatalf("inner not called: %#v", got)
	}
	if ev := <-ch; ev.Role != "assistant" || ev.Text != "hi" {
		t.Fatalf("hub event = %#v", ev)
	}
}

// senderFunc adapts a func to the Sender interface for tests.
type senderFunc func(context.Context, OutputJob) error

func (f senderFunc) Send(ctx context.Context, o OutputJob) error { return f(ctx, o) }

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
	var resp struct {
		Messages []ChatEvent `json:"messages"`
		HasMore  bool        `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := resp.Messages
	if len(got) != 2 {
		t.Fatalf("history len = %d, want 2 (non-send excluded): %#v", len(got), got)
	}
	if resp.HasMore {
		t.Fatalf("has_more should be false (only 2 messages)")
	}
	roles := got[0].Role + "," + got[1].Role
	if roles != "user,assistant" {
		t.Fatalf("order/roles = %q, want user,assistant", roles)
	}
	if got[0].Text != "hi there" || got[1].Text != "hello back" {
		t.Fatalf("texts = %#v", got)
	}

	// Paging: limit=1 returns only the newest (assistant), with has_more.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/chat/webx/history?limit=1", nil))
	var p1 struct {
		Messages []ChatEvent `json:"messages"`
		HasMore  bool        `json:"has_more"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &p1)
	if len(p1.Messages) != 1 || p1.Messages[0].Text != "hello back" || !p1.HasMore {
		t.Fatalf("page1 = %#v has_more=%v", p1.Messages, p1.HasMore)
	}
	// before=1 skips the newest → returns the older user message, no more.
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/chat/webx/history?limit=1&before=1", nil))
	var p2 struct {
		Messages []ChatEvent `json:"messages"`
		HasMore  bool        `json:"has_more"`
	}
	_ = json.Unmarshal(rec3.Body.Bytes(), &p2)
	if len(p2.Messages) != 1 || p2.Messages[0].Text != "hi there" || p2.HasMore {
		t.Fatalf("page2 = %#v has_more=%v", p2.Messages, p2.HasMore)
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
