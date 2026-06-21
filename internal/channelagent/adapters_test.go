package channelagent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTmuxInjectorAutoStartsMissingSession(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = oldDelay }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		if name == "tmux" && len(args) >= 1 && args[0] == "has-session" {
			return errors.New("missing")
		}
		return nil
	}

	err := TmuxInjector{Session: "claude-cron", Root: ".channel-agent", AutoStart: true}.Inject(context.Background(), InputJob{JobID: "j1", RequestID: "r1", InputHash: "h1"}, ".channel-agent/outbox/pending/j1.json")
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}

	wantFirst := []string{"tmux", "has-session", "-t", "claude-cron"}
	wantSecond := []string{"tmux", "new-session", "-d", "-s", "claude-cron", "claude"}
	if len(calls) != 5 {
		t.Fatalf("calls = %#v, want 5 calls", calls)
	}
	if !reflect.DeepEqual(calls[0], wantFirst) || !reflect.DeepEqual(calls[1], wantSecond) {
		t.Fatalf("calls = %#v", calls)
	}
	// Third call clears the box (C-c); fourth sends the prompt literally as a
	// single line; fifth submits with Enter.
	wantClear := []string{"tmux", "send-keys", "-t", "claude-cron", "C-c"}
	if !reflect.DeepEqual(calls[2], wantClear) {
		t.Fatalf("third call = %#v, want C-c clear", calls[2])
	}
	if calls[3][1] != "send-keys" || calls[3][len(calls[3])-2] != "-l" {
		t.Fatalf("fourth call = %#v, want literal send-keys", calls[3])
	}
	if strings.Contains(calls[3][len(calls[3])-1], "\n") {
		t.Fatalf("prompt must be single line, got %q", calls[3][len(calls[3])-1])
	}
	wantEnter := []string{"tmux", "send-keys", "-t", "claude-cron", "Enter"}
	if !reflect.DeepEqual(calls[4], wantEnter) {
		t.Fatalf("fifth call = %#v, want Enter submit", calls[4])
	}
}

func TestBuildClaudePromptTeachesNotify(t *testing.T) {
	job := InputJob{
		Schema:    1,
		JobID:     "j1",
		RequestID: "r1",
		InputHash: "h1",
		Source:    SourceMessage{Platform: "discord", ChannelID: "chan99", Content: "hi"},
	}
	p := BuildClaudePrompt(".channel-agent", job, ".channel-agent/outbox/pending/j1.json", nil)
	if !strings.Contains(p, "claude-cron notify") {
		t.Fatalf("prompt should teach notify:\n%s", p)
	}
	if !strings.Contains(p, "chan99") {
		t.Fatalf("prompt should include the job's channel id:\n%s", p)
	}
	if strings.Contains(p, "附帶圖片") {
		t.Fatalf("no images → prompt must not mention attachments:\n%s", p)
	}
	withImg := BuildClaudePrompt(".channel-agent", job, ".channel-agent/outbox/pending/j1.json", []string{"/tmp/a.png"})
	if !strings.Contains(withImg, "/tmp/a.png") || !strings.Contains(withImg, "附帶圖片") {
		t.Fatalf("with images → prompt must point at the local path:\n%s", withImg)
	}
}

func TestImageAttachmentDetection(t *testing.T) {
	cases := []struct {
		a      Attachment
		isImg  bool
		ext    string
	}{
		{Attachment{URL: "https://cdn/x.png", Type: "image/png"}, true, ".png"},
		{Attachment{URL: "https://cdn/x.jpg?ex=1&is=2"}, true, ".jpg"},
		{Attachment{URL: "https://cdn/x", Type: "image/webp"}, true, ".webp"},
		{Attachment{URL: "https://cdn/doc.pdf", Type: "application/pdf"}, false, ""},
		{Attachment{URL: "https://cdn/clip.mp4", Type: "video/mp4"}, false, ""},
	}
	for _, c := range cases {
		if got := isImageAttachment(c.a); got != c.isImg {
			t.Fatalf("isImageAttachment(%v) = %v, want %v", c.a, got, c.isImg)
		}
		if c.isImg {
			if got := imageExt(c.a); got != c.ext {
				t.Fatalf("imageExt(%v) = %q, want %q", c.a, got, c.ext)
			}
		}
	}
}
