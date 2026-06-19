package channelagent

import (
	"strings"
	"testing"
)

func TestDiscordColorDiff(t *testing.T) {
	in := "⏳ 🔧 Edit a.go\n```diff\n- old line\n+ new line\n```"
	got := discordColorDiff(in)
	if !strings.Contains(got, "```ansi\n") {
		t.Fatalf("expected ansi block: %q", got)
	}
	if !strings.Contains(got, "\x1b[31m- old line\x1b[0m") {
		t.Fatalf("red − missing: %q", got)
	}
	if !strings.Contains(got, "\x1b[32m+ new line\x1b[0m") {
		t.Fatalf("green + missing: %q", got)
	}
	if strings.Contains(got, "```diff") {
		t.Fatalf("diff fence should be gone: %q", got)
	}
	// No diff block → unchanged.
	plain := "⏳ ▶ go test ./..."
	if discordColorDiff(plain) != plain {
		t.Fatalf("plain text changed")
	}
}
