package channelagent

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"testing"
)

// Two Telegram push bindings share one port (one webhookServer), routed by
// path — the multi-binding port-conflict fix.
func TestPushManagerSharedWebhookServerTwoBindings(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "a", ".channel-agent")
	rootB := filepath.Join(t.TempDir(), "b", ".channel-agent")
	addr := "127.0.0.1:18811"

	mgr := NewPushManager(context.Background())
	defer mgr.StopAll()
	mgr.EnsureWebhook("a", addr, "/tg/111", TelegramWebhookHandler{Root: rootA, ChatID: "111"}, nil)
	mgr.EnsureWebhook("b", addr, "/tg/222", TelegramWebhookHandler{Root: rootB, ChatID: "222"}, nil)

	post := func(path, body string) int {
		var code int
		waitFor(t, func() bool {
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader([]byte(body)))
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			code = r.StatusCode
			r.Body.Close()
			return true
		})
		return code
	}

	bodyA := `{"update_id":1,"message":{"message_id":1,"date":1750000000,"text":"a","chat":{"id":111},"from":{"id":9}}}`
	bodyB := `{"update_id":2,"message":{"message_id":2,"date":1750000000,"text":"b","chat":{"id":222},"from":{"id":9}}}`
	if c := post("/tg/111", bodyA); c != http.StatusOK {
		t.Fatalf("POST a = %d", c)
	}
	if c := post("/tg/222", bodyB); c != http.StatusOK {
		t.Fatalf("POST b = %d", c)
	}
	waitFor(t, func() bool { return countJSONFilesSafe(filepath.Join(rootA, "inbox", "pending")) == 1 })
	waitFor(t, func() bool { return countJSONFilesSafe(filepath.Join(rootB, "inbox", "pending")) == 1 })

	// Reconcile drops binding b → its route 404s, a still serves.
	mgr.Reconcile(map[string]bool{"a": true})
	if mgr.WebhookRegistered("b") {
		t.Fatal("b should be unregistered")
	}
	if c := post("/tg/222", bodyB); c != http.StatusNotFound {
		t.Fatalf("POST b after reconcile = %d, want 404", c)
	}
}
