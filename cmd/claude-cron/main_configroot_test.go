package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigRootWalksUp(t *testing.T) {
	base := t.TempDir()
	// config.json sits at <base>/.channel-agent ...
	cfgDir := filepath.Join(base, ".channel-agent")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ...but the caller passes the nested control subdir.
	start := filepath.Join(cfgDir, "control")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := resolveConfigRoot(start); got != cfgDir {
		t.Errorf("resolveConfigRoot(%q) = %q, want %q", start, got, cfgDir)
	}
}

func TestResolveConfigRootFallsBackToStart(t *testing.T) {
	start := t.TempDir() // no config.json anywhere up the chain within this tree
	if got := resolveConfigRoot(start); got != start {
		t.Errorf("resolveConfigRoot with no config.json = %q, want unchanged %q", got, start)
	}
}
