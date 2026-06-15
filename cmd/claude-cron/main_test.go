package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
