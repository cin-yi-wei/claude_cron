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
