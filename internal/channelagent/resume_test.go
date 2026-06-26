package channelagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncodeProjectDir(t *testing.T) {
	if got := encodeProjectDir("/home/conray/project/fatgame-jfg-4512"); got != "-home-conray-project-fatgame-jfg-4512" {
		t.Fatalf("encode = %q", got)
	}
}

func TestLatestTranscriptPicksNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wt := "/some/work/tree"
	dir := filepath.Join(home, ".claude", "projects", encodeProjectDir(wt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"old.jsonl", "new.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make old.jsonl genuinely older.
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "old.jsonl"), past, past)

	if got := latestTranscript(wt); got != "new" {
		t.Fatalf("latestTranscript = %q, want new", got)
	}
	if got := latestTranscript("/no/such/tree"); got != "" {
		t.Fatalf("missing dir → want empty, got %q", got)
	}
}

func TestLatestTranscriptArchivesOversized(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wt := "/some/work/tree"
	dir := filepath.Join(home, ".claude", "projects", encodeProjectDir(wt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fat := filepath.Join(dir, "fat.jsonl")
	if err := os.WriteFile(fat, make([]byte, maxResumeTranscriptBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	// Oversized → not resumed, and moved out of the project dir.
	if got := latestTranscript(wt); got != "" {
		t.Fatalf("oversized transcript should not resume, got %q", got)
	}
	if _, err := os.Stat(fat); !os.IsNotExist(err) {
		t.Fatalf("oversized transcript should be archived out of project dir")
	}
	archive := filepath.Join(home, ".claude", "projects", "_archive")
	got, err := os.ReadDir(archive)
	if err != nil || len(got) != 1 {
		t.Fatalf("want 1 archived file in _archive, got %v (err %v)", got, err)
	}
	if name := got[0].Name(); !strings.Contains(name, "fat") || !strings.HasSuffix(name, ".jsonl") {
		t.Fatalf("archive name not self-describing: %q", name)
	}
}
