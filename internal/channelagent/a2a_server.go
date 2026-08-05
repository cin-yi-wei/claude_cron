package channelagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// TaskExecutor dispatches an accepted task into a sandbox. Phase 2 ships only
// StubExecutor; Phase 3 adds the real one. Keeping this an interface is what
// lets every test run without tmux.
type TaskExecutor interface {
	Start(ctx context.Context, task A2ATask, prompt string) error
}

// StubExecutor records calls and does nothing else.
type StubExecutor struct {
	Calls      int
	LastTask   A2ATask
	LastPrompt string
}

func (s *StubExecutor) Start(_ context.Context, task A2ATask, prompt string) error {
	s.Calls++
	s.LastTask = task
	s.LastPrompt = prompt
	return nil
}

// MessageSendParams is the params body of the message/send method.
type MessageSendParams struct {
	Agent     string `json:"agent"`
	ContextID string `json:"contextId"`
	Text      string `json:"text"`
	TaskID    string `json:"taskId"`
}

// A2AServer serves the Agent Card and the JSON-RPC endpoint. It MUST be mounted
// on a port separate from the admin API, which can create shell-capable
// bindings and must never be externally reachable.
type A2AServer struct {
	Root     string
	BaseURL  string
	Executor TaskExecutor
}

func (s *A2AServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent.json", s.handleCard)
	mux.HandleFunc("/a2a", s.handleRPC)
	return mux
}

func (s *A2AServer) handleCard(w http.ResponseWriter, r *http.Request) {
	agents, err := LoadAgents(s.Root)
	if err != nil {
		http.Error(w, "agent store unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(BuildAgentCard(s.BaseURL, agents))
}

func writeRPC(w http.ResponseWriter, resp RPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func (s *A2AServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPC(w, RPCFail(nil, RPCParseError, "unreadable body"))
		return
	}
	req, rerr := ParseRPC(body)
	if rerr != nil {
		writeRPC(w, RPCFail(nil, rerr.Code, rerr.Message))
		return
	}

	callers, err := LoadCallers(s.Root)
	if err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "caller store unavailable"))
		return
	}
	caller, ok := callers.Authenticate(bearer(r))
	if !ok {
		writeRPC(w, RPCFail(req.ID, RPCUnauthorized, "unknown or unapproved caller"))
		return
	}

	if req.Method != "message/send" {
		writeRPC(w, RPCFail(req.ID, RPCMethodNotFound, "unsupported method "+req.Method))
		return
	}

	var p MessageSendParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "malformed params"))
		return
	}
	if p.Agent == "" || p.ContextID == "" {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "agent and contextId are required"))
		return
	}

	agents, err := LoadAgents(s.Root)
	if err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "agent store unavailable"))
		return
	}
	agent, ok := agents.Get(p.Agent)
	if !ok || !agent.Enabled {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "unknown agent "+p.Agent))
		return
	}

	// The grant list is the whole policy: every capability the agent needs must
	// have been granted to this caller. No runtime prompt.
	for _, need := range agent.Capabilities {
		if !caller.Allows(need) {
			writeRPC(w, RPCFail(req.ID, RPCForbidden, "caller lacks capability "+need))
			return
		}
	}

	tasks, err := LoadTasks(s.Root)
	if err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "task store unavailable"))
		return
	}
	task := A2ATask{
		ContextID: p.ContextID,
		TaskID:    p.TaskID,
		Agent:     agent.Name,
		CallerID:  caller.CallerID,
		Session:   SessionNameFor(agent.Name, p.ContextID),
		State:     TaskSubmitted,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Prompt:    p.Text,
	}
	tasks.Upsert(task)
	if err := SaveTasks(s.Root, tasks); err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "cannot persist task"))
		return
	}

	if err := s.Executor.Start(r.Context(), task, p.Text); err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "dispatch failed: "+err.Error()))
		return
	}

	writeRPC(w, RPCOK(req.ID, map[string]any{
		"contextId": task.ContextID,
		"taskId":    task.TaskID,
		"state":     string(task.State),
	}))
}
