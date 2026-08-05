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

func TestAgentStoreRemove(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}}

	if ok := s.Remove("b"); !ok {
		t.Fatal("Remove(existing) should return true")
	}
	if _, ok := s.Get("b"); ok {
		t.Fatal("removed agent should be absent from Get")
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a should still be present")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("c should still be present")
	}
	if len(s.Agents) != 2 {
		t.Fatalf("expected 2 agents remaining, got %d: %#v", len(s.Agents), s.Agents)
	}

	if ok := s.Remove("nope"); ok {
		t.Fatal("Remove(missing) should return false")
	}
	if len(s.Agents) != 2 {
		t.Fatalf("Remove(missing) must not change length, got %d", len(s.Agents))
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a should still be present after no-op remove")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("c should still be present after no-op remove")
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
