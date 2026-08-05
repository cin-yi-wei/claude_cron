package channelagent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
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

// a2aContextIDRe bounds the caller-controlled contextId. SessionNameFor feeds
// it through sanitize (watcher.go), which strips every non-alphanumeric
// character. If this let through dashes, underscores, dots, spaces, etc.,
// two different contextIds (e.g. "c-1" and "c_1") could sanitize down to the
// same session name and collide once real sandboxes exist. Restricting the
// charset to plain alphanumerics makes sanitize a no-op on any accepted
// contextId, so distinct valid contextIds can never collide downstream.
// 1-128 chars is a conservative bound on a caller-supplied token.
var a2aContextIDRe = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

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
	if !a2aContextIDRe.MatchString(p.ContextID) {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "contextId must be 1-128 alphanumeric characters"))
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
	// have been granted to this caller. No runtime prompt. An agent that
	// declares zero capabilities must fail closed, not open — it must state
	// what it needs in order to be callable at all.
	if len(agent.Capabilities) == 0 {
		writeRPC(w, RPCFail(req.ID, RPCForbidden, "agent "+agent.Name+" declares no capabilities and cannot be called"))
		return
	}
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

	// contextId is caller-controlled. Without an ownership check, a second
	// caller could reuse another caller's contextId and overwrite its task row
	// (CallerID, Session, State), making the original task unbookkeepable. A
	// caller may only reuse a contextId that is unclaimed, terminal, or
	// already theirs.
	if existing, ok := tasks.ByContext(p.ContextID); ok {
		active := existing.State == TaskSubmitted || existing.State == TaskWorking
		if active && existing.CallerID != caller.CallerID {
			writeRPC(w, RPCFail(req.ID, RPCForbidden, "contextId is owned by another caller"))
			return
		}
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

	if !HasCapacity(tasks) {
		// Queued, not rejected: it stays in TaskSubmitted for DrainQueue.
		writeRPC(w, RPCOK(req.ID, map[string]any{
			"contextId": task.ContextID,
			"taskId":    task.TaskID,
			"state":     string(TaskSubmitted),
			"queued":    true,
		}))
		return
	}

	if err := s.Executor.Start(r.Context(), task, p.Text); err != nil {
		// Never echo the underlying error to an internet-facing caller: once
		// the real executor lands, this detail will carry worktree paths, git
		// output and tmux state — exactly the private project information
		// this design exists to keep off the wire. Log it server-side instead,
		// and mark the task failed so it stops occupying capacity forever.
		log.Printf("a2a: dispatch failed for task %s (agent=%s contextId=%s): %v", task.TaskID, task.Agent, task.ContextID, err)
		task.State = TaskFailed
		task.Detail = err.Error()
		tasks.Upsert(task)
		if serr := SaveTasks(s.Root, tasks); serr != nil {
			log.Printf("a2a: failed to persist failed task state for %s/%s: %v", task.Agent, task.ContextID, serr)
		}
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "dispatch failed"))
		return
	}

	writeRPC(w, RPCOK(req.ID, map[string]any{
		"contextId": task.ContextID,
		"taskId":    task.TaskID,
		"state":     string(task.State),
	}))
}
