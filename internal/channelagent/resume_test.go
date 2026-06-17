package channelagent

import (
	"os"
	"path/filepath"
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
