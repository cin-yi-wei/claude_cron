package channelagent

import (
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
				lines = append(lines, formatToolUse(b.Name, b.Input))
			}
		}
	}
	_ = AtomicWriteJSON(statePath, st)
	return lines
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
	case "Edit", "MultiEdit":
		return "🔧 Edit " + base(str("file_path"))
	case "Write":
		return "📝 Write " + base(str("file_path"))
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

// activityMessage joins lines into one throttle-friendly message per cycle.
func activityMessage(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("⏳ %s", strings.Join(lines, "\n"))
}
