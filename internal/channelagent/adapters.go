package channelagent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
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
	Session string
	Root    string
}

func (i TmuxInjector) Inject(ctx context.Context, job InputJob, outputPath string) error {
	if strings.TrimSpace(i.Session) == "" {
		return fmt.Errorf("tmux session is required")
	}
	prompt := BuildClaudePrompt(i.Root, job, outputPath)
	cmd := exec.CommandContext(ctx, "tmux", "send-keys", "-t", i.Session, prompt, "Enter")
	return cmd.Run()
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
