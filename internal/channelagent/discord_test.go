package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscordSourceFetchesChannelMessages(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v10/channels/c1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Fatalf("limit = %s, want 50", r.URL.Query().Get("limit"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id":        "m1",
			"content":   "hello",
			"timestamp": "2026-06-16T01:30:12.000000+00:00",
			"author":    map[string]any{"id": "u1"},
			"attachments": []map[string]any{{
				"id": "a1", "url": "https://cdn.example/a.png", "content_type": "image/png",
			}},
		}})
	}))
	defer server.Close()

	messages, err := DiscordSource{BaseURL: server.URL + "/api/v10", Token: "tok", ChannelID: "c1", Limit: 50}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bot tok" {
		t.Fatalf("Authorization = %q, want Bot tok", gotAuth)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	got := messages[0]
	if got.Platform != "discord" || got.ChannelID != "c1" || got.MessageID != "m1" || got.AuthorID != "u1" || got.Content != "hello" {
		t.Fatalf("mapped message = %#v", got)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].URL != "https://cdn.example/a.png" {
		t.Fatalf("attachments = %#v", got.Attachments)
	}
}

func TestDiscordSenderPostsMessage(t *testing.T) {
	var gotAuth string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v10/channels/c1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"sent"}`))
	}))
	defer server.Close()

	err := DiscordSender{BaseURL: server.URL + "/api/v10", Token: "tok", ChannelID: "c1"}.Send(context.Background(), OutputJob{Send: true, Text: "reply"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bot tok" {
		t.Fatalf("Authorization = %q, want Bot tok", gotAuth)
	}
	if gotBody["content"] != "reply" {
		t.Fatalf("content = %q, want reply", gotBody["content"])
	}
}
