package channelagent

import (
	"path/filepath"
	"testing"
)

func TestAgentStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents on empty root: %v", err)
	}
	if len(s.Agents) != 0 {
		t.Fatalf("empty root should give 0 agents, got %d", len(s.Agents))
	}

	if err := s.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Description: "reviews code", Capabilities: []string{"read"}, Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, s); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	a, ok := got.Get("codereview")
	if !ok {
		t.Fatal("agent missing after reload")
	}
	if a.ProjectDir != "/p/x" || !a.Enabled || len(a.Capabilities) != 1 {
		t.Fatalf("round-trip lost data: %#v", a)
	}
}

func TestAgentStoreRejectsDuplicateAndBadName(t *testing.T) {
	var s AgentStore
	if err := s.Add(Agent{Name: "dup"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := s.Add(Agent{Name: "dup"}); err == nil {
		t.Fatal("duplicate name must be rejected")
	}
	if err := s.Add(Agent{Name: "Bad Name"}); err == nil {
		t.Fatal("invalid name must be rejected")
	}
}

func TestAgentsPathIsNotBindingsJSON(t *testing.T) {
	root := t.TempDir()
	if got := AgentsPath(root); got != filepath.Join(root, "agents.json") {
		t.Fatalf("AgentsPath = %q", got)
	}
	if AgentsPath(root) == RegistryPath(root) {
		t.Fatal("agents store must not collide with bindings.json")
	}
}

func TestAgentStoreEnabledFiltersDisabled(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "on", Enabled: true},
		{Name: "off", Enabled: false},
	}}
	got := s.Enabled()
	if len(got) != 1 || got[0].Name != "on" {
		t.Fatalf("Enabled() = %#v", got)
	}
}
