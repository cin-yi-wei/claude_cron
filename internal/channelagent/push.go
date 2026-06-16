package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PushIngester is the active/push counterpart to PollIngester: instead of being
// polled once per cycle, it runs continuously (until ctx is cancelled),
// receiving events out-of-band and writing them to the inbox as they arrive.
// The per-cycle worker/sender then drain the inbox/outbox as usual.
type PushIngester interface {
	Run(ctx context.Context) error
}

// TelegramWebhookHandler is the http.Handler half of Telegram push: Telegram
// POSTs Update objects to the webhook URL, and this handler validates the
// secret, maps the update to a SourceMessage for its chat, and writes it to the
// inbox via IngestMessages (same dedup/locking as poll).
//
// It is a plain http.Handler so it can be unit-tested with httptest and mounted
// on a shared server (one server, many bindings keyed by path) by the caller.
type TelegramWebhookHandler struct {
	Root   string
	ChatID string
	// Secret, when non-empty, must match the X-Telegram-Bot-Api-Secret-Token
	// header Telegram sends (set at setWebhook time). Rejects spoofed posts.
	Secret string
}

func (h TelegramWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Secret != "" && r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != h.Secret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var update telegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if msg, ok := telegramUpdateToMessage(update, h.ChatID); ok {
		if _, err := IngestMessages(r.Context(), h.Root, []SourceMessage{msg}); err != nil {
			http.Error(w, "ingest failed", http.StatusInternalServerError)
			return
		}
	}
	// Always 200 for a well-formed, authorized post (even if filtered out for a
	// different chat) so Telegram does not retry indefinitely.
	w.WriteHeader(http.StatusOK)
}

// TelegramWebhookIngester is a PushIngester that runs an HTTP server hosting a
// TelegramWebhookHandler. For a single binding; multi-binding sharing of one
// port is the manager's job (a later step).
type TelegramWebhookIngester struct {
	Addr    string // listen address, e.g. ":8443"
	Path    string // URL path Telegram is configured to POST to, e.g. "/tg/<chat>"
	Handler TelegramWebhookHandler
}

func (t TelegramWebhookIngester) Run(ctx context.Context) error {
	if t.Path == "" {
		return fmt.Errorf("telegram webhook ingester: path is required")
	}
	mux := http.NewServeMux()
	mux.Handle(t.Path, t.Handler)
	srv := &http.Server{Addr: t.Addr, Handler: mux}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errc:
		return err
	}
}
