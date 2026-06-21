package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramSourceFetchesUpdates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/botTOKEN/getUpdates" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("timeout") != "0" {
			t.Fatalf("timeout = %s, want 0", r.URL.Query().Get("timeout"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{{
				"update_id": 101,
				"message": map[string]any{
					"message_id": 7,
					"date":       1781568000,
					"text":       "hello tg",
					"chat":       map[string]any{"id": 12345},
					"from":       map[string]any{"id": 67890},
				},
			}},
		})
	}))
	defer server.Close()

	messages, err := TelegramSource{BaseURL: server.URL, Token: "TOKEN", ChatID: "12345"}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	got := messages[0]
	if got.Platform != "telegram" || got.ChannelID != "12345" || got.MessageID != "101" || got.AuthorID != "67890" || got.Content != "hello tg" {
		t.Fatalf("mapped message = %#v", got)
	}
}

func TestTelegramSenderPostsMessage(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 8}})
	}))
	defer server.Close()

	err := TelegramSender{BaseURL: server.URL, Token: "TOKEN", ChatID: "12345"}.Send(context.Background(), OutputJob{Send: true, Text: "reply tg"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotBody["chat_id"] != "12345" || gotBody["text"] != "reply tg" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestTelegramExtractCapturesPhoto(t *testing.T) {
	up := telegramUpdate{
		UpdateID: 7,
		Message: &telegramMessage{
			MessageID: 1, Date: 1700000000,
			Caption: "看這個",
			Chat:    telegramChat{ID: 555},
			From:    telegramUser{ID: 9},
			Photo: []telegramPhotoSize{
				{FileID: "small", FileSize: 100},
				{FileID: "big", FileSize: 9000},
			},
		},
	}
	msg, ok := telegramExtract(up)
	if !ok {
		t.Fatal("extract ok=false")
	}
	if msg.Content != "看這個" {
		t.Fatalf("content = %q", msg.Content)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ID != "big" || msg.Attachments[0].Type != "image/jpeg" {
		t.Fatalf("attachments = %#v (want largest photo 'big')", msg.Attachments)
	}
	if msg.Attachments[0].URL != "" {
		t.Fatalf("URL should be empty pre-resolve, got %q", msg.Attachments[0].URL)
	}
}
