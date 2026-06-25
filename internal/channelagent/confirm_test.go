package channelagent

import (
	"context"
	"path/filepath"
	"testing"
)

func TestParseConfirmDialog(t *testing.T) {
	pane := "blah\n Do you want to create SKILL.md?\n ❯ 1. Yes\n   2. Yes, and allow Claude to edit its own settings for this session\n   3. No\n Esc to cancel"
	d, ok := parseConfirmDialog(pane)
	if !ok {
		t.Fatal("expected a confirm dialog")
	}
	if d.Question != "Do you want to create SKILL.md?" {
		t.Fatalf("question = %q", d.Question)
	}
	if len(d.Options) != 3 || d.Options[0] != "Yes" || d.Options[2] != "No" {
		t.Fatalf("options = %#v", d.Options)
	}
	// classifyScreen routes it as a confirm.
	if got := classifyScreen(pane); got != ScreenConfirm {
		t.Fatalf("classifyScreen = %q, want confirm", got)
	}
}

func TestParseConfirmDialogRejectsProse(t *testing.T) {
	// A numbered list with NO selection cursor is not a live confirm prompt.
	if _, ok := parseConfirmDialog("steps:\n1. clone\n2. build\n3. run"); ok {
		t.Fatal("prose numbered list must not parse as a confirm dialog")
	}
}

func TestConfirmChoice(t *testing.T) {
	cases := []struct {
		reply string
		n     int
		want  int
	}{
		{"1", 3, 1}, {"3", 3, 3}, {"2", 3, 2},
		{"y", 3, 1}, {"yes", 2, 1}, {"好", 3, 1},
		{"n", 3, 3}, {"no", 2, 2}, {"拒絕", 3, 3},
		{"4", 3, 0},   // out of range
		{"maybe", 3, 0}, // not a choice
		{"0", 3, 0},
	}
	for _, c := range cases {
		if got := confirmChoice(c.reply, c.n); got != c.want {
			t.Errorf("confirmChoice(%q,%d) = %d, want %d", c.reply, c.n, got, c.want)
		}
	}
}

func TestResolveConfirmReplyTypesChoiceAndArchives(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	var sent []string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		sent = append(sent, args[len(args)-1])
		return nil
	}

	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := seedDecisionJob(t, root, "3") // user picks option 3
	d := confirmDialog{Question: "Do you want to create SKILL.md?", Options: []string{"Yes", "Yes+allow", "No"}}

	if !resolveConfirmReply(context.Background(), root, "cc-x", d) {
		t.Fatal("expected resolveConfirmReply to act on the '3' reply")
	}
	// Typed the digit then Enter.
	if len(sent) < 2 || sent[0] != "3" || sent[len(sent)-1] != "Enter" {
		t.Fatalf("keystrokes = %#v, want [3 ... Enter]", sent)
	}
	// Reply archived to done, not left in pending.
	assertExists(t, filepath.Join(root, "inbox", "done", job.JobID+".json"))
	assertNotExists(t, filepath.Join(root, "inbox", "pending", job.JobID+".json"))
}
