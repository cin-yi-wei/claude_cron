package channelagent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type StdoutSender struct {
	Writer io.Writer
}

func (s StdoutSender) Send(_ context.Context, output OutputJob) error {
	if s.Writer == nil {
		return nil
	}
	_, err := fmt.Fprintln(s.Writer, output.Text)
	return err
}

type TmuxInjector struct {
	Session   string
	Root      string
	AutoStart bool
}

func (i TmuxInjector) Inject(ctx context.Context, job InputJob, outputPath string) error {
	if strings.TrimSpace(i.Session) == "" {
		return fmt.Errorf("tmux session is required")
	}
	if err := i.ensureSession(ctx); err != nil {
		return err
	}
	// Collapse the prompt to a single line. Embedded newlines sent to the Claude
	// TUI via send-keys are interpreted as Enter keypresses, which submit the
	// input prematurely and fragment the prompt. A single line submits cleanly.
	prompt := collapseWhitespace(BuildClaudePrompt(i.Root, job, outputPath))

	// Send the prompt literally (-l) so it is inserted as text rather than
	// interpreted as key names. Then submit with a SEPARATE Enter after a delay
	// long enough for the TUI to finish inserting a long prompt before it is
	// submitted. This two-step recipe (literal text, pause, Enter) is the only
	// one observed to reliably land and submit a multi-hundred-character prompt
	// in the Claude TUI; sending the Enter in the same call, or with too short a
	// delay, drops the input.
	if err := runExternalCommand(ctx, "tmux", "send-keys", "-t", i.Session, "-l", prompt); err != nil {
		return err
	}
	time.Sleep(injectSubmitDelay)
	return runExternalCommand(ctx, "tmux", "send-keys", "-t", i.Session, "Enter")
}

// injectSubmitDelay is the pause between inserting the prompt text and pressing
// Enter, giving the TUI time to finish accepting a long literal paste.
var injectSubmitDelay = 800 * time.Millisecond

// collapseWhitespace replaces all runs of whitespace (including newlines) with a
// single space and trims the result, producing a single-line string.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var runExternalCommand = func(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func (i TmuxInjector) ensureSession(ctx context.Context) error {
	err := runExternalCommand(ctx, "tmux", "has-session", "-t", i.Session)
	if err == nil {
		return nil
	}
	if !i.AutoStart {
		return err
	}
	return runExternalCommand(ctx, "tmux", "new-session", "-d", "-s", i.Session, "claude")
}

func BuildClaudePrompt(root string, job InputJob, outputPath string) string {
	return fmt.Sprintf(`請讀取 %s/current_job.json。

根據目前 Claude Code session / project context，分析裡面的新對話內容。

請將要回覆使用者的 JSON 寫入：
%s.tmp

完成後 rename 成：
%s

JSON 必須包含：
schema=%d
job_id=%q
request_id=%q
input_hash=%q
send
text

不要發送訊息。
不要修改 %s/state。
不要移動 inbox/outbox job。
不要做 hash 判斷。`, root, outputPath, outputPath, 1, job.JobID, job.RequestID, job.InputHash, root)
}
