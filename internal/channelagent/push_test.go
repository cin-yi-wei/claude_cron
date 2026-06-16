package channelagent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tgUpdateBody = `{"update_id":99,"message":{"message_id":5,"date":1750000000,"text":"hello","chat":{"id":12345},"from":{"id":777}}}`

func TestTelegramWebhookHandlerIngests(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	h := TelegramWebhookHandler{Root: root, ChatID: "12345", Secret: "s3cr3t"}

	req := httptest.NewRequest(http.MethodPost, "/tg/12345", strings.NewReader(tgUpdateBody))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
}

func TestTelegramWebhookHandlerRejectsBadSecret(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	h := TelegramWebhookHandler{Root: root, ChatID: "12345", Secret: "s3cr3t"}

	req := httptest.NewRequest(http.MethodPost, "/tg/12345", strings.NewReader(tgUpdateBody))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestTelegramWebhookHandlerRejectsGet(t *testing.T) {
	h := TelegramWebhookHandler{Root: t.TempDir(), ChatID: "12345"}
	req := httptest.NewRequest(http.MethodGet, "/tg/12345", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestTelegramWebhookHandlerFiltersOtherChat(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	// Handler bound to a different chat than the update carries.
	h := TelegramWebhookHandler{Root: root, ChatID: "99999"}
	req := httptest.NewRequest(http.MethodPost, "/tg/99999", strings.NewReader(tgUpdateBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (accepted but filtered)", rec.Code)
	}
	// Filtered out → IngestMessages never ran, so the inbox dir may not exist.
	// Treat a missing dir as zero jobs.
	entries, _ := os.ReadDir(filepath.Join(root, "inbox", "pending"))
	if len(entries) != 0 {
		t.Fatalf("pending jobs = %d, want 0 (other chat filtered out)", len(entries))
	}
}

// PushIngester is satisfied by TelegramWebhookIngester.
var _ PushIngester = TelegramWebhookIngester{}
