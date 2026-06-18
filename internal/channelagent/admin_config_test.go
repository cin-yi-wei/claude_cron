package channelagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func adminCfgHandler(t *testing.T, root string, restarted chan struct{}) AdminHandler {
	t.Helper()
	cfg, _ := DefaultConfig("discord")
	cfg.Discord.Transport = TransportGateway
	cfg.Telegram.Transport = TransportWebhook
	cfg.Push.Secret = "topsecret"
	cfg.Push.PublicURL = "https://x.example/tg"
	cfg.Control.TelegramChatID = "123"
	if err := SaveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	deps := ControlDeps{}
	return AdminHandler{Root: root, Deps: &deps, RestartServe: func() {
		select {
		case restarted <- struct{}{}:
		default:
		}
	}}
}

func TestAdminGetConfigMasksSecret(t *testing.T) {
	root := t.TempDir() + "/.channel-agent"
	h := adminCfgHandler(t, root, make(chan struct{}, 1))
	rr := httptest.NewRecorder()
	h.getConfig(rr)
	var dto adminConfigDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body)
	}
	if dto.DiscordTransport != TransportGateway || dto.TelegramTransport != TransportWebhook {
		t.Fatalf("transport = %#v", dto)
	}
	if !dto.PushSecretSet {
		t.Fatal("push_secret_set should be true")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("topsecret")) {
		t.Fatalf("secret leaked in GET config: %s", rr.Body)
	}
}

func TestAdminPutConfigUpdatesAndRestarts(t *testing.T) {
	root := t.TempDir() + "/.channel-agent"
	restarted := make(chan struct{}, 1)
	h := adminCfgHandler(t, root, restarted)

	body, _ := json.Marshal(map[string]any{"discord_transport": "poll", "push_public_url": "https://new.example/tg"})
	rr := httptest.NewRecorder()
	h.putConfig(rr, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body)
	}
	cfg, _ := LoadConfig(root)
	if cfg.DiscordTransport() != TransportPoll {
		t.Fatalf("discord transport not saved: %q", cfg.DiscordTransport())
	}
	if cfg.Push.PublicURL != "https://new.example/tg" {
		t.Fatalf("public_url not saved: %q", cfg.Push.PublicURL)
	}
	if cfg.Push.Secret != "topsecret" {
		t.Fatalf("secret clobbered: %q", cfg.Push.Secret)
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("RestartServe not called after save")
	}
}

func TestAdminPutConfigRejectsBadTransport(t *testing.T) {
	root := t.TempDir() + "/.channel-agent"
	restarted := make(chan struct{}, 1)
	h := adminCfgHandler(t, root, restarted)
	body, _ := json.Marshal(map[string]any{"discord_transport": "webhook"})
	rr := httptest.NewRecorder()
	h.putConfig(rr, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	select {
	case <-restarted:
		t.Fatal("should not restart on validation failure")
	case <-time.After(200 * time.Millisecond):
	}
}
