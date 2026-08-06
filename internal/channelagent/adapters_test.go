package channelagent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTmuxInjectorAutoStartsMissingSession(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = oldDelay }()
	// Skip the cold-start readiness probe so this test asserts only the inject
	// recipe (has-session, new-session, C-c, paste, Enter).
	oldBoot := sessionBootDelay
	sessionBootDelay = 0
	defer func() { sessionBootDelay = oldBoot }()

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

func TestInjectDefersWhenSessionBusy(t *testing.T) {
	oldRun, oldOut := runExternalCommand, runExternalCommandOutput
	defer func() { runExternalCommand, runExternalCommandOutput = oldRun, oldOut }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = oldDelay }()

	var sentKeys bool
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		if name == "tmux" && len(args) >= 1 && (args[0] == "send-keys") {
			sentKeys = true
		}
		return nil // has-session ok (session exists)
	}
	// Pane shows an in-flight turn → classifyScreen == ScreenWorking.
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "● Doing work...\n  (esc to interrupt)\n", nil
	}

	err := TmuxInjector{Session: "cc-x", Root: ".channel-agent"}.Inject(context.Background(), InputJob{JobID: "j1", RequestID: "r1", InputHash: "h1"}, ".channel-agent/outbox/pending/j1.json")
	if !errors.Is(err, errSessionBusy) {
		t.Fatalf("want errSessionBusy, got %v", err)
	}
	if sentKeys {
		t.Fatal("Inject must NOT send any keys into a busy pane")
	}
}

func TestInjectProceedsWhenIdle(t *testing.T) {
	oldRun, oldOut := runExternalCommand, runExternalCommandOutput
	defer func() { runExternalCommand, runExternalCommandOutput = oldRun, oldOut }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = oldDelay }()

	var enterSent bool
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		if name == "tmux" && len(args) >= 1 && args[0] == "send-keys" && args[len(args)-1] == "Enter" {
			enterSent = true
		}
		return nil
	}
	// Idle pane (empty input box) on capture; the post-submit verify also reads it
	// as empty so Inject reports success.
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "● done\n\n❯ \n", nil
	}

	err := TmuxInjector{Session: "cc-x", Root: ".channel-agent"}.Inject(context.Background(), InputJob{JobID: "j1", RequestID: "r1", InputHash: "h1"}, ".channel-agent/outbox/pending/j1.json")
	if err != nil {
		t.Fatalf("idle inject should succeed, got %v", err)
	}
	if !enterSent {
		t.Fatal("idle inject should submit with Enter")
	}
}

// review Minor 3: SelectTrustSettings's settle sleep must be ctx-aware so
// SandboxDriver.Stop/StopAll (which now call it from a2a_driver.go's loop and
// block on the goroutine actually exiting) aren't delayed by the full
// injectSubmitDelay once the caller has already given up. Verified safe for
// the other caller, supervisor.go's handleLoginScreen, via code inspection:
// it never re-checks the pane within the same tick after this call returns.
func TestSelectTrustSettingsIsCtxAware(t *testing.T) {
	oldRun := runExternalCommand
	defer func() { runExternalCommand = oldRun }()

	var calls []string
	runExternalCommand = func(_ context.Context, _ string, args ...string) error {
		calls = append(calls, args[len(args)-1])
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模擬 Stop/StopAll 正在等這個呼叫回來時取消 ctx

	start := time.Now()
	err := TmuxInjector{Session: "aa-x"}.SelectTrustSettings(ctx)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("SelectTrustSettings took %v after ctx cancellation, want a near-instant return (injectSubmitDelay is %v)", elapsed, injectSubmitDelay)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(calls) != 1 || calls[0] != "1" {
		t.Fatalf("calls = %#v, want only the initial \"1\" keystroke sent before the cancelled sleep", calls)
	}
}

// The ordinary (non-cancelled) path must be unaffected: both keystrokes still
// go out in order, and the settle delay still elapses (this is what the OTHER
// caller, supervisor.go's cc- login watchdog, still relies on).
func TestSelectTrustSettingsStillSendsBothKeystrokesWhenNotCancelled(t *testing.T) {
	oldRun := runExternalCommand
	defer func() { runExternalCommand = oldRun }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 5 * time.Millisecond
	defer func() { injectSubmitDelay = oldDelay }()

	var calls []string
	runExternalCommand = func(_ context.Context, _ string, args ...string) error {
		calls = append(calls, args[len(args)-1])
		return nil
	}

	if err := (TmuxInjector{Session: "aa-x"}).SelectTrustSettings(context.Background()); err != nil {
		t.Fatalf("SelectTrustSettings: %v", err)
	}
	if len(calls) != 2 || calls[0] != "1" || calls[1] != "Enter" {
		t.Fatalf("calls = %#v, want [1 Enter]", calls)
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
	p := BuildClaudePrompt(".channel-agent", job, ".channel-agent/outbox/pending/j1.json", nil, nil)
	if !strings.Contains(p, "claude-cron notify") {
		t.Fatalf("prompt should teach notify:\n%s", p)
	}
	if !strings.Contains(p, "chan99") {
		t.Fatalf("prompt should include the job's channel id:\n%s", p)
	}
	if strings.Contains(p, "附帶圖片") || strings.Contains(p, "附帶文字檔") {
		t.Fatalf("no attachments → prompt must not mention them:\n%s", p)
	}
	withImg := BuildClaudePrompt(".channel-agent", job, ".channel-agent/outbox/pending/j1.json", []string{"/tmp/a.png"}, nil)
	if !strings.Contains(withImg, "/tmp/a.png") || !strings.Contains(withImg, "附帶圖片") {
		t.Fatalf("with images → prompt must point at the local path:\n%s", withImg)
	}
	withTxt := BuildClaudePrompt(".channel-agent", job, ".channel-agent/outbox/pending/j1.json", nil, []string{"/tmp/m.txt"})
	if !strings.Contains(withTxt, "/tmp/m.txt") || !strings.Contains(withTxt, "附帶文字檔") {
		t.Fatalf("with text → prompt must point at the local path:\n%s", withTxt)
	}
}

// Important 2（final review 2026-08-06）：a2a_server.go 一律回傳一個 branch
// 給呼叫方，但沙盒收到的提示詞（跟 cc- 完全一樣）從沒教它要 commit —— sweep
// 十分鐘後就把 worktree 刪了，呼叫方拿到的是一個內容已經不存在的分支名。
// 修法是在提示詞裡對 A2A 的工作（job.Source.Platform == "a2a"）額外教一句
// 「回覆前先 commit」；cc- 的 job（Platform 是 discord/slack/...，從來不會是
// "a2a" —— 這個值只由 a2a_executor.go 在伺服器端設定，呼叫方無法偽造）必須
// 完全不受影響，字面一個字都不能多。
func TestBuildClaudePromptTeachesA2ASandboxToCommitBeforeReporting(t *testing.T) {
	ccJob := InputJob{
		Schema: 1, JobID: "j1", RequestID: "r1", InputHash: "h1",
		Source: SourceMessage{Platform: "discord", ChannelID: "chan99", Content: "hi"},
	}
	ccPrompt := BuildClaudePrompt(".channel-agent", ccJob, ".channel-agent/outbox/pending/j1.json", nil, nil)
	if strings.Contains(ccPrompt, "commit") {
		t.Fatalf("a cc- job's prompt must not mention committing at all:\n%s", ccPrompt)
	}

	a2aJob := InputJob{
		Schema: 1, JobID: "j2", RequestID: "r2", InputHash: "h2",
		Source: SourceMessage{Platform: "a2a", ChannelID: "c1", Content: "please refactor foo.go"},
	}
	a2aPrompt := BuildClaudePrompt(".channel-agent", a2aJob, ".channel-agent/outbox/pending/j2.json", nil, nil)
	if !strings.Contains(a2aPrompt, "git commit") && !strings.Contains(a2aPrompt, "commit") {
		t.Fatalf("an a2a job's prompt must instruct the sandbox to commit before reporting done:\n%s", a2aPrompt)
	}

	// The rest of the prompt (notify teaching etc.) must still be present —
	// this is an ADDITION, not a replacement of the shared template.
	if !strings.Contains(a2aPrompt, "claude-cron notify") {
		t.Fatalf("a2a prompt lost the shared notify teaching:\n%s", a2aPrompt)
	}
}

func TestTextAttachmentDetection(t *testing.T) {
	cases := []struct {
		a      Attachment
		isText bool
		ext    string
	}{
		{Attachment{URL: "https://files.slack.com/snippet.txt", Type: "text/plain"}, true, ".txt"},
		{Attachment{URL: "https://cdn/log.log?t=1"}, true, ".log"},
		{Attachment{URL: "https://cdn/x", Type: "application/json"}, true, ".json"},
		{Attachment{URL: "https://cdn/x.png", Type: "image/png"}, false, ""},
		{Attachment{URL: "https://cdn/doc.pdf", Type: "application/pdf"}, false, ""},
	}
	for _, c := range cases {
		got := isTextAttachment(c.a) && !isImageAttachment(c.a)
		if got != c.isText {
			t.Fatalf("text detect(%v) = %v, want %v", c.a, got, c.isText)
		}
		if c.isText {
			if e := textExt(c.a); e != c.ext {
				t.Fatalf("textExt(%v) = %q, want %q", c.a, e, c.ext)
			}
		}
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
