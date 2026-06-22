package channelagent

import "testing"

func TestClassifyScreen(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want ScreenState
	}{
		{"glitch", "● court\n<invoke name=\"Read\">\n<parameter name=\"file_path\">x</parameter>", ScreenGlitch},
		{"working_spinner", "✻ Cooking… (1m 4s · ↓ 3.5k tokens)", ScreenWorking},
		{"confirm", "Do you want to proceed?\n❯ 1. Yes\n  2. No", ScreenConfirm},
		{"idle", "some output\n────────\n❯ \n────────", ScreenIdle},
		{"prose_bypass_no_false_positive", "我在解釋 bypass permissions 的設計，不該誤判", ScreenUnknown},
	}
	for _, c := range cases {
		if got := classifyScreen(c.pane); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestClassifyScreenStripsANSI(t *testing.T) {
	pane := "\x1b[0;32m● Cooking… (2s · ↓ 10 tokens)\x1b[0m"
	if got := classifyScreen(pane); got != ScreenWorking {
		t.Fatalf("ansi-wrapped working = %q", got)
	}
}
