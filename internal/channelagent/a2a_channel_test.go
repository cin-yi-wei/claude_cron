package channelagent

import (
	"strings"
	"testing"
)

func TestAgentChannelResolves(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "pm", ProjectDir: "/p/x", ChannelID: "chan-pm", Enabled: true})
	_ = SaveAgents(root, agents)

	got, ok := AgentChannelFor(root, "pm")
	if !ok || got != "chan-pm" {
		t.Fatalf("AgentChannelFor = %q, %v", got, ok)
	}
	if _, ok := AgentChannelFor(root, "ghost"); ok {
		t.Fatal("unknown agent must not resolve a channel")
	}
}

// Concurrent tasks of one agent share its channel, so every line must say which
// context it came from or the stream is unreadable.
func TestSandboxOutputPrefixIdentifiesContext(t *testing.T) {
	a := SandboxOutputPrefix("ctxAAA")
	b := SandboxOutputPrefix("ctxBBB")
	if a == b {
		t.Fatal("different contexts must produce different prefixes")
	}
	if !strings.Contains(a, "ctxAAA") {
		t.Fatalf("prefix %q does not identify its context", a)
	}
}

// The agent channel is output-only. If it were ingested, anyone who can type in
// Discord could drive a sandbox directly, bypassing A2A authentication and
// capability grants. Nothing may register it as an input source.
func TestAgentChannelIsNeverAnInputSource(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "pm", ProjectDir: "/p/x", ChannelID: "chan-pm", Enabled: true})
	_ = SaveAgents(root, agents)

	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, ok := reg.BindingByChannel("chan-pm"); ok {
		t.Fatal("an agent channel must never resolve to a binding — that would make it ingestible")
	}
}
