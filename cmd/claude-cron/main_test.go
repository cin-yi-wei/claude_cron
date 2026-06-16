package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "claude_cron/internal/channelagent"
)

func TestRunInitCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")

	if code := run([]string{"init", "--root", root}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("run init exit = %d, want 0", code)
	}

	assertExists(t, filepath.Join(root, "inbox", "pending"))
}

func TestRunVersionCommand(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"version"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run version exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "claude-cron ") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunInitDiscordCommandWritesConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")

	code := run([]string{"init", "discord", "--root", root, "--discord-channel-id", "c1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run init discord exit = %d, want 0", code)
	}
	cfg, err := agent.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Platform != "discord" || cfg.Discord.ChannelID != "c1" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestRunDoctorCommandChecksConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	t.Setenv("DISCORD_TEST_TOKEN", "tok")
	cfg, err := agent.DefaultConfig("discord")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Discord.TokenEnv = "DISCORD_TEST_TOKEN"
	cfg.Discord.ChannelID = "c1"
	if err := agent.SaveConfig(root, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	var stdout bytes.Buffer
	code := run([]string{"doctor", "--root", root}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run doctor exit = %d, want 0", code)
	}
	if stdout.String() != "doctor=ok\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunServeOnceCommandUsesConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	cfg, err := agent.DefaultConfig("mock")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Mock.SourcePath = filepath.Join(root, "mock", "source_messages.json")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := agent.AtomicWriteJSON(cfg.Mock.SourcePath, []agent.SourceMessage{}); err != nil {
		t.Fatalf("write mock source: %v", err)
	}
	if err := agent.SaveConfig(root, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	var stdout bytes.Buffer
	code := run([]string{"serve", "--root", root, "--once"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run serve once exit = %d, want 0", code)
	}
	// The supervisor always runs (exit 0). With no bindings registered and no
	// real Discord token the control channel logs a "control error" line to
	// stdout; that is expected and non-fatal.
}

func TestRunWatcherCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	sourcePath := filepath.Join(root, "mock", "source_messages.json")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := agent.AtomicWriteJSON(sourcePath, []agent.SourceMessage{{
		Platform: "mock", ChannelID: "local", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T01:30:12+08:00", Content: "hi",
	}}); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var stdout bytes.Buffer
	if code := run([]string{"watcher", "--root", root, "--source", sourcePath}, &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("run watcher exit = %d, want 0", code)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
}

func TestRunWatcherDiscordCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	t.Setenv("DISCORD_TEST_TOKEN", "tok")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v10/channels/c1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "m1", "content": "hi", "timestamp": "2026-06-16T01:30:12Z", "author": map[string]any{"id": "u1"},
		}})
	}))
	defer server.Close()

	code := run([]string{
		"watcher",
		"--root", root,
		"--source-adapter", "discord",
		"--discord-base-url", server.URL + "/api/v10",
		"--discord-token-env", "DISCORD_TEST_TOKEN",
		"--discord-channel-id", "c1",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run watcher discord exit = %d, want 0", code)
	}
	if got := countJSONFiles(t, filepath.Join(root, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
}

func TestRunSenderTelegramCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	t.Setenv("TELEGRAM_TEST_TOKEN", "TOKEN")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := agent.AtomicWriteJSON(filepath.Join(root, "outbox", "pending", "job-1.json"), agent.OutputJob{
		Schema: 1, JobID: "job-1", RequestID: "req-1", InputHash: "hash-1", Send: true, Text: "reply",
	}); err != nil {
		t.Fatalf("write output: %v", err)
	}
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	code := run([]string{
		"sender",
		"--root", root,
		"--adapter", "telegram",
		"--telegram-base-url", server.URL,
		"--telegram-token-env", "TELEGRAM_TEST_TOKEN",
		"--telegram-chat-id", "12345",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run sender telegram exit = %d, want 0", code)
	}
	if gotBody["chat_id"] != "12345" || gotBody["text"] != "reply" {
		t.Fatalf("telegram body = %#v", gotBody)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func countJSONFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count
}

func TestListSubcommandPrintsBindings(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	reg := agent.Registry{}
	_ = reg.Add(agent.BindingDefaults(root, "proj-a", "/p/a", "dev"))
	if err := agent.SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"list", "--root", root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proj-a") {
		t.Fatalf("stdout = %q, want it to list proj-a", stdout.String())
	}
}

func TestBindSubcommandRejectsBadName(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, _ := agent.DefaultConfig("discord")
	cfg.Discord.ChannelID = "c1"
	cfg.Discord.GuildID = "g1"
	_ = agent.SaveConfig(root, cfg)

	var stdout, stderr bytes.Buffer
	code := run([]string{"bind", "Bad_Name", "/p/a", "dev", "--root", root}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for bad name; stdout=%s", stdout.String())
	}
	if !strings.Contains(stdout.String()+stderr.String(), "name") {
		t.Fatalf("expected name error, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
