package channelagent

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// adminConfigDTO is the GET /api/config view: editable settings with NO secret
// values (secrets are reported only as "set" booleans + non-secret fields).
type adminConfigDTO struct {
	DiscordTransport  string `json:"discord_transport"`
	TelegramTransport string `json:"telegram_transport"`
	PushListen        string `json:"push_listen"`
	PushPublicURL     string `json:"push_public_url"`
	PushSecretSet     bool   `json:"push_secret_set"`
	TelegramChatID    string `json:"telegram_chat_id"`
}

// adminConfigUpdate is the PUT /api/config body: pointer fields so only the
// provided ones are changed. Token (.env) editing is a later step (B2).
type adminConfigUpdate struct {
	DiscordTransport  *string `json:"discord_transport"`
	TelegramTransport *string `json:"telegram_transport"`
	PushListen        *string `json:"push_listen"`
	PushPublicURL     *string `json:"push_public_url"`
	PushSecret        *string `json:"push_secret"`
	TelegramChatID    *string `json:"telegram_chat_id"`
}

func (h AdminHandler) getConfig(w http.ResponseWriter) {
	cfg, err := LoadConfig(h.Root)
	if err != nil {
		http.Error(w, "config error", http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, adminConfigDTO{
		DiscordTransport:  cfg.DiscordTransport(),
		TelegramTransport: cfg.TelegramTransport(),
		PushListen:        cfg.Push.Listen,
		PushPublicURL:     cfg.Push.PublicURL,
		PushSecretSet:     cfg.Push.Secret != "",
		TelegramChatID:    cfg.Control.TelegramChatID,
	})
}

func validTransport(platform, t string) bool {
	switch platform {
	case PlatformDiscord:
		return t == TransportGateway || t == TransportPoll
	case PlatformTelegram:
		return t == TransportWebhook || t == TransportPoll
	}
	return false
}

func (h AdminHandler) putConfig(w http.ResponseWriter, r *http.Request) {
	if h.Deps == nil {
		http.Error(w, "writes disabled", http.StatusServiceUnavailable)
		return
	}
	var up adminConfigUpdate
	if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg, err := LoadConfig(h.Root)
	if err != nil {
		http.Error(w, "config error", http.StatusInternalServerError)
		return
	}
	if up.DiscordTransport != nil {
		if !validTransport(PlatformDiscord, *up.DiscordTransport) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("discord transport must be %s or %s", TransportGateway, TransportPoll)})
			return
		}
		cfg.Discord.Transport = *up.DiscordTransport
		cfg.Discord.GatewayDemux = false // enum is now authoritative; drop legacy bool
	}
	if up.TelegramTransport != nil {
		if !validTransport(PlatformTelegram, *up.TelegramTransport) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("telegram transport must be %s or %s", TransportWebhook, TransportPoll)})
			return
		}
		cfg.Telegram.Transport = *up.TelegramTransport
		cfg.Telegram.Webhook = false
	}
	if up.PushListen != nil {
		cfg.Push.Listen = *up.PushListen
	}
	if up.PushPublicURL != nil {
		cfg.Push.PublicURL = *up.PushPublicURL
	}
	if up.PushSecret != nil {
		cfg.Push.Secret = *up.PushSecret
	}
	if up.TelegramChatID != nil {
		cfg.Control.TelegramChatID = *up.TelegramChatID
	}
	if err := SaveConfig(h.Root, cfg); err != nil {
		http.Error(w, "save error", http.StatusInternalServerError)
		return
	}
	// Config is read at serve startup; apply by restarting (async, after the
	// response flushes — the restart kills this very process).
	restarting := h.RestartServe != nil
	writeJSONResponse(w, map[string]any{"result": "saved", "restarting": restarting})
	if restarting {
		go h.RestartServe()
	}
}
