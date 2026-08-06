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

// Add 會驗證名字，但 Add 沒有正式呼叫端 —— agents.json 目前只能手寫，所以
// 驗證必須在 Load 這一側。含 '/' 或 '..' 的名字會流進 SessionNameFor →
// SandboxRoot / SandboxWorktree 與 tmux session 名。
func TestLoadAgentsSkipsInvalidNames(t *testing.T) {
	root := t.TempDir()
	if err := AtomicWriteJSON(AgentsPath(root), map[string]any{"agents": []map[string]any{
		{"name": "pm", "project_dir": "/p/pm", "enabled": true},
		{"name": "../../etc", "project_dir": "/p/x", "enabled": true},
		{"name": "Bad Name", "project_dir": "/p/y", "enabled": true},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "pm" {
		t.Fatalf("agents = %#v, want only pm", got.Agents)
	}
}

// 「唯讀輸出」這個不變量目前只靠慣例維持。一個 ChannelID 與某 binding 相同的
// agent 會讓 dcRoute 把該頻道的人類訊息吃進那個 cc- session。
func TestLoadAgentsSkipsAgentsSharingABindingChannel(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "w", ChannelID: "chan-1", Worktree: t.TempDir(), Root: pathIn(root, "bindings", "w")})

	var agents AgentStore
	_ = agents.Add(Agent{Name: "pm", ProjectDir: "/p/pm", ChannelID: "chan-1", Enabled: true})
	_ = agents.Add(Agent{Name: "ok", ProjectDir: "/p/ok", ChannelID: "chan-2", Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatal(err)
	}

	got, _ := LoadAgents(root)
	if len(got.Agents) != 1 || got.Agents[0].Name != "ok" {
		t.Fatalf("agents = %#v; an agent sharing a binding channel must be dropped", got.Agents)
	}
}

// TestWithAgentsPreservesValidationFailingEntries pins round 2026-08-06
// final review, Minor 4: WithAgents loaded through the filtered LoadAgents,
// so any admin mutation entirely unrelated to a validation-failing entry
// (bad name, or a channel_id clashing with a binding) silently dropped that
// entry from agents.json forever the moment it saved the filtered list back
// — a single POST /api/a2a/agents to create an unrelated agent permanently
// erased "Bad_Name". That also defeats LoadAgentsRaw's typo exemption in
// revokeReasonForRunningTask, which exists precisely so a malformed entry is
// not mistaken for a deliberate removal — an exemption with nothing left to
// exempt once WithAgents has already deleted the entry from disk.
func TestWithAgentsPreservesValidationFailingEntries(t *testing.T) {
	root := t.TempDir()
	if err := AtomicWriteJSON(AgentsPath(root), map[string]any{"agents": []map[string]any{
		{"name": "Bad_Name", "project_dir": "/p/bad", "enabled": true},
		{"name": "good", "project_dir": "/p/good", "enabled": true},
	}}); err != nil {
		t.Fatal(err)
	}

	// 一次跟 Bad_Name 完全無關的 admin 操作：新增另一個 agent。
	if err := WithAgents(root, func(agents *AgentStore) error {
		return agents.Add(Agent{Name: "new", ProjectDir: "/p/new", Enabled: true})
	}); err != nil {
		t.Fatalf("WithAgents: %v", err)
	}

	raw, err := LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	names := map[string]bool{}
	for _, a := range raw.Agents {
		names[a.Name] = true
	}
	if !names["Bad_Name"] {
		t.Fatalf("an unrelated admin mutation must not silently delete a validation-failing entry: got %#v", raw.Agents)
	}
	if !names["good"] || !names["new"] {
		t.Fatalf("both the pre-existing valid entry and the newly added one must survive: got %#v", raw.Agents)
	}
}
