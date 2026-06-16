package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvWalksUpAndDoesNotOverwrite(t *testing.T) {
	repo := t.TempDir()
	// .env lives at the repo root...
	envPath := filepath.Join(repo, ".env")
	content := "# comment line\nDISCORD_BOT_TOKEN=\"tok-from-env\"\nKEEP_EXISTING=from-file\nMALFORMED\n\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// ...while we invoke from a nested root, like .channel-agent/control.
	start := filepath.Join(repo, ".channel-agent", "control")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}

	// An already-set var must win over the .env value.
	t.Setenv("KEEP_EXISTING", "from-process")
	// Ensure the loaded var is unset beforehand and cleaned up after.
	os.Unsetenv("DISCORD_BOT_TOKEN")
	t.Cleanup(func() { os.Unsetenv("DISCORD_BOT_TOKEN") })

	loadDotEnv(start)

	if got := os.Getenv("DISCORD_BOT_TOKEN"); got != "tok-from-env" {
		t.Errorf("DISCORD_BOT_TOKEN = %q, want %q (quotes stripped, walked up from nested root)", got, "tok-from-env")
	}
	if got := os.Getenv("KEEP_EXISTING"); got != "from-process" {
		t.Errorf("KEEP_EXISTING = %q, want %q (must not overwrite existing env)", got, "from-process")
	}
}

func TestLoadDotEnvMissingFileIsNoop(t *testing.T) {
	// No .env anywhere under the temp dir: must not panic or set anything.
	loadDotEnv(t.TempDir())
}
