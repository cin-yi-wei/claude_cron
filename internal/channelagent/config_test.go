package channelagent

import (
	"path/filepath"
	"testing"
)

func TestDefaultConfigForDiscord(t *testing.T) {
	cfg, err := DefaultConfig("discord")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	if cfg.Platform != "discord" {
		t.Fatalf("Platform = %q, want discord", cfg.Platform)
	}
	if cfg.Discord.TokenEnv != "DISCORD_BOT_TOKEN" {
		t.Fatalf("Discord token env = %q", cfg.Discord.TokenEnv)
	}
	if cfg.Claude.TmuxSession != "claude-cron" {
		t.Fatalf("tmux session = %q", cfg.Claude.TmuxSession)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	cfg, err := DefaultConfig("telegram")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Telegram.ChatID = "12345"

	if err := SaveConfig(root, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Platform != "telegram" || got.Telegram.ChatID != "12345" {
		t.Fatalf("loaded config = %#v", got)
	}
}
