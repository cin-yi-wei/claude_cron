package channelagent

import (
	"strings"
	"testing"
)

func TestTelegramHTML(t *testing.T) {
	in := "⏳ 🔧 Edit a.go\n```diff\n- a := 1\n+ a := 2\n```\ndone <x> & y"
	got := telegramHTML(in)
	if !strings.Contains(got, `<pre><code class="language-diff">`) {
		t.Fatalf("missing diff code block: %q", got)
	}
	if !strings.Contains(got, "- a := 1\n+ a := 2") {
		t.Fatalf("diff body lost: %q", got)
	}
	// Text outside fences must be HTML-escaped.
	if !strings.Contains(got, "done &lt;x&gt; &amp; y") {
		t.Fatalf("outside text not escaped: %q", got)
	}
	// No raw backtick fences left.
	if strings.Contains(got, "```") {
		t.Fatalf("raw fence leaked: %q", got)
	}
}
