package channelagent

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"testing"
)

// TestPushTelegramEndToEndToInbox exercises the real push path: PushManager
// starts a TelegramWebhookIngester on a local port, a POST (as Telegram would
// send) hits the running server, and a job lands in the binding's inbox.
func TestPushTelegramEndToEndToInbox(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	addr := "127.0.0.1:18799"
	chat := "12345"

	ing := TelegramWebhookIngester{
		Addr:    addr,
		Path:    "/tg/" + chat,
		Handler: TelegramWebhookHandler{Root: root, ChatID: chat, Secret: "sek"},
		// PublicURL empty → no setWebhook call (local-only), the "fake" bring-up.
	}

	mgr := NewPushManager(context.Background())
	defer mgr.StopAll()
	mgr.Ensure("tgbind", ing, nil)

	url := "http://" + addr + "/tg/" + chat
	// Wait until the server is accepting connections, then POST an update.
	var resp *http.Response
	waitFor(t, func() bool {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(tgUpdateBody)))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "sek")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp = r
		return true
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}

	waitFor(t, func() bool {
		return countJSONFiles(t, filepath.Join(root, "inbox", "pending")) == 1
	})
}
