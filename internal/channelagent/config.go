package channelagent

import (
	"fmt"
	"path/filepath"
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
}

type TelegramConfig struct {
	TokenEnv string `json:"token_env"`
	ChatID   string `json:"chat_id"`
	BaseURL  string `json:"base_url,omitempty"`
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
