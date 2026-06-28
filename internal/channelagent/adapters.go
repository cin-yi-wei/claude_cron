package channelagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// errSessionBusy is returned by Inject when the Claude TUI is mid-turn (a spinner
// is running) or sitting on its own confirm/permission dialog. Typing then would
// send C-c into the pane — interrupting the in-flight turn (observed as
// "assistant error: exit status 1") or dismissing the dialog — and the prompt
// would land in a busy box and fail to submit ("still in input box"). The caller
// treats this as "not now, retry next cycle" WITHOUT counting a failed attempt,
// so a long legitimate turn doesn't burn the waiting job's retry budget. This is
// also what stops the duplicate-prompt storm: serve no longer re-injects (and
// re-interrupts) every cycle while a turn is running — it waits for idle.
var errSessionBusy = errors.New("inject deferred: session busy (mid-turn or dialog)")

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
	// Don't inject into a busy pane. typeAndSubmit leads with C-c, so injecting
	// mid-turn interrupts the running turn and injecting over a confirm dialog
	// dismisses it; either way the prompt is also likely to fail to submit. Defer
	// instead — the caller requeues without burning a retry attempt.
	if i.paneBusy(ctx) {
		return errSessionBusy
	}
	// Download any image attachments to local files so Claude can actually read
	// them (vision); the prompt then points at the local paths. Best-effort.
	images := downloadImageAttachments(i.Root, job)
	// Some platforms (e.g. Slack) turn an over-long text message into a file
	// attachment (.txt snippet). Download those too so the prompt can point
	// Claude at them — otherwise the actual message body is lost. Best-effort.
	texts := downloadTextAttachments(i.Root, job)
	// Collapse the prompt to a single line. Embedded newlines sent to the Claude
	// TUI via send-keys are interpreted as Enter keypresses, which submit the
	// input prematurely and fragment the prompt. A single line submits cleanly.
	prompt := collapseWhitespace(BuildClaudePrompt(i.Root, job, outputPath, images, texts))

	// The Enter occasionally fails to submit (drops), leaving the prompt sitting
	// in the input box and never processed. So submit, then VERIFY the input box
	// is empty; if not, re-run the full recipe. Retry a few times.
	var lastErr error
	for attempt := 0; attempt < injectMaxAttempts; attempt++ {
		// defer-don't-burn: if the turn went busy between attempts (a prior
		// attempt's C-c/paste kicked off work, or a dialog popped), requeue
		// instead of spending a retry on a submit that can't land.
		if i.paneBusy(ctx) {
			return errSessionBusy
		}
		if err := i.typeAndSubmit(ctx, prompt); err != nil {
			// typeAndSubmit defers (no Enter sent) when the pane turned busy
			// during the paste-settle window — propagate as a defer, don't burn
			// the remaining attempts on it.
			if errors.Is(err, errSessionBusy) {
				return err
			}
			lastErr = err
			continue
		}
		time.Sleep(injectVerifyDelay)
		pane, err := runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", i.Session)
		if err != nil || !inputBoxHasText(pane) {
			return nil // can't verify, or input box is empty → submitted
		}
		lastErr = fmt.Errorf("inject: prompt still in input box after attempt %d", attempt+1)
	}
	return lastErr
}

// LooksGlitched reports whether the session printed raw tool-call markup as text
// (e.g. "<invoke name=...>" / "<parameter name=...>") instead of executing it — a
// transient model glitch that ends the turn with no reply. Normal tool use
// renders as "● Read(...)", never the raw XML, so this signature is safe.
func (i TmuxInjector) LooksGlitched(ctx context.Context) bool {
	pane, err := runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", i.Session)
	if err != nil {
		return false
	}
	return classifyScreen(pane) == ScreenGlitch
}

// typeAndSubmit runs the one recipe observed to reliably submit in the Claude
// TUI: Ctrl-C to clear the box, the prompt as a literal paste, a pause for the
// long paste to settle, then a SEPARATE Enter.
func (i TmuxInjector) typeAndSubmit(ctx context.Context, prompt string) error {
	if err := runExternalCommand(ctx, "tmux", "send-keys", "-t", i.Session, "C-c"); err != nil {
		return err
	}
	if err := runExternalCommand(ctx, "tmux", "send-keys", "-t", i.Session, "-l", prompt); err != nil {
		return err
	}
	time.Sleep(injectSubmitDelay)
	// Re-check right before Enter. The pane can turn busy (a turn started, or a
	// confirm dialog popped) DURING the paste-settle delay — a TOCTOU window the
	// pre-loop busy check can't cover. Sending Enter then either fails to submit
	// or confirms the dialog. Defer instead; the leading C-c on the next attempt
	// clears the text we just pasted.
	if i.paneBusy(ctx) {
		return errSessionBusy
	}
	return runExternalCommand(ctx, "tmux", "send-keys", "-t", i.Session, "Enter")
}

// paneBusy reports whether the pane is mid-turn (Working) or showing a confirm
// dialog — states where injecting or submitting would interrupt the turn,
// dismiss the dialog, or simply fail to land. Capture failure returns false (we
// can't tell, so don't block). Used both before injecting and right before the
// Enter, so a state change at any point defers rather than burns a retry.
func (i TmuxInjector) paneBusy(ctx context.Context) bool {
	pane, err := runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", i.Session)
	if err != nil {
		return false
	}
	switch classifyScreen(pane) {
	case ScreenWorking, ScreenConfirm:
		return true
	}
	return false
}

// inputBoxHasText reports whether the Claude TUI's input box (the bottom-most
// "❯" line) still holds unsent text. The echoed sent message also starts with
// "❯" but appears above; only the LAST such line is the live input box.
func inputBoxHasText(pane string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimLeft(lines[i], " \t")
		if strings.HasPrefix(l, "❯") {
			return strings.TrimSpace(strings.TrimPrefix(l, "❯")) != ""
		}
	}
	return false // no prompt line seen → don't loop
}

// injectSubmitDelay is the pause between inserting the prompt text and pressing
// Enter; injectVerifyDelay is the settle before checking the box; injectMaxAttempts
// bounds the resubmit retries.
var (
	injectSubmitDelay = 1200 * time.Millisecond
	injectVerifyDelay = 1200 * time.Millisecond
	injectMaxAttempts = 3
)

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

// runExternalCommandOutput is like runExternalCommand but returns stdout. Used
// to read `tmux list-sessions`. Injectable for tests.
var runExternalCommandOutput = func(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	out, err := cmd.Output()
	return string(out), err
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

func BuildClaudePrompt(root string, job InputJob, outputPath string, imagePaths, textPaths []string) string {
	// Use absolute paths so the prompt resolves correctly regardless of the
	// agent's current working directory (the agent may `cd` while renaming the
	// output file, which would otherwise break the next job's relative paths).
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if abs, err := filepath.Abs(outputPath); err == nil {
		outputPath = abs
	}
	images := ""
	if len(imagePaths) > 0 {
		images = "\n\n本則訊息附帶圖片，已下載到本地檔，請用 Read 工具讀取這些路徑並一併分析（Read 支援圖片）：\n" + strings.Join(imagePaths, "\n")
	}
	texts := ""
	if len(textPaths) > 0 {
		texts = "\n\n本則訊息附帶文字檔（平台把過長訊息轉存成檔案，例如 Slack 的 snippet），實際訊息正文在這些檔案裡，請用 Read 工具讀取其內容，當作使用者訊息的一部分一併處理：\n" + strings.Join(textPaths, "\n")
	}
	return fmt.Sprintf(`請讀取 %s/current_job.json。%s%s

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
不要做 hash 判斷。

若你啟動了長時間的背景任務（detached，例如安裝、編譯、長測試），請在 shell 指令鏈的最後加上：
&& claude-cron notify %s "完成訊息" --root %s
這樣任務跑完會自動通知使用者；不要為了等它而卡住這次回覆。`, root, images, texts, outputPath, outputPath, 1, job.JobID, job.RequestID, job.InputHash, root, job.Source.ChannelID, root)
}

// downloadImageAttachments saves a job's image attachments under
// <root>/attachments/<jobid>/ and returns their absolute local paths so the
// injected prompt can point Claude at real files (Read supports images). Best
// effort: a non-image attachment or a failed download is skipped.
func downloadImageAttachments(root string, job InputJob) []string {
	var out []string
	dir := pathIn(root, "attachments", sanitize(job.JobID))
	for idx, a := range job.Source.Attachments {
		if a.URL == "" || !isImageAttachment(a) {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return out
		}
		dst := filepath.Join(dir, fmt.Sprintf("%d%s", idx, imageExt(a)))
		if err := httpDownloadFile(a.URL, dst); err != nil {
			continue
		}
		if abs, err := filepath.Abs(dst); err == nil {
			dst = abs
		}
		out = append(out, dst)
	}
	return out
}

// downloadTextAttachments saves a job's text/document attachments under
// <root>/attachments/<jobid>/ and returns their absolute local paths. Platforms
// like Slack convert an over-long message into a .txt snippet file; without this
// the message body would be lost. Best effort: non-text or failed downloads are
// skipped. Images are handled separately by downloadImageAttachments.
func downloadTextAttachments(root string, job InputJob) []string {
	var out []string
	dir := pathIn(root, "attachments", sanitize(job.JobID))
	for idx, a := range job.Source.Attachments {
		if a.URL == "" || isImageAttachment(a) || !isTextAttachment(a) {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return out
		}
		dst := filepath.Join(dir, fmt.Sprintf("%d%s", idx, textExt(a)))
		if err := httpDownloadFile(a.URL, dst); err != nil {
			continue
		}
		if abs, err := filepath.Abs(dst); err == nil {
			dst = abs
		}
		out = append(out, dst)
	}
	return out
}

func isTextAttachment(a Attachment) bool {
	t := strings.ToLower(a.Type)
	if strings.HasPrefix(t, "text/") {
		return true
	}
	switch t {
	case "application/json", "application/xml", "application/x-yaml", "application/yaml",
		"application/x-log", "application/csv", "application/x-ndjson":
		return true
	}
	switch strings.ToLower(filepath.Ext(strings.SplitN(a.URL, "?", 2)[0])) {
	case ".txt", ".text", ".log", ".md", ".markdown", ".csv", ".tsv",
		".json", ".jsonl", ".ndjson", ".xml", ".yaml", ".yml", ".ini", ".conf":
		return true
	}
	return false
}

func textExt(a Attachment) string {
	if e := filepath.Ext(strings.SplitN(a.URL, "?", 2)[0]); len(e) >= 2 && len(e) <= 10 {
		return e
	}
	if strings.Contains(strings.ToLower(a.Type), "json") {
		return ".json"
	}
	return ".txt"
}

func isImageAttachment(a Attachment) bool {
	if strings.HasPrefix(strings.ToLower(a.Type), "image/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(strings.SplitN(a.URL, "?", 2)[0])) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

func imageExt(a Attachment) string {
	if e := filepath.Ext(strings.SplitN(a.URL, "?", 2)[0]); len(e) >= 2 && len(e) <= 5 {
		return e
	}
	switch strings.ToLower(a.Type) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".img"
}

// httpDownloadFile GETs url into dst (capped at 25MB, 30s timeout).
func httpDownloadFile(url, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(resp.Body, 25<<20))
	return err
}
