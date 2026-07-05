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
	// The "Select login method" menu (/login step before the OAuth URL) is part of
	// the login flow — classify it as login so the auth watchdog drives it (picks
	// the subscription option). Require the subscription line too so a random
	// numbered list can't match. Checked BEFORE the confirm-dialog detector, which
	// would otherwise capture this numbered menu as a generic confirm prompt.
	if strings.Contains(low, "select login method") && strings.Contains(low, "subscription") {
		return ScreenLogin
	}
	// Two POST-login screens that gate a freshly-authed session before it's usable.
	// Both are part of the login flow → classify as login so handleLoginScreen
	// auto-advances them (Enter / trust). Without this, a pasted code completes auth
	// but the session sits on these forever and never processes messages.
	if paneAwaitingLoginContinue(low) || paneAwaitingManagedSettings(low) {
		return ScreenLogin
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

// loginURLRE matches the OAuth authorize URL Claude prints when the browser
// callback can't be reached (headless/SSH/container) and it falls back to the
// "paste the code" flow. The REAL URL host observed on Claude Code v2.1.201 is
// claude.com/cai/oauth/authorize (verified from a live login pane screenshot);
// older/alt hosts claude.ai and console.anthropic.com are covered too. Anchored
// on the oauth/authorize path so an arbitrary logged URL in scrollback isn't
// mistaken for the login URL.
var loginURLRE = regexp.MustCompile(`https://(?:claude\.(?:com|ai)|console\.anthropic\.com)/[^\s"'<>]*(?:oauth|authorize)[^\s"'<>]*`)

// extractLoginURL returns the OAuth login URL from a pane snapshot, or "" if the
// pane isn't showing one. Used by the re-login flow to relay the URL to the
// channel so a human can complete auth and paste the code back.
//
// The OAuth URL (~277 chars) is far longer than the pane width, and Claude's Ink
// TUI renders it by absolute cursor positioning — each visual row is a SEPARATE
// pane line, NOT a soft-wrap. So `capture-pane -J` (which only rejoins
// soft-wrapped lines) can't stitch it, and a plain regex over the snapshot grabs
// only the first row → a truncated URL (verified in prod: cut at one 80-col row).
// Reconstruct instead: find the row that starts the URL, then greedily append the
// following rows that are pure URL continuation (non-empty, no interior
// whitespace) until a blank line or a line with spaces (e.g. "Paste code here").
func extractLoginURL(pane string) string {
	s := stripANSI(pane)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		idx := strings.Index(ln, "https://")
		if idx < 0 {
			continue
		}
		// Start of the URL on this row. If more (space-separated) text follows on
		// the same row, the URL ends at that space.
		first := strings.TrimRight(ln[idx:], " \t\r")
		if sp := strings.IndexAny(first, " \t"); sp >= 0 {
			first = first[:sp]
		}
		var b strings.Builder
		b.WriteString(first)
		// Append hard-wrapped continuation rows.
		for j := i + 1; j < len(lines); j++ {
			ct := strings.TrimSpace(lines[j])
			if ct == "" || strings.ContainsAny(ct, " \t") {
				break // blank or contains spaces → past the end of the URL
			}
			b.WriteString(ct)
		}
		if m := loginURLRE.FindString(b.String()); m != "" {
			return m
		}
	}
	// Fallback: single-line match anywhere in the snapshot.
	return loginURLRE.FindString(s)
}

// paneAwaitingLoginContinue reports whether the pane is at the post-login
// "Login successful. Press Enter to continue…" screen (arg is ANSI-stripped
// lowercase). Advancing = send Enter.
func paneAwaitingLoginContinue(low string) bool {
	return strings.Contains(low, "login successful") && strings.Contains(low, "press enter to continue")
}

// paneAwaitingManagedSettings reports whether the pane is at Claude's
// "Managed settings require approval" gate (hooks trust) shown on boot after
// login (arg is ANSI-stripped lowercase). Advancing = pick "1. Yes, I trust".
func paneAwaitingManagedSettings(low string) bool {
	return strings.Contains(low, "managed settings require approval") &&
		strings.Contains(low, "trust these settings")
}

// paneAwaitingLoginMethod reports whether the pane is at Claude's "Select login
// method" menu (shown right after /login, before the OAuth URL). We must pick
// option 1 (Claude subscription) to advance to the URL. Require both the header
// and the subscription option so ordinary text can't trip it.
func paneAwaitingLoginMethod(pane string) bool {
	low := strings.ToLower(stripANSI(pane))
	return strings.Contains(low, "select login method") &&
		strings.Contains(low, "subscription")
}

// paneAwaitingPasteCode reports whether the pane is at Claude's "Paste code here"
// prompt — the state where sending the OAuth code (send-keys) completes login.
func paneAwaitingPasteCode(pane string) bool {
	low := strings.ToLower(stripANSI(pane))
	return strings.Contains(low, "paste code here") || strings.Contains(low, "paste the code") ||
		(strings.Contains(low, "paste") && strings.Contains(low, "code") && strings.Contains(low, "prompted"))
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
