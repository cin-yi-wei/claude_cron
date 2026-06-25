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
	ScreenLogin   ScreenState = "login"   // auth expired: "Please run /login" / 401
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

	// Login needed: genuinely logged out / token rejected. These phrases are
	// conclusive on their own.
	for _, sig := range []string{"invalid authentication credentials", "not logged in", "/login to authenticate"} {
		if strings.Contains(low, sig) {
			return ScreenLogin
		}
	}
	// "Please run /login" is trickier: Claude ALSO prefixes it onto transient
	// network errors, e.g. "● Please run /login · API Error: 401 The socket
	// connection was closed unexpectedly". Auth is fine there — only the socket
	// blipped — and the line replays on --resume. Classifying that as a login
	// screen makes the auth watchdog kill a healthy, authenticated session every
	// cycle (a tight restart loop). So treat "please run /login" as login ONLY
	// when it is NOT part of an inline transient API/network error line.
	if strings.Contains(low, "please run /login") {
		transientAPIError := strings.Contains(low, "api error") && (strings.Contains(low, "socket") ||
			strings.Contains(low, "connection") || strings.Contains(low, "closed unexpectedly") ||
			strings.Contains(low, "timeout") || strings.Contains(low, "timed out") ||
			strings.Contains(low, "fetch(") || strings.Contains(low, "econnreset") ||
			strings.Contains(low, "network") || strings.Contains(low, "etimedout"))
		if !transientAPIError {
			return ScreenLogin
		}
	}

	// Glitch: the model printed raw tool-call markup instead of executing it.
	// These literals never appear in a normal rendered TUI (tools render as
	// "● Tool(...)"), so any one is conclusive.
	for _, sig := range []string{"<invoke name=", "<parameter name=", "</invoke>", "<function_calls>", "antml:invoke"} {
		if strings.Contains(s, sig) {
			return ScreenGlitch
		}
	}

	// Confirm dialog: Claude's own permission/confirm/trust prompt (proceed?,
	// make this edit?, trust folder?, create SKILL.md?, edit settings?, …). These
	// are structurally a question plus numbered options with the "❯" selection
	// cursor; parseConfirmDialog requires that cursor so prose/markdown numbered
	// lists can't trigger it. Covers every native dialog, not a fixed phrase list.
	if _, ok := parseConfirmDialog(s); ok {
		return ScreenConfirm
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
