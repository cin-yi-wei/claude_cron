package channelagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFormatToolUse(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"Bash", `{"command":"go test ./..."}`, "▶ go test ./..."},
		{"Edit", `{"file_path":"/a/b/auth.go"}`, "🔧 Edit auth.go"},
		{"Write", `{"file_path":"/x/main.go"}`, "📝 Write main.go"},
		{"Grep", `{"pattern":"func main"}`, "🔍 Grep func main"},
		{"WebFetch", `{"url":"https://x.dev"}`, "🌐 Fetch https://x.dev"},
		{"Frobnicate", `{}`, "🔧 Frobnicate"},
		{"Edit", `{"file_path":"/x/a.go","old_string":"a := 1","new_string":"a := 2"}`, "🔧 Edit a.go\n```diff\n- a := 1\n+ a := 2\n```"},
		{"Write", `{"file_path":"/x/n.go","content":"package main"}`, "📝 Write n.go\n```\npackage main\n```"},
	}
	for _, c := range cases {
		got := formatToolUse(c.name, json.RawMessage(c.input))
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestCondense(t *testing.T) {
	if got := condense("  hello\n  world  ", 100); got != "hello world" {
		t.Fatalf("condense collapse = %q", got)
	}
	if got := condense(strings.Repeat("a", 50), 10); got != strings.Repeat("a", 10)+"…" {
		t.Fatalf("condense truncate = %q", got)
	}
}

func TestActivityMessages(t *testing.T) {
	if len(activityMessages(nil)) != 0 {
		t.Fatal("empty lines should give no messages")
	}
	got := activityMessages([]string{"🔧 Edit a.go", "▶ go test"})
	if len(got) != 1 {
		t.Fatalf("short activity should be one message, got %d", len(got))
	}
	if !strings.HasPrefix(got[0], "⏳ ") || !strings.Contains(got[0], "🔧 Edit a.go") || !strings.Contains(got[0], "▶ go test") {
		t.Fatalf("activityMessages[0] = %q", got[0])
	}
}

func TestActivityMessagesSplitsInsteadOfTruncating(t *testing.T) {
	// Two large entries that together exceed one message must spill to >1 message,
	// and the full content must survive (no "截斷").
	big := func(tag string) string {
		var b strings.Builder
		b.WriteString("🔧 Edit " + tag + ".go\n```diff\n")
		for i := 0; i < 60; i++ {
			b.WriteString("+ " + tag + " line with some length to add bulk here\n")
		}
		b.WriteString("```")
		return b.String()
	}
	msgs := activityMessages([]string{big("a"), big("b")})
	if len(msgs) < 2 {
		t.Fatalf("expected split into multiple messages, got %d", len(msgs))
	}
	for i, m := range msgs {
		if n := len([]rune(m)); n > activityMsgMax {
			t.Fatalf("message %d over cap: %d > %d", i, n, activityMsgMax)
		}
		if strings.Count(m, "```")%2 != 0 {
			t.Fatalf("message %d has an unbalanced code fence:\n%s", i, m)
		}
	}
	joined := strings.Join(msgs, "\n")
	if strings.Contains(joined, "截斷") {
		t.Fatal("content should be split, not truncated")
	}
}
