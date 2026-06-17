package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestTelegramReaderRoutesByChatAndAdvancesOffset(t *testing.T) {
	var gotOffset string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOffset = r.URL.Query().Get("offset")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{
				{"update_id": 100, "message": map[string]any{"message_id": 1, "date": 1781568000, "text": "to-binding", "chat": map[string]any{"id": 111}, "from": map[string]any{"id": 9}}},
				{"update_id": 101, "message": map[string]any{"message_id": 2, "date": 1781568001, "text": "to-control", "chat": map[string]any{"id": 222}, "from": map[string]any{"id": 9}}},
				{"update_id": 102, "message": map[string]any{"message_id": 3, "date": 1781568002, "text": "unrouted", "chat": map[string]any{"id": 999}, "from": map[string]any{"id": 9}}},
			},
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	offPath := filepath.Join(dir, "off.json")
	var binMsgs, ctlMsgs []SourceMessage
	routes := map[string]func(context.Context, SourceMessage) error{
		"111": func(_ context.Context, m SourceMessage) error { binMsgs = append(binMsgs, m); return nil },
		"222": func(_ context.Context, m SourceMessage) error { ctlMsgs = append(ctlMsgs, m); return nil },
	}

	r := TelegramReader{BaseURL: server.URL, Token: "TOKEN", OffsetPath: offPath}
	if err := r.Drain(context.Background(), routes); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(binMsgs) != 1 || binMsgs[0].Content != "to-binding" {
		t.Fatalf("binding route = %#v", binMsgs)
	}
	if len(ctlMsgs) != 1 || ctlMsgs[0].Content != "to-control" {
		t.Fatalf("control route = %#v", ctlMsgs)
	}
	// chat 999 had no route → dropped (not an error).

	// Offset persisted past the max update_id (102 → 103).
	var off tgOffset
	if err := ReadJSON(offPath, &off); err != nil {
		t.Fatalf("read offset: %v", err)
	}
	if off.Offset != 103 {
		t.Fatalf("offset = %d, want 103", off.Offset)
	}

	// A second Drain sends the persisted offset so consumed updates aren't re-fetched.
	_ = r.Drain(context.Background(), routes)
	if gotOffset != "103" {
		t.Fatalf("second Drain offset param = %q, want 103", gotOffset)
	}
}

func TestTelegramReaderRetriesOnDeliveryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{
				{"update_id": 50, "message": map[string]any{"message_id": 1, "date": 1781568000, "text": "x", "chat": map[string]any{"id": 5}, "from": map[string]any{"id": 1}}},
			},
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	offPath := filepath.Join(dir, "off.json")
	routes := map[string]func(context.Context, SourceMessage) error{
		"5": func(_ context.Context, _ SourceMessage) error { return context.DeadlineExceeded },
	}
	r := TelegramReader{BaseURL: server.URL, Token: "TOKEN", OffsetPath: offPath}
	if err := r.Drain(context.Background(), routes); err == nil {
		t.Fatal("expected delivery error to propagate")
	}
	// Offset must NOT advance on delivery failure (so the message is retried).
	var off tgOffset
	_ = ReadJSON(offPath, &off)
	if off.Offset != 0 {
		t.Fatalf("offset advanced to %d despite delivery failure", off.Offset)
	}
}

func TestTelegramBufferSourceFetchClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buf.json")
	m := SourceMessage{Platform: "telegram", ChannelID: "1", MessageID: "10", Content: "hi"}
	if err := appendTelegramBuffer(path, m); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendTelegramBuffer(path, SourceMessage{ChannelID: "1", MessageID: "11", Content: "yo"}); err != nil {
		t.Fatalf("append2: %v", err)
	}
	src := TelegramBufferSource{Path: path}
	got, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 || got[0].Content != "hi" || got[1].Content != "yo" {
		t.Fatalf("fetch = %#v", got)
	}
	// Second fetch is empty (buffer cleared).
	got2, _ := src.Fetch(context.Background())
	if len(got2) != 0 {
		t.Fatalf("buffer not cleared: %#v", got2)
	}
}
