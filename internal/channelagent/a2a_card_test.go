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
	s := AgentStore{Agents: []Agent{
		{
			Name:         "codereview",
			ProjectDir:   "/home/conray/project/fatgame-jfg-4908",
			Description:  "reviews code",
			Capabilities: []string{"read"},
			Enabled:      true,
		},
		{Name: "dataseai-secret", Description: "private", Enabled: false},
	}}
	card := BuildAgentCard("https://example.test/a2a", s)
	blob, _ := json.Marshal(card)
	blobStr := string(blob)
	forbidden := []string{
		"cc-",
		"fatgame",
		"bindings.json",
		"/home/conray/project/fatgame-jfg-4908",
		"jfg-4908",
		"dataseai-secret",
	}
	for _, f := range forbidden {
		if strings.Contains(blobStr, f) {
			t.Fatalf("Agent Card leaked %q: %s", f, blobStr)
		}
	}
}

func TestBuildAgentCardMultipleAgentsAndNilCapabilities(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "alpha", Description: "first agent", Capabilities: []string{"read", "write"}, Enabled: true},
		{Name: "beta", Description: "second agent", Capabilities: nil, Enabled: true},
		{Name: "gamma", Description: "third agent", Capabilities: []string{"exec"}, Enabled: true},
		{Name: "omitted", Description: "not enabled", Enabled: false},
	}}
	card := BuildAgentCard("https://example.test/a2a", s)
	if len(card.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d: %#v", len(card.Skills), card.Skills)
	}
	wantOrder := []string{"alpha", "beta", "gamma"}
	for i, id := range wantOrder {
		if card.Skills[i].ID != id {
			t.Fatalf("skill[%d].ID = %q, want %q (order not deterministic): %#v", i, card.Skills[i].ID, id, card.Skills)
		}
	}

	blob, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blobStr := string(blob)
	if strings.Contains(blobStr, `"tags":null`) {
		t.Fatalf("nil Capabilities rendered as null tags instead of being omitted: %s", blobStr)
	}

	var decoded struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasTags := decoded.Skills[1]["tags"]; hasTags {
		t.Fatalf("beta skill (nil Capabilities) should omit \"tags\" entirely, got: %#v", decoded.Skills[1])
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
