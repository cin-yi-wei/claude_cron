package channelagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestA2AServer(t *testing.T) (*A2AServer, string) {
	t.Helper()
	// unauthorizedAudits is package-level (shared across every request this
	// process ever serves, in test or in prod) so the per-source 1/second
	// throttle survives process restarts and works across every A2AServer
	// instance. In tests that means every case in this file shares the same
	// map — and httptest.NewRequest always stamps the same default
	// RemoteAddr, so without a reset here the very first unauthorized
	// request in ANY earlier test would still be "seen" a moment later and
	// silently suppress the first entry a later test expects to land.
	unauthorizedAudits.mu.Lock()
	unauthorizedAudits.seen = map[string]time.Time{}
	unauthorizedAudits.mu.Unlock()
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
	// task 6: the capacity claim (submitted -> dispatching) happens in the
	// same locked section that upserts the row, before the executor is ever
	// called — so by the time this handler call has returned successfully,
	// the row is already TaskDispatching, not TaskSubmitted. StubExecutor
	// never writes TaskWorking itself (that is SandboxExecutor's job once a
	// real sandbox is actually up), so this is the terminal state a stub
	// dispatch leaves behind.
	if tk.State != TaskDispatching {
		t.Fatalf("state = %s, want dispatching", tk.State)
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

// --- Fix-round coverage (review findings 1-5) ---

// failingExecutor lets tests force a dispatch failure without touching tmux.
type failingExecutor struct {
	err error
}

func (f *failingExecutor) Start(_ context.Context, _ A2ATask, _ string) error {
	return f.err
}

// Finding 1: a dispatch failure must never echo the underlying error text
// (worktree paths, git/tmux output) to the internet-facing caller.
func TestDispatchFailureMessageIsGeneric(t *testing.T) {
	s, _ := newTestA2AServer(t)
	secretErr := errors.New("worktree /home/conray/private-project: git checkout failed")
	s.Executor = &failingExecutor{err: secretErr}

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatal("want an error, got success")
	}
	if resp.Error.Code != RPCInternalError {
		t.Fatalf("code = %d, want RPCInternalError", resp.Error.Code)
	}
	if strings.Contains(resp.Error.Message, "private-project") || strings.Contains(resp.Error.Message, "git checkout") {
		t.Fatalf("internal error detail leaked to caller: %q", resp.Error.Message)
	}
}

// Finding 4: a failed dispatch must not leave the task row stuck in a
// non-terminal state forever, silently eating capacity.
func TestFailedDispatchMarksTaskFailedNotSubmitted(t *testing.T) {
	s, root := newTestA2AServer(t)
	s.Executor = &failingExecutor{err: errors.New("boom")}

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatal("want an error, got success")
	}

	tasks, _ := LoadTasks(root)
	tk, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("task not persisted")
	}
	if tk.State != TaskFailed {
		t.Fatalf("state = %s, want failed (a failed dispatch must not stay submitted forever)", tk.State)
	}
	if tk.Detail == "" {
		t.Fatal("Detail should carry the failure reason server-side")
	}
	if tasks.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d, want 0 (a failed dispatch must not keep occupying capacity)", tasks.ActiveCount())
	}
}

// Finding 2: an agent that declares zero capabilities must fail closed, not
// open. The grant list is stated to be the entire policy; an agent that asks
// for nothing must not be callable by everyone.
func TestZeroCapabilityAgentDeniedByDefault(t *testing.T) {
	s, root := newTestA2AServer(t)
	agents, _ := LoadAgents(root)
	agents.Agents[0].Capabilities = nil
	_ = SaveAgents(root, agents)

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCForbidden {
		t.Fatalf("want forbidden for a zero-capability agent, got %#v", resp.Error)
	}
	tasks, _ := LoadTasks(root)
	if _, ok := tasks.ByContext("c1"); ok {
		t.Fatal("task was persisted for a denied zero-capability agent")
	}
}

// Finding 3a: a contextId is caller-controlled. A second caller must not be
// able to hijack another caller's contextId and overwrite its task row, while
// the original owner resubmitting the same contextId (a legitimate
// multi-turn continuation) must still work.
func TestContextIDOwnershipEnforced(t *testing.T) {
	s, root := newTestA2AServer(t)
	callers, _ := LoadCallers(root)
	_ = callers.Register("peer-b", "secret-2")
	callers.Approve("peer-b", []string{"read"})
	if err := SaveCallers(root, callers); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}

	// peer-a opens context c1.
	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("setup: unexpected error: %#v", resp.Error)
	}

	// peer-a continuing its own context must still be allowed.
	rec = postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"follow up"}}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("owner continuation: unexpected error: %#v", resp.Error)
	}

	// peer-b trying to hijack peer-a's contextId must be forbidden.
	rec = postRPC(t, s.Handler(), "secret-2", `{"jsonrpc":"2.0","id":3,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"steal"}}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCForbidden {
		t.Fatalf("want forbidden for a contextId hijack attempt, got %#v", resp.Error)
	}

	tasks, _ := LoadTasks(root)
	tk, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("task missing")
	}
	if tk.CallerID != "peer-a" {
		t.Fatalf("CallerID = %q, want peer-a (task must not be hijacked by another caller)", tk.CallerID)
	}
	if tk.Prompt != "follow up" {
		t.Fatalf("Prompt = %q, want the owner's last message, not the hijacker's", tk.Prompt)
	}
}

// Finding 3b: contextId is fed through sanitize() (watcher.go) inside
// SessionNameFor, which strips every non-alphanumeric character. Reject
// anything outside a conservative alphanumeric charset and length bound at
// the boundary so two distinct accepted contextIds can never collide once
// they reach SessionNameFor.
func TestContextIDCharsetAndLengthValidated(t *testing.T) {
	s, _ := newTestA2AServer(t)
	invalid := []string{
		"c 1",                    // space
		"c.1",                    // dot
		"c/1",                    // slash
		"c-1",                    // dash: would sanitize to the same session as "c1"
		"c_1",                    // underscore: same collision risk
		strings.Repeat("a", 129), // over the length bound
	}
	for _, cid := range invalid {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":%q,"text":"hi"}}`, cid)
		rec := postRPC(t, s.Handler(), "secret-1", body)
		var resp RPCResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error == nil || resp.Error.Code != RPCInvalidParams {
			t.Fatalf("contextId %q: want invalid params, got %#v", cid, resp.Error)
		}
	}
}

// Finding 5: a disabled agent must be indistinguishable from one that does
// not exist at all, so the card's opt-in exposure cannot be probed around.
func TestDisabledAgentIndistinguishableFromNonexistent(t *testing.T) {
	s, root := newTestA2AServer(t)
	agents, _ := LoadAgents(root)
	_ = agents.Add(Agent{Name: "ghost", ProjectDir: "/p/y", Description: "d", Capabilities: []string{"read"}, Enabled: false})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"ghost","contextId":"c2","text":"hi"}}`
	rec1 := postRPC(t, s.Handler(), "secret-1", body)
	var resp1 RPCResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &resp1)

	// Now the agent doesn't exist at all.
	agents2, _ := LoadAgents(root)
	agents2.Remove("ghost")
	if err := SaveAgents(root, agents2); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	rec2 := postRPC(t, s.Handler(), "secret-1", body)
	var resp2 RPCResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)

	if resp1.Error == nil || resp2.Error == nil {
		t.Fatalf("expected errors in both cases, got %#v / %#v", resp1.Error, resp2.Error)
	}
	if resp1.Error.Code != RPCInvalidParams {
		t.Fatalf("disabled-agent code = %d, want RPCInvalidParams", resp1.Error.Code)
	}
	if resp1.Error.Code != resp2.Error.Code || resp1.Error.Message != resp2.Error.Message {
		t.Fatalf("disabled vs nonexistent agent responses differ: %#v vs %#v", resp1.Error, resp2.Error)
	}
}

// Finding 5: the request body must be size-limited. We can't directly assert
// on memory use from a unit test, but we can assert the security-relevant
// consequence: an oversized body must never result in an accepted, persisted
// task — that would mean the limit silently stopped applying.
func TestOversizedBodyNeverProducesATask(t *testing.T) {
	s, root := newTestA2AServer(t)
	huge := strings.Repeat("a", 2<<20) // 2MiB, past the 1MiB io.LimitReader cap
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":%q}}`, huge)
	if len(body) <= 1<<20 {
		t.Fatalf("test body not large enough: %d bytes", len(body))
	}

	rec := postRPC(t, s.Handler(), "secret-1", body)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatalf("want an error for an oversized body, got success: %#v", resp.Result)
	}
	tasks, _ := LoadTasks(root)
	if _, ok := tasks.ByContext("c1"); ok {
		t.Fatal("oversized body must never result in a persisted task")
	}
}

// Task 12: every accepted/rejected delegation must leave a durable audit
// trail — the endpoint is externally reachable with no interactive prompt,
// so this log is the only record of who asked for what. Fix rounds: the
// header's "every accepted/rejected delegation" promise covers six distinct
// outcomes, each independently distinguishable in the log.
func TestServerWritesAuditOnAcceptAndDeny(t *testing.T) {
	s, root := newTestA2AServer(t)

	// 1: ordinary accept.
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"ok"}}`)

	// 2: ordinary grant denial (agent needs "write", caller only granted "read").
	agents, _ := LoadAgents(root)
	agents.Agents[0].Capabilities = []string{"write"}
	_ = SaveAgents(root, agents)
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"agent":"codereview","contextId":"c2","text":"denied"}}`)

	// 3: agent misconfigured — declares zero capabilities, so nothing can be
	// granted. Must be distinguishable from an ordinary grant denial.
	agents, _ = LoadAgents(root)
	agents.Agents[0].Capabilities = nil
	_ = SaveAgents(root, agents)
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":3,"method":"message/send","params":{"agent":"codereview","contextId":"c3","text":"nobody home"}}`)

	// Restore a grantable capability for the remaining scenarios.
	agents, _ = LoadAgents(root)
	agents.Agents[0].Capabilities = []string{"read"}
	_ = SaveAgents(root, agents)

	// 4: peer-a opens context c4 (accepted); then peer-b tries to hijack it —
	// the most important path, since it looks like a deliberate attempt to
	// interfere with another caller's task. The entry must name both the
	// rejected caller and the contextId.
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":4,"method":"message/send","params":{"agent":"codereview","contextId":"c4","text":"mine"}}`)
	callers, _ := LoadCallers(root)
	_ = callers.Register("peer-b", "secret-2")
	callers.Approve("peer-b", []string{"read"})
	_ = SaveCallers(root, callers)
	postRPC(t, s.Handler(), "secret-2", `{"jsonrpc":"2.0","id":5,"method":"message/send","params":{"agent":"codereview","contextId":"c4","text":"steal"}}`)

	// 5: dispatch failure — the caller was authorized and the request was
	// well-formed, but the executor errors. This branch mutates task state
	// (TaskFailed) via SaveTasks, so it must not do so silently. The audit
	// Summary must carry the caller's request text, never the executor's raw
	// error (which can carry worktree paths / internal detail — the same leak
	// already fixed once on the response path in TestDispatchFailureMessageIsGeneric).
	secretErr := "worktree /home/conray/private-project: git checkout failed"
	s.Executor = &failingExecutor{err: fmt.Errorf("%s", secretErr)}
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":6,"method":"message/send","params":{"agent":"codereview","contextId":"c6","text":"please fail"}}`)

	// 6: accepted but held at capacity — queued, not dispatched. Fill every
	// sandbox slot directly (bypassing the RPC, matching the pattern used in
	// a2a_lifecycle_test.go) so the next message/send is queued rather than
	// started. Restore a working executor first so this path isn't confused
	// with the dispatch-failure one above.
	s.Executor = &StubExecutor{}
	tasks, _ := LoadTasks(root)
	for i := 0; i < MaxConcurrentSandboxes; i++ {
		tasks.Upsert(A2ATask{ContextID: "full" + string(rune('a'+i)), Agent: "codereview", State: TaskWorking})
	}
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":7,"method":"message/send","params":{"agent":"codereview","contextId":"c5","text":"hold please"}}`)

	entries, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(entries) != 7 {
		t.Fatalf("audit entries = %d, want 7: %#v", len(entries), entries)
	}

	if entries[0].Outcome != "accepted" {
		t.Fatalf("entry 0 outcome = %q, want accepted", entries[0].Outcome)
	}
	if entries[1].Outcome != "forbidden" {
		t.Fatalf("entry 1 outcome = %q, want forbidden", entries[1].Outcome)
	}
	if entries[2].Outcome != "forbidden_no_capabilities" {
		t.Fatalf("entry 2 outcome = %q, want forbidden_no_capabilities (misconfigured agent, distinct from an ordinary grant denial)", entries[2].Outcome)
	}
	if entries[2].Outcome == entries[1].Outcome {
		t.Fatal("zero-capability denial must not share an outcome string with an ordinary grant denial")
	}
	if entries[3].Outcome != "accepted" {
		t.Fatalf("entry 3 (peer-a opens c4) outcome = %q, want accepted", entries[3].Outcome)
	}
	if entries[4].Outcome != "forbidden_hijack" {
		t.Fatalf("entry 4 outcome = %q, want forbidden_hijack", entries[4].Outcome)
	}
	if entries[4].CallerID != "peer-b" {
		t.Fatalf("hijack entry CallerID = %q, want peer-b (the rejected caller)", entries[4].CallerID)
	}
	if entries[4].ContextID != "c4" {
		t.Fatalf("hijack entry ContextID = %q, want c4", entries[4].ContextID)
	}
	if entries[5].Outcome != "dispatch_failed" {
		t.Fatalf("entry 5 outcome = %q, want dispatch_failed", entries[5].Outcome)
	}
	if entries[5].Outcome == entries[0].Outcome || entries[5].Outcome == entries[1].Outcome || entries[5].Outcome == entries[2].Outcome || entries[5].Outcome == entries[4].Outcome {
		t.Fatal("dispatch_failed must not share an outcome string with accepted or any forbidden_* outcome — the caller did nothing wrong here")
	}
	if entries[5].Summary != "please fail" {
		t.Fatalf("dispatch-failure entry Summary = %q, want the caller's request text", entries[5].Summary)
	}
	if strings.Contains(entries[5].Summary, secretErr) || strings.Contains(entries[5].Summary, "private-project") || strings.Contains(entries[5].Summary, "git checkout") {
		t.Fatalf("dispatch-failure audit entry leaked the executor's raw error: %#v", entries[5])
	}
	if entries[6].Outcome != "queued" {
		t.Fatalf("entry 6 outcome = %q, want queued", entries[6].Outcome)
	}
	if entries[6].Outcome == entries[0].Outcome || entries[6].Outcome == entries[3].Outcome {
		t.Fatal("a queued (not dispatched) task must not share an outcome string with an accepted (dispatched) task")
	}
}

// Finding 5: an Authorization header that is present but malformed (wrong
// scheme, or "Bearer" with no token) must be rejected exactly like a missing
// header — not accidentally treated as a valid credential.
// Task 6: ownership must be enforced regardless of task state. SessionNameFor
// and SandboxWorktree are caller-independent, and EnsureWorktree no-ops when
// the path already exists, so a second caller reusing even a terminal (e.g.
// completed) contextId would otherwise inherit the original caller's checkout
// — its uncommitted files, its branch, its sandbox root.
func TestTerminalContextIDCannotBeTakenOverByAnotherCaller(t *testing.T) {
	s, root := newTestA2AServer(t)

	callers, _ := LoadCallers(root)
	_ = callers.Register("peer-b", "secret-2")
	callers.Approve("peer-b", []string{"read"})
	_ = SaveCallers(root, callers)

	// peer-a's task on c1 has finished.
	var tasks TaskStore
	tasks.Upsert(A2ATask{
		ContextID: "c1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskCompleted,
	})
	_ = SaveTasks(root, tasks)

	rec := postRPC(t, s.Handler(), "secret-2",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"take over"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCForbidden {
		t.Fatalf("a different caller must not reuse a terminal contextId, got %#v", resp.Error)
	}

	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.CallerID != "peer-a" {
		t.Fatalf("original owner overwritten: %q", tk.CallerID)
	}
}

func TestSameCallerMayReuseItsOwnTerminalContextID(t *testing.T) {
	s, root := newTestA2AServer(t)
	var tasks TaskStore
	tasks.Upsert(A2ATask{
		ContextID: "c1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskCompleted,
	})
	_ = SaveTasks(root, tasks)

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"follow up"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("the owning caller must be able to follow up: %#v", resp.Error)
	}
}

// ctxRecordingExecutor records whether the context handed to Start was
// already cancelled when dispatch began, so the test below can prove
// dispatch does NOT inherit the request's context.
type ctxRecordingExecutor struct {
	StubExecutor
	sawDone bool
}

func (c *ctxRecordingExecutor) Start(ctx context.Context, task A2ATask, prompt string) error {
	select {
	case <-ctx.Done():
		c.sawDone = true
	default:
	}
	return c.StubExecutor.Start(ctx, task, prompt)
}

// Task 7: a client that disconnects mid-request must not cancel dispatch —
// otherwise a half-built sandbox (git worktree add or tmux start interrupted
// partway) is left behind for the forensics retention rule to preserve
// forever. This test uses an ALREADY-CANCELLED request context, so if
// dispatch used r.Context() directly, ctx.Done() would already be closed by
// the time Start observes it.
func TestDispatchSurvivesClientDisconnect(t *testing.T) {
	s, _ := newTestA2AServer(t)
	ex := &ctxRecordingExecutor{}
	s.Executor = ex

	req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`))
	req.Header.Set("Authorization", "Bearer secret-1")
	cancelled, cancel := context.WithCancel(req.Context())
	cancel() // client has already gone away
	req = req.WithContext(cancelled)

	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if ex.Calls != 1 {
		t.Fatalf("executor calls = %d, want 1", ex.Calls)
	}
	if ex.sawDone {
		t.Fatal("dispatch inherited the cancelled request context")
	}
}

func TestMalformedAuthorizationHeaderRejected(t *testing.T) {
	s, _ := newTestA2AServer(t)
	cases := []struct {
		name   string
		header string
	}{
		{"raw token, no scheme", "secret-1"},
		{"Bearer with empty token", "Bearer "},
		{"wrong scheme", "Basic secret-1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`))
		req.Header.Set("Authorization", c.header)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var resp RPCResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error == nil || resp.Error.Code != RPCUnauthorized {
			t.Fatalf("%s: want unauthorized, got %#v", c.name, resp.Error)
		}
	}
}

// 有效等級 = min(請求的 level, caller.grant_level)。請求高於授權 → RPCForbidden
// 且留一筆稽核。
func TestMessageSendLevelIsCappedByCallerGrant(t *testing.T) {
	s, root := newTestA2AServer(t)
	callers, _ := LoadCallers(root)
	callers.SetGrantLevel("peer-a", GrantDevelop)
	_ = SaveCallers(root, callers)

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","level":"full"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCForbidden {
		t.Fatalf("requesting a level above the grant must be forbidden, got %#v", resp.Error)
	}
	entries, _ := ReadAudit(root)
	if len(entries) == 0 || entries[len(entries)-1].Outcome != "forbidden_level" {
		t.Fatalf("audit tail = %#v", entries)
	}
}

func TestMessageSendDefaultsToCallerGrantLevel(t *testing.T) {
	s, root := newTestA2AServer(t)
	callers, _ := LoadCallers(root)
	callers.SetGrantLevel("peer-a", GrantDevelop)
	_ = SaveCallers(root, callers)

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %#v", resp.Error)
	}
	tasks, _ := LoadTasks(root)
	tk, _ := tasks.ByContext("c1")
	if tk.Level != GrantDevelop {
		t.Fatalf("task level = %q, want %q", tk.Level, GrantDevelop)
	}
}

func TestMessageSendRejectsUnknownLevel(t *testing.T) {
	s, _ := newTestA2AServer(t)
	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","level":"root"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCInvalidParams {
		t.Fatalf("unknown level must be invalid params, got %#v", resp.Error)
	}
}

// D1：同一 contextId 換 agent 會永久孤兒化一個活著的沙盒（SessionNameFor 與
// SandboxWorktree 都含 agent 名，Upsert 以 contextId 為 key 整列覆寫，舊的
// aa-<oldagent>-<ctx> 就不再被任何 row 參照）。拒絕而非拆除：在 handler 內
// 拆掉舊沙盒需要在鎖內碰 tmux / git。
func TestSameContextIDCannotSwitchAgent(t *testing.T) {
	s, root := newTestA2AServer(t)
	agents, _ := LoadAgents(root)
	_ = agents.Add(Agent{Name: "pm", ProjectDir: "/p/pm", Capabilities: []string{"read"}, Enabled: true})
	_ = SaveAgents(root, agents)

	fake := &FakeSessionManager{}
	s.Executor = NewSandboxExecutor(root, fake)

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"first"}}`)
	var first RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first.Error != nil {
		t.Fatalf("first send failed: %#v", first.Error)
	}

	rec = postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"agent":"pm","contextId":"c1","text":"second"}}`)
	var second RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if second.Error == nil || second.Error.Code != RPCForbidden {
		t.Fatalf("switching agent on a live contextId must be forbidden, got %#v", second.Error)
	}
	if len(fake.Started) != 1 {
		t.Fatalf("started %#v; the first sandbox must not be orphaned by a second one", fake.Started)
	}
	entries, _ := ReadAudit(root)
	if len(entries) == 0 || entries[len(entries)-1].Outcome != "forbidden_agent_switch" {
		t.Fatalf("audit tail = %#v", entries)
	}
}

// 規格第五節測試 1：handler 與 DrainQueue 同時對同一個 contextId 動作，只能
// 有一則 prompt 真的落進沙盒。
//
// review round 2, minor 4: 原本靠「另一條 goroutine 跑 20 次 1ms 間隔的
// DrainQueue」去賭中派送視窗 —— production 派送窗口最長 90 秒，而
// FakeSessionManager.Start 微秒級就回來，這個賭法只在修好前的程式碼上*機率
// 性*失敗，不是結構性保證。改用 EnsureWorkspaceHold/Entered 把第一次派送
// 卡死在 EnsureWorkspace 裡，讓「DrainQueue 這時一定會看到 dispatching」變
// 成確定的事，不是碰運氣。
func TestHandlerAndDrainQueueNeverDoubleDispatch(t *testing.T) {
	s, root := newTestA2AServer(t)
	fake := &FakeSessionManager{}
	hold := make(chan struct{})
	entered := make(chan struct{})
	fake.EnsureWorkspaceHold = hold
	fake.EnsureWorkspaceEntered = entered
	ex := NewSandboxExecutor(root, fake)
	s.Executor = ex

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		postRPC(t, s.Handler(), "secret-1",
			`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"go"}}`)
	}()

	<-entered // 派送已經卡在 EnsureWorkspace 裡，row 現在一定是 dispatching。

	var wg2 sync.WaitGroup
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		for i := 0; i < 20; i++ {
			_, _ = DrainQueue(context.Background(), root, ex)
		}
	}()
	wg2.Wait()

	close(hold) // 放開原本那次派送，讓它跑完。
	wg.Wait()

	if n := len(fake.Injected); n != 1 {
		t.Fatalf("injected %d prompts for one contextId; the delegated work would run %d times", n, n)
	}
	if n := len(fake.Workspaces); n != 1 {
		t.Fatalf("EnsureWorkspace called %d times; DrainQueue must never repeat a dispatch already in flight", n)
	}
	if n := len(fake.Started); n != 1 {
		t.Fatalf("session-start step ran %d times; DrainQueue must never Start a row that is already dispatching", n)
	}
}

// 規格第五節測試 2：N 條 goroutine 送 N 個不同 contextId，併發上限必須是硬
// 上限而不是建議值。
//
// review round 2, minor 4: 原本只斷言「不超過上限」，一個什麼都沒派送出去
// 的建置（bug 讓每個請求都誤判成 queued）也會通過這個斷言。40 個不同
// contextId、cap 是 8，且 fake 從不失敗，派送出去的數量是確定的 8，不是
// 「至多 8」。
func TestConcurrentSubmitsRespectTheSandboxCap(t *testing.T) {
	s, root := newTestA2AServer(t)
	fake := &FakeSessionManager{}
	s.Executor = NewSandboxExecutor(root, fake)

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"message/send","params":{"agent":"codereview","contextId":"ctx%02d","text":"go"}}`, i, i)
			postRPC(t, s.Handler(), "secret-1", body)
		}(i)
	}
	wg.Wait()

	if got := len(fake.Started); got != MaxConcurrentSandboxes {
		t.Fatalf("started %d sandboxes, want exactly the cap %d (fake never fails, so all headroom must be used)", got, MaxConcurrentSandboxes)
	}
}

// Important 1（review round 2）：DrainQueue 認領 c1 並翻成 dispatching 之後，
// 它的 Start 還卡在 EnsureWorkspace 裡（production 這裡最長 90 秒）。同一個
// caller 這時對 c1 送出後續訊息。這則後續訊息絕不能自己再認領一次、再呼叫
// 一次 Start —— 那會跟 DrainQueue 正在跑的 EnsureWorkspace/Sessions.Start
// 搶同一個 worktree/session，而且如果後續訊息自己的 Start 之後失敗（git ref
// lock、inject 錯誤），markFailed 會把這一列標成 failed，而 DrainQueue 那邊
// 的 session 其實還活著。用 EnsureWorkspaceHold 把 DrainQueue 的派送卡在
// EnsureWorkspace 裡，保證後續訊息一定會在派送真的還在飛的時候送達，不是
// 賭時間。
func TestFollowUpDuringInFlightDrainQueueDispatchDoesNotDoubleDispatch(t *testing.T) {
	s, root := newTestA2AServer(t)
	fake := &FakeSessionManager{}
	hold := make(chan struct{})
	entered := make(chan struct{})
	fake.EnsureWorkspaceHold = hold
	fake.EnsureWorkspaceEntered = entered
	ex := NewSandboxExecutor(root, fake)
	s.Executor = ex

	// 讓任務一開始就是 submitted 且沒有人碰過它，好讓 DrainQueue（不是
	// handler）是唯一認領並派送它的一方。
	var seed TaskStore
	seed.Upsert(A2ATask{
		ContextID: "c1", Agent: "codereview", CallerID: "peer-a",
		Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted,
		Prompt: "first", StartedAt: time.Now().UTC().Format(time.RFC3339),
		Level: GrantReadOnly,
	})
	if err := SaveTasks(root, seed); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = DrainQueue(context.Background(), root, ex)
	}()

	<-entered // DrainQueue 的 Start 現在真的卡在 EnsureWorkspace 裡。

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"follow up"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("follow-up against an in-flight dispatch must succeed, got %#v", resp.Error)
	}

	close(hold) // 放開 DrainQueue 的派送，讓它跑完。
	wg.Wait()

	if n := len(fake.Workspaces); n != 1 {
		t.Fatalf("EnsureWorkspace called %d times; the follow-up must not repeat DrainQueue's in-flight dispatch", n)
	}
	if n := len(fake.Started); n != 1 {
		t.Fatalf("session-start ran %d times; the follow-up must never call Start again", n)
	}

	tasks, _ := LoadTasks(root)
	tk, _ := tasks.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state = %s, want working (DrainQueue's dispatch, not the follow-up, owns this row's lifecycle)", tk.State)
	}
	if tk.Prompt != "follow up" {
		t.Fatalf("prompt = %q, want the follow-up's text recorded", tk.Prompt)
	}
}

// Important 2（review round 2）：8 個沙盒全滿時對其中一個活著的 working row
// 送後續訊息。舊程式碼在容量檢查算出 false 之後，仍然無條件 Upsert 一個全新
// 的（State=submitted、Worktree/Branch 全空的）task 蓋掉這個活著的 row ——
// RunningCount 從 8 跌到 7、活著的 worktree 立刻失去唯一參照（SweepTimeouts
// 只回收 Worktree 非空的 row），下一個請求就能在滿載時再啟動第 9 個沙盒。
func TestFollowUpOnWorkingRowNeverRegressesCapacityEvenWhenFull(t *testing.T) {
	s, root := newTestA2AServer(t)
	stub := s.Executor.(*StubExecutor)

	var seed TaskStore
	for i := 0; i < MaxConcurrentSandboxes; i++ {
		id := fmt.Sprintf("live%02d", i)
		seed.Upsert(A2ATask{
			ContextID: id, Agent: "codereview", CallerID: "peer-a",
			Session:   SessionNameFor("codereview", id),
			State:     TaskWorking,
			Worktree:  "/p/x-" + id,
			Branch:    "aa/" + id,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	if err := SaveTasks(root, seed); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"live00","text":"follow up"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("follow-up on a live row must succeed even at full capacity, got %#v", resp.Error)
	}
	if stub.Calls != 0 {
		t.Fatalf("Executor.Start called %d times; a follow-up must never re-dispatch", stub.Calls)
	}

	tasks, _ := LoadTasks(root)
	live0, ok := tasks.ByContext("live00")
	if !ok {
		t.Fatal("live00 task disappeared")
	}
	if live0.State != TaskWorking {
		t.Fatalf("state = %s, want working (must not regress to submitted)", live0.State)
	}
	if live0.Worktree != "/p/x-live00" || live0.Branch != "aa/live00" {
		t.Fatalf("identity clobbered: worktree=%q branch=%q", live0.Worktree, live0.Branch)
	}
	if live0.Prompt != "follow up" {
		t.Fatalf("prompt = %q, want the follow-up's text recorded", live0.Prompt)
	}
	if got := tasks.RunningCount(); got != MaxConcurrentSandboxes {
		t.Fatalf("RunningCount = %d, want unchanged %d — a follow-up must never free or cost a capacity slot", got, MaxConcurrentSandboxes)
	}

	// 緊接著送一個全新的 contextId：cap 真的還是滿的，證明剛才那則後續訊息
	// 沒有偷偷放出一個名額。
	rec = postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"agent":"codereview","contextId":"brandnew","text":"go"}}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("new context send failed: %#v", resp.Error)
	}
	tasks, _ = LoadTasks(root)
	nw, ok := tasks.ByContext("brandnew")
	if !ok || nw.State != TaskSubmitted {
		t.Fatalf("new context state = %#v, want queued (submitted) since the cap is still genuinely full", nw)
	}
}

// 對憑證做暴力嘗試會在 a2a-audit.jsonl 產生零行 —— 對一個以「誰要求了什麼的
// 持久紀錄」為存在理由的對外監聽器，這是最該有的一筆。
func TestUnauthorizedRequestIsAudited(t *testing.T) {
	s, root := newTestA2AServer(t)
	postRPC(t, s.Handler(), "totally-wrong",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)

	got, err := ReadAudit(root)
	if err != nil || len(got) != 1 {
		t.Fatalf("audit = %#v, %v; want exactly one unauthorized entry", got, err)
	}
	e := got[0]
	if e.Outcome != "unauthorized" || e.CallerID != "" {
		t.Fatalf("entry = %#v", e)
	}
	if e.CredentialFP == "" || len(e.CredentialFP) != 8 {
		t.Fatalf("credential fingerprint = %q, want 8 hex chars", e.CredentialFP)
	}
	if strings.Contains(e.CredentialFP, "totally-wrong") {
		t.Fatal("the credential itself must never be recorded")
	}
	if e.RemoteAddr == "" {
		t.Fatal("the source address must be recorded")
	}
}

// 灌爆保護：同一來源 IP 每秒最多一筆 unauthorized。
func TestUnauthorizedAuditIsRateLimitedPerSource(t *testing.T) {
	s, root := newTestA2AServer(t)
	for i := 0; i < 20; i++ {
		postRPC(t, s.Handler(), fmt.Sprintf("wrong-%d", i),
			`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	}
	got, _ := ReadAudit(root)
	if len(got) > 3 {
		t.Fatalf("wrote %d unauthorized entries for one source in one second; the log would be flooded", len(got))
	}
	if len(got) == 0 {
		t.Fatal("rate limiting must not suppress the first entry")
	}
}

func TestBadRequestsAreAudited(t *testing.T) {
	s, root := newTestA2AServer(t)
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"tasks/bogus","params":{}}`)
	got, _ := ReadAudit(root)
	if len(got) != 1 || got[0].Outcome != "bad_request" || got[0].CallerID != "peer-a" {
		t.Fatalf("audit = %#v", got)
	}
}

func TestOverlongTaskIDIsRejected(t *testing.T) {
	s, _ := newTestA2AServer(t)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","taskId":%q,"text":"hi"}}`,
		strings.Repeat("t", 200))
	rec := postRPC(t, s.Handler(), "secret-1", body)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCInvalidParams {
		t.Fatalf("an unbounded taskId lets a caller stash a ~1 MiB blob in the task store; got %#v", resp.Error)
	}
}

// round 11 review, Important 2, reproduced end-to-end: an APPROVED caller
// (secret-1/peer-a — no unauthorized/bearer trickery needed) sending a
// message/send whose contextId fails the alphanumeric/length check reaches
// auditBadRequest with the raw, unbounded p.ContextID. Before the fix, three
// such requests (900 KB illegal contextId each) produced a 2.7 MB
// a2a-audit.jsonl — enough that well under a hundred requests would rotate
// the 32 MiB cap and overwrite the .1 generation too, destroying the entire
// audit history. The body stays under the handler's 1 MiB io.LimitReader
// cap so it reaches auditBadRequest rather than failing to parse.
func TestBadRequestAuditBoundsCallerControlledContextID(t *testing.T) {
	s, root := newTestA2AServer(t)
	huge := strings.Repeat("x", 900_000)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":%q,"text":"hi"}}`, huge)
	for i := 0; i < 3; i++ {
		postRPC(t, s.Handler(), "secret-1", body)
	}
	info, err := os.Stat(AuditPath(root))
	if err != nil {
		t.Fatalf("stat audit log: %v", err)
	}
	if info.Size() > 10_000 {
		t.Fatalf("audit log grew to %d bytes for 3 requests carrying a 900KB caller-controlled contextId; ContextID must be bounded before it reaches the log", info.Size())
	}
	got, err := ReadAudit(root)
	if err != nil || len(got) != 3 {
		t.Fatalf("ReadAudit = %#v, %v", got, err)
	}
	for _, e := range got {
		if n := len([]rune(e.ContextID)); n > maxAuditFieldRunes+8 {
			t.Fatalf("stored ContextID kept %d runes", n)
		}
	}
}

// Same probe against the unknown-agent branch, whose Summary embeds
// "unknown agent "+p.Agent (already bounded via Summary truncation) but
// whose AuditEntry.Agent field previously carried the raw, unbounded value
// separately.
func TestBadRequestAuditBoundsCallerControlledAgent(t *testing.T) {
	s, root := newTestA2AServer(t)
	huge := strings.Repeat("y", 900_000)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":%q,"contextId":"c1","text":"hi"}}`, huge)
	postRPC(t, s.Handler(), "secret-1", body)

	got, err := ReadAudit(root)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadAudit = %#v, %v", got, err)
	}
	if n := len([]rune(got[0].Agent)); n > maxAuditFieldRunes+8 {
		t.Fatalf("stored Agent kept %d runes", n)
	}
}

func TestTasksGetReturnsTheCallersOwnTask(t *testing.T) {
	s, root := newTestA2AServer(t)
	var tasks TaskStore
	tasks.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "codereview", CallerID: "peer-a",
		Session: "aa-codereview-c1", Branch: "aa/aa-codereview-c1", State: TaskCompleted,
		StartedAt: "2026-08-06T00:00:00Z", CompletedAt: "2026-08-06T00:10:00Z",
		// DetailSafe:true — 這一列在模擬 a2a_result.go 收下的沙盒回覆（唯一
		// 真正安全的來源），不是 markFailed 包住的 err.Error()。
		Detail: "all good", DetailSafe: true,
	})
	_ = SaveTasks(root, tasks)

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"contextId":"c1"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %#v", resp.Error)
	}
	res, _ := resp.Result.(map[string]any)
	if res["state"] != "completed" || res["detail"] != "all good" ||
		res["branch"] != "aa/aa-codereview-c1" || res["taskId"] != "t1" {
		t.Fatalf("result = %#v", res)
	}
	// session / worktree 路徑是私有專案資訊，不得出現在回應裡。
	if _, leaked := res["session"]; leaked {
		t.Fatal("tasks/get must not expose the sandbox session name")
	}
	if _, leaked := res["worktree"]; leaked {
		t.Fatal("tasks/get must not expose the worktree path")
	}
}

// 不洩漏存在性：別人的 contextId 與不存在的 contextId 回完全相同的錯誤。
//
// round-11-review Minor：只比 Code/Message 是一個很弱的柵欄——一個永遠回
// errTaskNotVisible、完全不查 store 的假處理器也會通過它。改成比對整個回
// 應 body（兩個請求的 id 相同，錯誤回應完全不含 contextId，所以正確實作下
// body 必須逐位元組相同）、HTTP 狀態碼，並確認兩條路徑都沒有寫入 audit
// log（handleTasksGet 目前只在格式錯誤時才呼叫 auditBadRequest，擁有權判
// 定分支完全靜默——如果有人不小心讓其中一條分支多寫一筆 audit，就是另一個
// 可觀察的側管道，這裡也要接住）。
func TestTasksGetHidesOtherCallersTasks(t *testing.T) {
	s, root := newTestA2AServer(t)
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "codereview", CallerID: "someone-else", State: TaskWorking})
	_ = SaveTasks(root, tasks)

	callers, _ := LoadCallers(root)
	_ = callers.Register("peer-b", "secret-2")
	callers.Approve("peer-b", []string{"read"})
	callers.SetGrantLevel("peer-b", GrantReadOnly)
	_ = SaveCallers(root, callers)

	mine := postRPC(t, s.Handler(), "secret-2", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"contextId":"c1"}}`)
	ghost := postRPC(t, s.Handler(), "secret-2", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"contextId":"nosuch"}}`)

	var a, b RPCResponse
	_ = json.Unmarshal(mine.Body.Bytes(), &a)
	_ = json.Unmarshal(ghost.Body.Bytes(), &b)
	if a.Error == nil || b.Error == nil {
		t.Fatalf("both must error: %#v / %#v", a.Error, b.Error)
	}
	if a.Error.Code != b.Error.Code || a.Error.Message != b.Error.Message {
		t.Fatalf("existence leaked: %#v vs %#v", a.Error, b.Error)
	}
	if mine.Code != ghost.Code {
		t.Fatalf("HTTP status differs: %d vs %d", mine.Code, ghost.Code)
	}
	if mine.Body.String() != ghost.Body.String() {
		t.Fatalf("response bodies differ:\n mine=%q\nghost=%q", mine.Body.String(), ghost.Body.String())
	}
	entries, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ownership/not-found queries must not write to the audit log, got %d entries: %#v", len(entries), entries)
	}
}

// detail 是沙盒自撰文字，回應中截斷至 64 KiB。這是對「沙盒文字不流出 HTTP」
// 的刻意放寬 —— 沒有它就沒有交付（規格第六節開放問題 8）。
//
// round-11-review Minor：原本的 fixture 經過 tasks.Upsert，寫入時就已經把
// Detail 截到 maxDetailBytes（a2a_tasks.go Upsert），所以就算
// taskSnapshotPayload 完全不截斷、直接回傳 t.Detail 原文，這個測試一樣會
// 通過——它從來沒有真的測到 taskSnapshotPayload 自己的防禦性截斷。改成繞
// 過 Upsert，直接組一個 TaskStore 寫進 tasks.json，讓 store 上的 Detail
// 本身就超過 maxDetailBytes（模擬「未來某條路徑繞過 Upsert 直接改了
// Detail」的情境），這樣如果拿掉 taskSnapshotPayload 裡的截斷，這個測試會
// 真的失敗。
func TestTasksGetTruncatesDetail(t *testing.T) {
	s, root := newTestA2AServer(t)
	tasks := TaskStore{Tasks: []A2ATask{{
		ContextID: "c1", TaskID: "t1", Agent: "codereview", CallerID: "peer-a",
		State: TaskCompleted,
		// DetailSafe:true——這是唯一要驗證的是截斷防線，不是 redaction 防
		// 線；若留成預設 false，回應會被 detailWithheldMessage 取代，這個
		// 測試就測不到截斷邏輯了。
		Detail: strings.Repeat("x", 3*maxDetailBytes), DetailSafe: true,
	}}}
	_ = SaveTasks(root, tasks)

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"contextId":"c1"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	res, _ := resp.Result.(map[string]any)
	if got := len(res["detail"].(string)); got > maxDetailBytes+32 {
		t.Fatalf("detail = %d bytes, want at most %d", got, maxDetailBytes)
	}
}

// round-11-review Critical：Detail 不只是沙盒自撰文字——markFailed／派送
// 失敗這幾條路徑把 err.Error() 包進同一個欄位，可能夾帶絕對路徑（例如
// "ensure worktree: /home/x/project/.channel-agent/sandboxes/...: git
// checkout failed"）。這個測試模擬那種情況（DetailSafe 留預設值 false，
// 就像 markFailed 對這類錯誤實際寫入的那樣），斷言 tasks/get 絕不把它原文
// 交給遠端呼叫方；state 仍然要正確回報，operator 端的完整原文不受影響
// （這個測試只檢查回應，不檢查 store）。
func TestTasksGetRedactsUnsafeDetail(t *testing.T) {
	s, root := newTestA2AServer(t)
	hostErr := "ensure worktree: /home/conray/private-project/.channel-agent/sandboxes/aa-codereview-c1: git checkout failed"
	tasks := TaskStore{Tasks: []A2ATask{{
		ContextID: "c1", TaskID: "t1", Agent: "codereview", CallerID: "peer-a",
		State: TaskFailed, Detail: hostErr, // DetailSafe: false（零值，未標記）
	}}}
	_ = SaveTasks(root, tasks)

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"contextId":"c1"}}`)
	if strings.Contains(rec.Body.String(), "private-project") || strings.Contains(rec.Body.String(), "git checkout failed") {
		t.Fatalf("unsafe Detail leaked into response body: %s", rec.Body.String())
	}
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %#v", resp.Error)
	}
	res, _ := resp.Result.(map[string]any)
	if res["state"] != "failed" {
		t.Fatalf("state = %#v, want failed", res["state"])
	}
	detail, _ := res["detail"].(string)
	if detail == "" || strings.Contains(detail, hostErr) {
		t.Fatalf("detail = %q, want a generic message with no host path", detail)
	}
}
