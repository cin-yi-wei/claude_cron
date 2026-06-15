package main

import (
	"bytes"
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
