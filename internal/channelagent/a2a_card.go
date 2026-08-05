package channelagent

// AgentCardSkill advertises one agent. The ID is the agent name; a caller names
// it when submitting a task.
type AgentCardSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

// AgentCard is the public discovery document served at /.well-known/agent.json.
// Only opted-in agents appear: binding names would leak project and client info.
type AgentCard struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Name            string           `json:"name"`
	Description     string           `json:"description"`
	URL             string           `json:"url"`
	Skills          []AgentCardSkill `json:"skills"`
}

func BuildAgentCard(baseURL string, s AgentStore) AgentCard {
	card := AgentCard{
		ProtocolVersion: "0.2.0",
		Name:            "claude_cron",
		Description:     "Delegated task execution in isolated sandboxes.",
		URL:             baseURL,
		Skills:          []AgentCardSkill{},
	}
	for _, a := range s.Enabled() {
		card.Skills = append(card.Skills, AgentCardSkill{
			ID:          a.Name,
			Name:        a.Name,
			Description: a.Description,
			Tags:        a.Capabilities,
		})
	}
	return card
}
