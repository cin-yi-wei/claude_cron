package channelagent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// confirmDialog is a Claude native TUI confirm prompt: a question plus an ordered
// list of numbered options (e.g. "Do you want to create SKILL.md? / 1. Yes /
// 2. Yes, and allow… / 3. No"). These are NOT PreToolUse-gated, so the channel
// permission gate never sees them — without help the session blocks forever on a
// keypress nobody in the channel can make. The confirm watchdog surfaces the
// dialog to the binding's channel and types the user's choice back into the pane.
type confirmDialog struct {
	Question string
	Options  []string
}

// hash identifies a dialog so the watchdog posts it once, not every cycle.
func (d confirmDialog) hash() string {
	h := sha1.Sum([]byte(d.Question + "\x00" + strings.Join(d.Options, "\x00")))
	return hex.EncodeToString(h[:8])
}

// parseConfirmDialog extracts a confirm dialog from a pane snapshot. It requires
// sequentially-numbered options (1., 2., …) with the TUI selection cursor "❯" on
// one of them — that cursor is what distinguishes a live confirm prompt from
// ordinary chat/markdown that happens to contain a numbered list.
func parseConfirmDialog(pane string) (confirmDialog, bool) {
	lines := strings.Split(stripANSI(pane), "\n")
	var opts []string
	firstOptIdx, sawCursor := -1, false
	for i, ln := range lines {
		raw := strings.TrimSpace(ln)
		cursor := strings.HasPrefix(raw, "❯")
		t := strings.TrimSpace(strings.TrimPrefix(raw, "❯"))
		if len(t) >= 3 && t[0] >= '1' && t[0] <= '9' && t[1] == '.' && int(t[0]-'0') == len(opts)+1 {
			opts = append(opts, strings.TrimSpace(t[2:]))
			if firstOptIdx < 0 {
				firstOptIdx = i
			}
			if cursor {
				sawCursor = true
			}
		}
	}
	if len(opts) < 2 || !sawCursor {
		return confirmDialog{}, false
	}
	question := ""
	for i := firstOptIdx - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			question = t
			break
		}
	}
	return confirmDialog{Question: question, Options: opts}, true
}

// confirmChoice maps a user reply to a 1-based option index for an n-option
// dialog. Accepts an explicit digit, or yes→first option / no→last option.
// Returns 0 when the reply is not a recognizable choice (left for normal handling).
func confirmChoice(reply string, n int) int {
	r := strings.ToLower(strings.TrimSpace(reply))
	if d, err := strconv.Atoi(r); err == nil && d >= 1 && d <= n {
		return d
	}
	switch r {
	case "y", "yes", "ya", "好", "可以", "允許", "准", "ok":
		return 1
	case "n", "no", "不", "否", "拒絕", "取消":
		return n
	}
	return 0
}

// confirmPromptMessage renders the dialog for the channel.
func confirmPromptMessage(d confirmDialog) string {
	var b strings.Builder
	b.WriteString("🔧 session 需要你決定（這是 Claude 內建的確認框，不是工具權限）：\n")
	if d.Question != "" {
		b.WriteString(d.Question + "\n")
	}
	for i, o := range d.Options {
		fmt.Fprintf(&b, "%d. %s\n", i+1, o)
	}
	b.WriteString("回覆選項編號（例如 1）；也可回 y=第一項 / n=最後一項。")
	return b.String()
}

// sendConfirmChoice types the chosen option number into the session and submits.
func sendConfirmChoice(ctx context.Context, session string, choice int) error {
	if err := runExternalCommand(ctx, "tmux", "send-keys", "-t", session, strconv.Itoa(choice)); err != nil {
		return err
	}
	return runExternalCommand(ctx, "tmux", "send-keys", "-t", session, "Enter")
}

// resolveConfirmReply checks the binding's inbox for a queued reply that answers
// the on-screen confirm dialog; if found it types the choice into the pane and
// archives the reply. Runs OUTSIDE claude.lock (the session is blocked on the
// prompt while the worker holds the lock in waitOutput), so the reply must be
// applied here or it never reaches the dialog. Returns true if it acted.
func resolveConfirmReply(ctx context.Context, root, session string, d confirmDialog) bool {
	pendingPath, err := oldestJSON(pathIn(root, "inbox", "pending"))
	if err != nil || pendingPath == "" {
		return false
	}
	var job InputJob
	if err := ReadJSON(pendingPath, &job); err != nil {
		return false
	}
	choice := confirmChoice(job.Source.Content, len(d.Options))
	if choice == 0 {
		return false // not a choice → leave for normal injection once unblocked
	}
	if err := sendConfirmChoice(ctx, session, choice); err != nil {
		return false
	}
	_ = moveFile(pendingPath, pathIn(root, "inbox", "done", filepath.Base(pendingPath)))
	return true
}
