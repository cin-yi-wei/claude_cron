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

func TestDiscordConfigHasGuildID(t *testing.T) {
	root := t.TempDir()
	cfg, err := DefaultConfig("discord")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Discord.GuildID = "g123"
	if err := SaveConfig(root, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Discord.GuildID != "g123" {
		t.Fatalf("GuildID = %q, want g123", got.Discord.GuildID)
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

// A2AConfig 的 docstring 白紙黑字寫著 Listen MUST differ from the admin
// address，但從來沒有人驗證過。
func TestLoadConfigRejectsA2AOnTheAdminAddress(t *testing.T) {
	root := t.TempDir()
	if err := AtomicWriteJSON(ConfigPath(root), map[string]any{
		"admin": map[string]any{"listen": "127.0.0.1:8787", "token": "t"},
		"a2a":   map[string]any{"enabled": true, "listen": "127.0.0.1:8787"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(root); err == nil {
		t.Fatal("a2a.listen equal to admin.listen must be refused at load time")
	}
}
