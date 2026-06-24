package channelagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeOAuthTokenConfigured reports whether a long-lived subscription OAuth
// token is set (CLAUDE_CODE_OAUTH_TOKEN). When it is, session auth comes from
// that token — injected into every session at startup — NOT from the interactive
// ~/.claude/.credentials.json login. A login-looking screen then just means the
// live process predates the token; a restart re-injects it. Nagging the user to
// run /login is wrong in that case: /login won't help and isn't needed.
func claudeOAuthTokenConfigured() bool {
	return strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")) != ""
}

// claudeCredentialsPath is Claude Code's OAuth credentials file (subscription
// login). Shared by every cc-* session on the host.
func claudeCredentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// claudeCredsValid reports whether the stored subscription OAuth access token is
// currently valid (expiresAt in the future). Used to decide whether a session
// stuck on "Please run /login" can be auto-fixed by a restart (creds are fresh,
// the live process just holds a stale token) or genuinely needs a human /login
// (creds also expired). ok=false when the file is missing/unreadable.
func claudeCredsValid() (valid bool, ok bool) {
	p := claudeCredentialsPath()
	if p == "" {
		return false, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return false, false
	}
	var c struct {
		ClaudeAiOauth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &c) != nil || c.ClaudeAiOauth.ExpiresAt == 0 {
		return false, false
	}
	// expiresAt is epoch ms. Treat as valid only with a small safety margin.
	return c.ClaudeAiOauth.ExpiresAt > nowUnixMilli()+30_000, true
}

// nowUnixMilli is a tiny indirection so tests can avoid the wall clock if needed.
var nowUnixMilli = func() int64 { return time.Now().UnixMilli() }
