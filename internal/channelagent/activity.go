package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Activity streaming: surface what a Claude session is DOING (thinking + tool
// calls) as it works, not just its final reply. Each cc-* session writes a
// transcript JSONL; this tails the new events since last cycle and turns them
// into concise progress lines (💭 thinking, 🔧 edit, ▶ run, 🔍 search …) that the
// supervisor sends to the binding's channel + web chat.

// transcriptPath returns the binding's current Claude transcript file, or "".
func transcriptPath(worktree string) string {
	id := latestTranscript(worktree)
	if id == "" {
		return ""
	}
	abs := worktree
	if a, err := filepath.Abs(worktree); err == nil {
		abs = a
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects", encodeProjectDir(abs), id+".jsonl")
}

type activityState struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

// transcriptEvent is the subset of a transcript JSONL line we parse.
type transcriptEvent struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// CollectActivity reads transcript events written since the last call for this
// binding and returns formatted progress lines (thinking + tool_use; assistant
// text is skipped — the final answer arrives via the outbox). It persists a byte
// offset in <bRoot>/state/activity.json. On first sight of a transcript it seeks
// to the END (no backlog replay), so only NEW activity streams.
func CollectActivity(bRoot, worktree string) []string {
	tp := transcriptPath(worktree)
	if tp == "" {
		return nil
	}
	statePath := pathIn(bRoot, "state", "activity.json")
	var st activityState
	_ = ReadJSON(statePath, &st)

	fi, err := os.Stat(tp)
	if err != nil {
		return nil
	}
	size := fi.Size()
	if st.Path != tp {
		// New/rotated transcript → start at the end, skip history.
		st = activityState{Path: tp, Offset: size}
		_ = AtomicWriteJSON(statePath, st)
		return nil
	}
	if st.Offset > size {
		st.Offset = 0 // truncated
	}
	if st.Offset == size {
		return nil
	}

	f, err := os.Open(tp)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, size-st.Offset)
	n, _ := f.ReadAt(buf, st.Offset)
	buf = buf[:n]
	// Only consume up to the last complete line so a half-written JSON line is
	// re-read next cycle.
	lastNL := strings.LastIndexByte(string(buf), '\n')
	if lastNL < 0 {
		return nil
	}
	region := buf[:lastNL]
	st.Offset += int64(lastNL) + 1

	var lines []string
	for _, raw := range strings.Split(string(region), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var ev transcriptEvent
		if json.Unmarshal([]byte(raw), &ev) != nil || ev.Type != "assistant" {
			continue
		}
		for _, b := range ev.Message.Content {
			switch b.Type {
			case "thinking":
				if s := condense(b.Thinking, 160); s != "" {
					lines = append(lines, "💭 "+s)
				}
			case "tool_use":
				if isPlumbing(b.Input) {
					continue // skip the reply-publishing plumbing (it trails the reply)
				}
				lines = append(lines, formatToolUse(b.Name, b.Input))
			}
		}
	}
	_ = AtomicWriteJSON(statePath, st)
	return lines
}

// isPlumbing reports whether a tool call is the session's own job/reply
// plumbing (reading current_job.json, writing/moving the outbox reply file).
// That activity is internal noise and — being the session's LAST action before
// a turn ends — would stream right after the reply, so it is filtered out.
func isPlumbing(input json.RawMessage) bool {
	s := string(input)
	for _, pat := range []string{"outbox/pending", "outbox/sent", "current_job.json", ".json.tmp", "/inbox/"} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// formatToolUse renders a one-line summary of a tool call.
func formatToolUse(name string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	str := func(k string) string { s, _ := m[k].(string); return s }
	base := func(p string) string {
		if p == "" {
			return ""
		}
		return filepath.Base(p)
	}
	switch name {
	case "Bash":
		return "▶ " + condense(str("command"), 160)
	case "Edit":
		f := base(str("file_path"))
		o, n := condense(str("old_string"), 100), condense(str("new_string"), 100)
		if o == "" && n == "" {
			return "🔧 Edit " + f
		}
		return "🔧 Edit " + f + "\n  − " + o + "\n  + " + n
	case "MultiEdit":
		f := base(str("file_path"))
		cnt := 0
		if arr, ok := m["edits"].([]any); ok {
			cnt = len(arr)
		}
		return fmt.Sprintf("🔧 Edit %s (%d 處)", f, cnt)
	case "Write":
		f := base(str("file_path"))
		c := condense(str("content"), 140)
		if c == "" {
			return "📝 Write " + f
		}
		return "📝 Write " + f + "\n  " + c
	case "Read":
		return "👀 Read " + base(str("file_path"))
	case "Grep":
		return "🔍 Grep " + condense(str("pattern"), 60)
	case "Glob":
		return "🔍 Glob " + condense(str("pattern"), 60)
	case "Task", "Agent":
		return "🤖 " + condense(str("description"), 80)
	case "WebFetch":
		return "🌐 Fetch " + condense(str("url"), 80)
	case "WebSearch":
		return "🌐 Search " + condense(str("query"), 80)
	default:
		return "🔧 " + name
	}
}

// condense collapses whitespace/newlines and truncates to max runes.
func condense(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// activityMessage joins lines into one throttle-friendly message per tick.
func activityMessage(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("⏳ %s", strings.Join(lines, "\n"))
}

// activitySender builds the Sender that delivers a binding's activity to its
// channel + the web hub (keyed by binding name so the web chat sees it too).
func activitySender(b Binding, cfg Config, tokens bindingTokens) Sender {
	switch b.PlatformOf() {
	case PlatformWeb:
		return WebSender{Hub: DefaultChatHub, Key: b.Name}
	case PlatformTelegram:
		return TeeSender{Inner: TelegramSender{BaseURL: cfg.Telegram.BaseURL, Token: tokens.telegram, ChatID: b.ChannelID}, Hub: DefaultChatHub, Key: b.Name}
	default:
		return TeeSender{Inner: DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: tokens.discord, ChannelID: b.ChannelID}, Hub: DefaultChatHub, Key: b.Name}
	}
}

// RunActivityStreamOnce sweeps every active binding once, sending any new
// transcript activity. Run on a fast independent ticker (NOT inside the
// supervisor cycle, which blocks on the per-binding worker wait — that would
// batch all activity to the end of a turn instead of streaming it live).
func RunActivityStreamOnce(ctx context.Context, root string, cfg Config) {
	reg, err := LoadRegistry(root)
	if err != nil {
		return
	}
	tokens := bindingTokens{discord: os.Getenv(cfg.Discord.TokenEnv), telegram: os.Getenv(cfg.Telegram.TokenEnv)}
	for _, b := range reg.Bindings {
		if b.Paused {
			continue
		}
		lines := CollectActivity(b.Root, b.Worktree)
		if len(lines) == 0 {
			continue
		}
		_ = activitySender(b, cfg, tokens).Send(ctx, OutputJob{Schema: 1, Send: true, Text: activityMessage(lines)})
	}
}
