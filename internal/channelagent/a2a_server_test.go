package channelagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestA2AServer(t *testing.T) (*A2AServer, string) {
	t.Helper()
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Description: "d", Capabilities: []string{"read"}, Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	var callers CallerStore
	_ = callers.Register("peer-a", "secret-1")
	callers.Approve("peer-a", []string{"read"})
	if err := SaveCallers(root, callers); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}
	return &A2AServer{Root: root, BaseURL: "https://example.test/a2a", Executor: &StubExecutor{}}, root
}

func postRPC(t *testing.T, h http.Handler, credential, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentCardServedWithoutAuth(t *testing.T) {
	s, _ := newTestA2AServer(t)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var card AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if len(card.Skills) != 1 {
		t.Fatalf("skills = %d", len(card.Skills))
	}
}

func TestUnauthenticatedTaskRejected(t *testing.T) {
	s, root := newTestA2AServer(t)
	stub := s.Executor.(*StubExecutor)
	rec := postRPC(t, s.Handler(), "", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCUnauthorized {
		t.Fatalf("want unauthorized, got %#v", resp.Error)
	}
	// Security property: an unauthenticated request must never create or
	// persist a task, and must never reach the executor.
	if stub.Calls != 0 {
		t.Fatalf("executor calls = %d, want 0 (unauthenticated request must not dispatch)", stub.Calls)
	}
	tasks, _ := LoadTasks(root)
	if _, ok := tasks.ByContext("c1"); ok {
		t.Fatal("task was persisted for an unauthenticated request")
	}
}

func TestUngrantedCapabilityForbidden(t *testing.T) {
	s, root := newTestA2AServer(t)
	agents, _ := LoadAgents(root)
	agents.Agents[0].Capabilities = []string{"write"} // caller only granted "read"
	_ = SaveAgents(root, agents)
	stub := s.Executor.(*StubExecutor)

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCForbidden {
		t.Fatalf("want forbidden, got %#v", resp.Error)
	}
	// Security property: a request forbidden on capability grounds must leave
	// no task persisted and must never reach the executor. This is the
	// ordering guarantee the brief calls out explicitly (reject before dispatch,
	// authenticate/authorize before touching task state).
	if stub.Calls != 0 {
		t.Fatalf("executor calls = %d, want 0 (forbidden request must not dispatch)", stub.Calls)
	}
	tasks, _ := LoadTasks(root)
	if _, ok := tasks.ByContext("c1"); ok {
		t.Fatal("task was persisted for a forbidden request")
	}
}

func TestUnknownAgentRejected(t *testing.T) {
	s, _ := newTestA2AServer(t)
	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"nope","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCInvalidParams {
		t.Fatalf("want invalid params, got %#v", resp.Error)
	}
}

func TestValidTaskIsPersistedAndDispatched(t *testing.T) {
	s, root := newTestA2AServer(t)
	stub := s.Executor.(*StubExecutor)
	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"review this"}}`)
	var resp RPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %#v", resp.Error)
	}
	if stub.Calls != 1 {
		t.Fatalf("executor calls = %d, want 1", stub.Calls)
	}
	if stub.LastPrompt != "review this" {
		t.Fatalf("prompt = %q", stub.LastPrompt)
	}
	tasks, _ := LoadTasks(root)
	tk, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("task not persisted")
	}
	if tk.State != TaskSubmitted {
		t.Fatalf("state = %s", tk.State)
	}
	if tk.Session != SessionNameFor("codereview", "c1") {
		t.Fatalf("session = %q", tk.Session)
	}
}

func TestUnknownMethodRejected(t *testing.T) {
	s, _ := newTestA2AServer(t)
	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"bogus/method","params":{}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCMethodNotFound {
		t.Fatalf("want method not found, got %#v", resp.Error)
	}
}
