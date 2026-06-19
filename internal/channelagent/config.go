package channelagent

import (
	"fmt"
	"path/filepath"
	"time"
)

type Config struct {
	Platform     string         `json:"platform"`
	PollInterval string         `json:"poll_interval"`
	Mock         MockConfig     `json:"mock"`
	Discord      DiscordConfig  `json:"discord"`
	Telegram     TelegramConfig `json:"telegram"`
	Claude       ClaudeConfig   `json:"claude"`
	Push         PushConfig     `json:"push,omitempty"`
	Admin        AdminConfig    `json:"admin,omitempty"`
	Control      ControlConfig  `json:"control,omitempty"`
	// IdleSleepMinutes: a binding idle this long auto-sleeps (session killed to
	// free RAM; auto-wakes on next message). 0 = default (30 min); <0 = disabled.
	IdleSleepMinutes int `json:"idle_sleep_minutes,omitempty"`
	// StallMinutes: a session with queued work but no transcript progress for this
	// long is treated as stuck and restarted. 0 = default (10 min); <0 = disabled.
	StallMinutes int `json:"stall_minutes,omitempty"`
}

// IdleSleepTimeout resolves the auto-sleep idle threshold. Zero return = feature
// disabled (the supervisor only sleeps when timeout > 0).
func (c Config) IdleSleepTimeout() time.Duration {
	m := c.IdleSleepMinutes
	if m == 0 {
		m = 30 // default: sleep after 30 min idle
	}
	if m < 0 {
		return 0 // explicitly disabled
	}
	return time.Duration(m) * time.Minute
}

// StallTimeout resolves the stall-watchdog threshold. Zero return = disabled.
func (c Config) StallTimeout() time.Duration {
	m := c.StallMinutes
	if m == 0 {
		m = 10 // default: restart after 10 min of no transcript progress
	}
	if m < 0 {
		return 0
	}
	return time.Duration(m) * time.Minute
}

// ControlConfig configures the control channel's own ingestion. Mode "push"
// adds a Discord Gateway feed on top of polling (poll always runs as a backstop
// — the control channel is the lifeline); empty/"poll" is plain polling.
//
// TelegramChatID, when set, enables a SECOND, isolated control plane on Telegram
// (same bot as worker bindings, distinguished by chat id). A user talking to that
// chat can /bind /list /pause etc. but only sees bindings their own plane owns.
type ControlConfig struct {
	Mode           string `json:"mode,omitempty"`
	TelegramChatID string `json:"telegram_chat_id,omitempty"`
}

// ControlPlane identifies one control entrance: its namespace Name (used to tag
// and filter the bindings it owns), the Platform new bindings default to, and the
// channel/chat id it listens on. The Discord plane is named "discord" and keeps
// the legacy cc-control session + root/control paths; others are suffixed.
type ControlPlane struct {
	Name      string
	Platform  string
	ChannelID string
}

// Transport returns a binding's ACTUAL message transport, derived from the
// global demux flags rather than the per-binding mode. Under demux (the current
// model) every binding of a platform shares one connection, so the legacy
// per-binding mode (poll/push) no longer drives ingestion — it's only the
// fallback when demux is off. This keeps display/behaviour aligned with reality.
func (c Config) Transport(b Binding) string {
	switch b.PlatformOf() {
	case PlatformDiscord:
		return c.DiscordTransport()
	case PlatformTelegram:
		return c.TelegramTransport()
	case PlatformWeb:
		return "browser"
	}
	return b.ModeOf()
}

// ControlPlanes returns the configured control entrances: always the Discord
// plane (back-compat), plus a Telegram plane when ControlConfig.TelegramChatID is
// set. Token resolution and source/sender selection happen in the supervisor.
func (c Config) ControlPlanes() []ControlPlane {
	planes := []ControlPlane{{Name: PlatformDiscord, Platform: PlatformDiscord, ChannelID: c.Discord.ChannelID}}
	if c.Control.TelegramChatID != "" {
		planes = append(planes, ControlPlane{Name: PlatformTelegram, Platform: PlatformTelegram, ChannelID: c.Control.TelegramChatID})
	}
	return planes
}

// AdminConfig configures the admin HTTP API. Token, when set, is required as a
// Bearer token on every request. Binding a non-loopback Listen without a Token
// is refused (the API can create/delete shell-capable sessions).
type AdminConfig struct {
	Listen string `json:"listen,omitempty"`
	Token  string `json:"token,omitempty"`
}

// PushConfig configures push-mode (active) ingestion. Listen is the local
// address the webhook/HTTP server binds (e.g. ":8443"); Secret, when set, is
// the token Telegram echoes in X-Telegram-Bot-Api-Secret-Token. All optional;
// poll-mode bindings ignore this block entirely.
type PushConfig struct {
	Listen string `json:"listen,omitempty"`
	Secret string `json:"secret,omitempty"`
	// PublicURL is the externally reachable base URL Telegram POSTs to (the
	// binding's path is appended). Empty disables webhook registration, so the
	// server still runs locally but Telegram is not told to push.
	PublicURL string `json:"public_url,omitempty"`
}

type MockConfig struct {
	SourcePath string `json:"source_path"`
}

type DiscordConfig struct {
	TokenEnv  string `json:"token_env"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// Transport selects how the whole bot ingests: "gateway" (single shared
	// Gateway websocket, demuxed by channel id) or "poll" (per-binding REST poll).
	// This is the authoritative knob; GatewayDemux is the legacy boolean kept only
	// as a fallback for old configs (see DiscordTransport).
	Transport string `json:"transport,omitempty"`
	// Deprecated: use Transport. Read only as a fallback when Transport is empty.
	GatewayDemux bool `json:"gateway_demux,omitempty"`
}

const (
	TransportGateway = "gateway"
	TransportWebhook = "webhook"
	TransportPoll    = "poll"
)

// DiscordTransport resolves the Discord ingestion transport: the explicit
// Transport enum if set, else the legacy GatewayDemux boolean, else "gateway"
// (the demux model is the default).
func (c Config) DiscordTransport() string {
	if c.Discord.Transport != "" {
		return c.Discord.Transport
	}
	if c.Discord.GatewayDemux {
		return TransportGateway
	}
	return TransportGateway
}

type TelegramConfig struct {
	TokenEnv string `json:"token_env"`
	ChatID   string `json:"chat_id"`
	BaseURL  string `json:"base_url,omitempty"`
	// Transport: "webhook" (single demux endpoint) or "poll" (shared getUpdates
	// reader). Authoritative; Webhook is the legacy boolean fallback.
	Transport string `json:"transport,omitempty"`
	// Deprecated: use Transport. Read only as a fallback when Transport is empty.
	Webhook bool `json:"webhook,omitempty"`
}

// TelegramTransport resolves the Telegram ingestion transport: the explicit
// Transport enum if set, else the legacy Webhook boolean, else "poll".
func (c Config) TelegramTransport() string {
	if c.Telegram.Transport != "" {
		return c.Telegram.Transport
	}
	if c.Telegram.Webhook {
		return TransportWebhook
	}
	return TransportPoll
}

type ClaudeConfig struct {
	TmuxSession string `json:"tmux_session"`
	Timeout     string `json:"timeout"`
	AutoStart   bool   `json:"auto_start"`
}

func DefaultConfig(platform string) (Config, error) {
	if platform == "" {
		platform = "mock"
	}
	switch platform {
	case "mock", "discord", "telegram", "tg":
	default:
		return Config{}, fmt.Errorf("unsupported platform %q", platform)
	}
	if platform == "tg" {
		platform = "telegram"
	}
	return Config{
		Platform:     platform,
		PollInterval: "10s",
		Mock: MockConfig{
			SourcePath: ".channel-agent/mock/source_messages.json",
		},
		Discord: DiscordConfig{
			TokenEnv: "DISCORD_BOT_TOKEN",
		},
		Telegram: TelegramConfig{
			TokenEnv: "TELEGRAM_BOT_TOKEN",
		},
		Claude: ClaudeConfig{
			TmuxSession: "claude-cron",
			Timeout:     "120s",
			AutoStart:   true,
		},
	}, nil
}

func ConfigPath(root string) string {
	return filepath.Join(root, "config.json")
}

func SaveConfig(root string, cfg Config) error {
	if err := Init(root); err != nil {
		return err
	}
	return AtomicWriteJSON(ConfigPath(root), cfg)
}

func LoadConfig(root string) (Config, error) {
	var cfg Config
	if err := ReadJSON(ConfigPath(root), &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
