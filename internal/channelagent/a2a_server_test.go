package channelagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
func TestHandlerAndDrainQueueNeverDoubleDispatch(t *testing.T) {
	s, root := newTestA2AServer(t)
	fake := &FakeSessionManager{}
	ex := NewSandboxExecutor(root, fake)
	s.Executor = ex

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		postRPC(t, s.Handler(), "secret-1",
			`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"go"}}`)
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, _ = DrainQueue(context.Background(), root, ex)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	if n := len(fake.Injected); n != 1 {
		t.Fatalf("injected %d prompts for one contextId; the delegated work would run %d times", n, n)
	}
}

// 規格第五節測試 2：N 條 goroutine 送 N 個不同 contextId，併發上限必須是硬
// 上限而不是建議值。
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

	if got := len(fake.Started); got > MaxConcurrentSandboxes {
		t.Fatalf("started %d sandboxes, cap is %d", got, MaxConcurrentSandboxes)
	}
}
