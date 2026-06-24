package channelagent

import "testing"

func TestClaudeOAuthTokenConfigured(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if claudeOAuthTokenConfigured() {
		t.Fatal("empty token should report not configured")
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "   ")
	if claudeOAuthTokenConfigured() {
		t.Fatal("whitespace-only token should report not configured")
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-xxx")
	if !claudeOAuthTokenConfigured() {
		t.Fatal("a real token should report configured")
	}
}
