package channelagent

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// Agent is an A2A-exposed identity: aa-<Name>. Unlike a Binding it has no
// channel and never executes work itself — tasks run in per-contextId
// aa-<Name>-<ctx> instances.
type Agent struct {
	Name        string `json:"name"`
	ProjectDir  string `json:"project_dir"`
	Description string `json:"description"`
	// Capabilities 是**路由標籤**,不是沙盒權限。dispatch 當下要求呼叫方持有
	// 這裡宣告的每一項(宣告零項的 agent fail-closed),但它對沙盒實際能做
	// 什麼零影響 —— 那由任務的 GrantLevel 與 a2a_gate.go 決定。
	Capabilities []string `json:"capabilities"`
	Enabled      bool     `json:"enabled"`
	// ChannelID is this agent identity's output-only Discord channel. All of the
	// agent's concurrent tasks stream there so an operator can see whether work
	// is actually progressing. It is NEVER ingested: reading it would let anyone
	// who can type in Discord drive a sandbox, bypassing A2A auth entirely.
	ChannelID string `json:"channel_id,omitempty"`
}

type AgentStore struct {
	Agents []Agent `json:"agents"`
}

// a2aNameRe mirrors the binding name rule: lowercase letters, digits, dashes.
var a2aNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func AgentsPath(root string) string { return filepath.Join(root, "agents.json") }

// LoadAgents 讀 agents.json，並在回傳前套用兩條驗證。Add 已經驗證過名字，但
// Add 沒有正式呼叫端（agents.json 目前只能手寫），所以驗證必須在這一側。
//
//  1. 名字必須符合 a2aNameRe：含 '/' 或 '..' 的名字會流進 SessionNameFor →
//     SandboxRoot / SandboxWorktree 與 tmux session 名。
//  2. ChannelID 不得與任何 binding 的 channel 相同：那會讓 dcRoute
//     （supervisor.go）把該頻道的人類訊息吃進那個 cc- session，破壞「agent
//     頻道唯讀輸出」這個不變量。bindings.json 只讀不寫。
//
// 兩者都是「跳過並 log」而不是整份載入失敗：一個手寫錯誤不該讓所有 agent 消失。
func LoadAgents(root string) (AgentStore, error) {
	var s AgentStore
	if err := ReadJSON(AgentsPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AgentStore{}, nil
		}
		return AgentStore{}, err
	}
	bindingChannels := map[string]string{}
	if reg, err := LoadRegistry(root); err == nil {
		for _, b := range reg.Bindings {
			if b.ChannelID != "" {
				bindingChannels[b.ChannelID] = b.Name
			}
		}
	}
	kept := s.Agents[:0]
	for _, a := range s.Agents {
		if !a2aNameRe.MatchString(a.Name) {
			log.Printf("a2a: 跳過 agent %q：名稱不合法（只允許小寫字母、數字、連字號）", a.Name)
			continue
		}
		if name, clash := bindingChannels[a.ChannelID]; a.ChannelID != "" && clash {
			log.Printf("a2a: 跳過 agent %q：它的 channel_id 與 binding %q 相同，那會讓該頻道的人類訊息被 ingest 進 cc- session", a.Name, name)
			continue
		}
		kept = append(kept, a)
	}
	s.Agents = kept
	return s, nil
}

func SaveAgents(root string, s AgentStore) error {
	return AtomicWriteJSON(AgentsPath(root), s)
}

func (s *AgentStore) Get(name string) (Agent, bool) {
	for _, a := range s.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

func (s *AgentStore) Add(a Agent) error {
	if !a2aNameRe.MatchString(a.Name) {
		return fmt.Errorf("invalid agent name %q: use lowercase letters, digits, dashes", a.Name)
	}
	if _, exists := s.Get(a.Name); exists {
		return fmt.Errorf("agent %q already exists", a.Name)
	}
	s.Agents = append(s.Agents, a)
	return nil
}

func (s *AgentStore) Remove(name string) bool {
	for i, a := range s.Agents {
		if a.Name == name {
			s.Agents = append(s.Agents[:i], s.Agents[i+1:]...)
			return true
		}
	}
	return false
}

// Enabled returns only agents that opted in to being advertised.
func (s AgentStore) Enabled() []Agent {
	var out []Agent
	for _, a := range s.Agents {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}
