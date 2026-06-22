package channelagent

import (
	"regexp"
	"strings"
)

// ScreenState is a structural classification of a Claude TUI pane snapshot,
// inspired by pikiloom's classifyClaudeScreen: instead of ad-hoc grepping we
// reduce the (ANSI-stripped) screen to one of a few well-defined states. Used to
// tell "idle / working / a broken turn / a confirm dialog" apart reliably.
type ScreenState string

const (
	ScreenUnknown ScreenState = "unknown"
	ScreenIdle    ScreenState = "idle"    // sitting at an empty ❯ prompt; turn ended
	ScreenWorking ScreenState = "working" // generating / running a tool (spinner)
	ScreenConfirm ScreenState = "confirm" // Claude's own permission/confirm dialog
	ScreenGlitch  ScreenState = "glitch"  // printed literal tool-call markup as text
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// stripANSI removes ANSI escape sequences so screen matching works on plain text.
func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// classifyScreen reduces a tmux capture-pane snapshot to a ScreenState. Detectors
// require MULTIPLE distinctive fragments where a single keyword would be
// ambiguous (so ordinary chat text mentioning "bypass" or a code block can't
// trigger a false positive) — same defensiveness as pikiloom.
func classifyScreen(pane string) ScreenState {
	s := stripANSI(pane)
	low := strings.ToLower(s)

	// Glitch: the model printed raw tool-call markup instead of executing it.
	// These literals never appear in a normal rendered TUI (tools render as
	// "● Tool(...)"), so any one is conclusive.
	for _, sig := range []string{"<invoke name=", "<parameter name=", "</invoke>", "<function_calls>", "antml:invoke"} {
		if strings.Contains(s, sig) {
			return ScreenGlitch
		}
	}

	// Confirm dialog: Claude's own permission/confirm/trust prompt. Require the
	// question text AND a numbered "❯" option so prose can't trigger it.
	hasOption := strings.Contains(s, "❯ 1.") || strings.Contains(s, "❯ 2.") || strings.Contains(low, "1. yes") || strings.Contains(low, "2. yes")
	if hasOption {
		for _, q := range []string{"do you want to proceed", "do you want to make this edit", "bypass permissions mode", "yes, i accept", "do you trust", "no, exit"} {
			if strings.Contains(low, q) {
				return ScreenConfirm
			}
		}
	}

	// Working: a spinner / in-flight turn. Recent Claude shows a status line like
	// "(esc to interrupt)" and "(1m 4s · ↓ 3.5k tokens)". Require a spinner cue.
	if strings.Contains(low, "esc to interrupt") || strings.Contains(low, "· ↓") || strings.Contains(low, "↓ ") && strings.Contains(low, "tokens") {
		return ScreenWorking
	}

	// Idle: the last non-empty line is the input prompt with nothing queued.
	if !inputBoxHasText(s) && lastPromptLineSeen(s) {
		return ScreenIdle
	}
	return ScreenUnknown
}

// lastPromptLineSeen reports whether a "❯" input line exists in the snapshot.
func lastPromptLineSeen(s string) bool {
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(ln, " \t"), "❯") {
			return true
		}
	}
	return false
}
