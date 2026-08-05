package channelagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildAgentCardOnlyIncludesEnabledAgents(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "codereview", Description: "reviews code", Capabilities: []string{"read"}, Enabled: true},
		{Name: "secret-client-work", Description: "private", Enabled: false},
	}}
	card := BuildAgentCard("https://example.test/a2a", s)
	if len(card.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(card.Skills))
	}
	if card.Skills[0].ID != "codereview" {
		t.Fatalf("skill ID = %q", card.Skills[0].ID)
	}
	blob, _ := json.Marshal(card)
	if strings.Contains(string(blob), "secret-client-work") {
		t.Fatal("disabled agent leaked into Agent Card")
	}
}

func TestBuildAgentCardNeverLeaksBindings(t *testing.T) {
	card := BuildAgentCard("https://example.test/a2a", AgentStore{})
	blob, _ := json.Marshal(card)
	for _, forbidden := range []string{"cc-", "fatgame", "bindings.json"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("Agent Card leaked %q", forbidden)
		}
	}
}

func TestBuildAgentCardSetsURLAndVersion(t *testing.T) {
	card := BuildAgentCard("https://example.test/a2a", AgentStore{})
	if card.URL != "https://example.test/a2a" {
		t.Fatalf("URL = %q", card.URL)
	}
	if card.ProtocolVersion == "" || card.Name == "" {
		t.Fatalf("card missing identity fields: %#v", card)
	}
}
