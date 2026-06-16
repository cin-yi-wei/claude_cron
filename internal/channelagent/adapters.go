package channelagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

	// Clear any leftover text in the input box before typing. A single Ctrl-C in
	// the Claude TUI clears the current input line (two in quick succession would
	// quit, but one is safe). Without this, a partially-typed prompt left from a
	// prior injection is concatenated with this one and the combined garbage
	// fails to produce a valid response. (C-u and Escape do NOT clear the box.)
	if err := runExternalCommand(ctx, "tmux", "send-keys", "-t", i.Session, "C-c"); err != nil {
		return err
	}
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
	cmd := exec.CommandContext(ctx, name, args...)
	// Strip TMUX/TMUX_PANE from the environment. When `serve` itself runs inside
	// a tmux session, those vars make `tmux send-keys -t <other-session>` target
	// the wrong (nested) context and silently fail to deliver keystrokes to the
	// agent session. Clearing them makes the tmux client act as a plain client.
	cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	return cmd.Run()
}

func envWithout(env []string, drop ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		keep := true
		for _, d := range drop {
			if strings.HasPrefix(kv, d+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
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
	// Use absolute paths so the prompt resolves correctly regardless of the
	// agent's current working directory (the agent may `cd` while renaming the
	// output file, which would otherwise break the next job's relative paths).
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if abs, err := filepath.Abs(outputPath); err == nil {
		outputPath = abs
	}
	return fmt.Sprintf(`請讀取 %s/current_job.json。

根據目前 Claude Code session / project context，分析裡面的新對話內容，產生要回覆使用者的訊息。

請將回覆 JSON 寫入：
%s.tmp
完成後 rename 成：
%s

JSON 必須是這個格式（注意 send 要設成布林 true 才會把回覆送出）：
{
  "schema": %d,
  "job_id": %q,
  "request_id": %q,
  "input_hash": %q,
  "send": true,
  "text": "你要回覆給使用者的內容"
}

把 "text" 換成你實際要回覆的內容。要回覆就把 "send" 設為 true；只有在判斷不該回覆（例如重複、無意義）時才設為 false。

不要自己呼叫任何發送訊息的指令。
不要修改 %s/state。
不要移動 inbox/outbox job。
不要做 hash 判斷。`, root, outputPath, outputPath, 1, job.JobID, job.RequestID, job.InputHash, root)
}
