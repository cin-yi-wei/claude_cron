package channelagent

import (
	"path/filepath"
	"testing"
)

func TestRegistryAddGetRemoveRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry empty: %v", err)
	}
	if len(reg.Bindings) != 0 {
		t.Fatalf("expected empty registry, got %d", len(reg.Bindings))
	}

	b := BindingDefaults(root, "proj-a", "/home/u/a", "ticket-1")
	if err := reg.Add(b); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := reg.Add(b); err == nil {
		t.Fatal("duplicate Add should error")
	}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	reloaded, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	got, ok := reloaded.Get("proj-a")
	if !ok {
		t.Fatal("Get proj-a not found")
	}
	if got.TmuxSession != "cc-proj-a" {
		t.Fatalf("TmuxSession = %q, want cc-proj-a", got.TmuxSession)
	}
	if got.Worktree != "/home/u/proj-a" { // sibling of project dir /home/u/a
		t.Fatalf("Worktree = %q", got.Worktree)
	}
	if got.Root != filepath.Join(root, "bindings", "proj-a") {
		t.Fatalf("Root = %q", got.Root)
	}
	if !reloaded.Remove("proj-a") {
		t.Fatal("Remove returned false")
	}
	if _, ok := reloaded.Get("proj-a"); ok {
		t.Fatal("proj-a still present after Remove")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"proj-a", "abc123", "x"} {
		if !ValidName(ok) {
			t.Fatalf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"Proj", "a_b", "a b", "", "a/b"} {
		if ValidName(bad) {
			t.Fatalf("ValidName(%q) = true, want false", bad)
		}
	}
}
