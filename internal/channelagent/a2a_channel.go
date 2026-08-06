package channelagent

import "fmt"

// AgentChannelFor resolves an agent's output channel.
func AgentChannelFor(root, agentName string) (string, bool) {
	agents, err := LoadAgents(root)
	if err != nil {
		return "", false
	}
	a, ok := agents.Get(agentName)
	if !ok || a.ChannelID == "" {
		return "", false
	}
	return a.ChannelID, true
}

// SandboxOutputPrefix labels a line with its originating context. One agent
// channel carries every concurrent task of that agent, so unlabelled output
// interleaves into noise and defeats the monitoring purpose.
func SandboxOutputPrefix(contextID string) string {
	return fmt.Sprintf("[%s]", contextID)
}
