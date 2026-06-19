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

func TestActivityMessage(t *testing.T) {
	if activityMessage(nil) != "" {
		t.Fatal("empty lines should give empty message")
	}
	got := activityMessage([]string{"🔧 Edit a.go", "▶ go test"})
	if !strings.HasPrefix(got, "⏳ ") || !strings.Contains(got, "🔧 Edit a.go") || !strings.Contains(got, "▶ go test") {
		t.Fatalf("activityMessage = %q", got)
	}
}
