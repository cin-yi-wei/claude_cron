# A2A Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let external agents delegate tasks to this machine over Google's A2A protocol, executed in per-contextId isolated sandboxes.

**Architecture:** Three new JSON stores (`agents.json`, `callers.json`, `tasks.json`) live beside the existing `bindings.json` but are never mixed with it. An A2A HTTP server runs inside the `serve` process on its **own port**, advertises opt-in `aa-<agent>` identities via an Agent Card, and dispatches each incoming task into an `aa-<agent>-<ctx>` tmux session with its own git worktree. Session start/stop sits behind a `SessionManager` interface so tests never spawn tmux.

**Tech Stack:** Go 1.26, module `claude_cron`, package `channelagent` (`internal/channelagent/`), stdlib `net/http` + `encoding/json`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-05-a2a-integration-design.md` (commit d82ed54)

## Global Constraints

- **Never modify existing `cc-` machinery**: `bindings.json`, `registry.go`, `supervisor.go`, `reap.go` behaviour must be unchanged. New code goes in new `a2a_*.go` files.
- **New files live in `internal/channelagent/`** (not a new package) so they can reuse unexported helpers `pathIn`, `sanitize`, `countJSON`. "Independent" in the spec means independent *state and loop*, not a separate Go package.
- **Tests must never start a real tmux session or a real `claude` process.** Session control is only reachable through the `SessionManager` interface.
- **A2A listener port MUST differ from the admin API port** (admin defaults to `127.0.0.1:8787` and can create shell-capable bindings).
- **Reuse existing helpers**: `ReadJSON(path string, v any) error`, `AtomicWriteJSON(path string, v any) error`, `pathIn(root string, parts ...string) string`.
- **No `cc-` binding is ever exposed** in the Agent Card.
- Concurrency cap: **8**. Soft timeout: **30 min**. Hard timeout: **2 h**. Post-completion retention: **10 min**.
- **No automatic retry** of failed tasks.
- Test style follows `internal/channelagent/permission_test.go`: `t.TempDir()`, table-driven where natural.

---

## File Structure

| File | Responsibility |
|---|---|
| `a2a_agents.go` | `Agent` type, `agents.json` load/save/CRUD |
| `a2a_callers.go` | `Caller` type, `callers.json`, approval + capability grants |
| `a2a_tasks.go` | `A2ATask` type, state machine, `tasks.json` |
| `a2a_card.go` | Agent Card generation from enabled agents |
| `a2a_rpc.go` | JSON-RPC 2.0 envelope parse/encode, error codes |
| `a2a_server.go` | HTTP handler, auth, routing, wiring |
| `a2a_session.go` | `SessionManager` interface + real tmux implementation |
| `a2a_executor.go` | Worktree + session + inject + result detection |
| `a2a_lifecycle.go` | Timeout sweeps, concurrency cap, queue drain |
| `a2a_audit.go` | Append-only audit log |
| `a2a_*_test.go` | One test file per source file above |

---

# Phase 1 — Data Layer

No network, no sessions. Pure data.

### Task 1: Agent store

**Files:**
- Create: `internal/channelagent/a2a_agents.go`
- Test: `internal/channelagent/a2a_agents_test.go`

**Interfaces:**
- Consumes: `ReadJSON`, `AtomicWriteJSON`, `pathIn` (existing, `fileutil.go` / `init.go`)
- Produces: `Agent` struct; `AgentsPath(root string) string`; `LoadAgents(root string) (AgentStore, error)`; `SaveAgents(root string, s AgentStore) error`; `(*AgentStore).Get(name string) (Agent, bool)`; `(*AgentStore).Add(a Agent) error`; `(*AgentStore).Remove(name string) bool`; `(AgentStore).Enabled() []Agent`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"path/filepath"
	"testing"
)

func TestAgentStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents on empty root: %v", err)
	}
	if len(s.Agents) != 0 {
		t.Fatalf("empty root should give 0 agents, got %d", len(s.Agents))
	}

	if err := s.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Description: "reviews code", Capabilities: []string{"read"}, Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, s); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	a, ok := got.Get("codereview")
	if !ok {
		t.Fatal("agent missing after reload")
	}
	if a.ProjectDir != "/p/x" || !a.Enabled || len(a.Capabilities) != 1 {
		t.Fatalf("round-trip lost data: %#v", a)
	}
}

func TestAgentStoreRejectsDuplicateAndBadName(t *testing.T) {
	var s AgentStore
	if err := s.Add(Agent{Name: "dup"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := s.Add(Agent{Name: "dup"}); err == nil {
		t.Fatal("duplicate name must be rejected")
	}
	if err := s.Add(Agent{Name: "Bad Name"}); err == nil {
		t.Fatal("invalid name must be rejected")
	}
}

func TestAgentsPathIsNotBindingsJSON(t *testing.T) {
	root := t.TempDir()
	if got := AgentsPath(root); got != filepath.Join(root, "agents.json") {
		t.Fatalf("AgentsPath = %q", got)
	}
	if AgentsPath(root) == RegistryPath(root) {
		t.Fatal("agents store must not collide with bindings.json")
	}
}

func TestAgentStoreEnabledFiltersDisabled(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "on", Enabled: true},
		{Name: "off", Enabled: false},
	}}
	got := s.Enabled()
	if len(got) != 1 || got[0].Name != "on" {
		t.Fatalf("Enabled() = %#v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestAgent -v`
Expected: FAIL — `undefined: LoadAgents`, `undefined: Agent`, etc.

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Agent is an A2A-exposed identity: aa-<Name>. Unlike a Binding it has no
// channel and never executes work itself — tasks run in per-contextId
// aa-<Name>-<ctx> instances.
type Agent struct {
	Name         string   `json:"name"`
	ProjectDir   string   `json:"project_dir"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Enabled      bool     `json:"enabled"`
}

type AgentStore struct {
	Agents []Agent `json:"agents"`
}

// a2aNameRe mirrors the binding name rule: lowercase letters, digits, dashes.
var a2aNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func AgentsPath(root string) string { return filepath.Join(root, "agents.json") }

func LoadAgents(root string) (AgentStore, error) {
	var s AgentStore
	if err := ReadJSON(AgentsPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AgentStore{}, nil
		}
		return AgentStore{}, err
	}
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestAgent -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_agents.go internal/channelagent/a2a_agents_test.go
git commit -m "feat(a2a): agent store"
```

---

### Task 2: Caller store

**Files:**
- Create: `internal/channelagent/a2a_callers.go`
- Test: `internal/channelagent/a2a_callers_test.go`

**Interfaces:**
- Consumes: `ReadJSON`, `AtomicWriteJSON`
- Produces: `Caller` struct; `CallerStatus` constants `CallerPending`/`CallerApproved`/`CallerRevoked`; `CallersPath(root string) string`; `LoadCallers(root string) (CallerStore, error)`; `SaveCallers(root string, s CallerStore) error`; `(*CallerStore).Register(id, credential string) error`; `(*CallerStore).Approve(id string, caps []string) bool`; `(*CallerStore).Revoke(id string) bool`; `(CallerStore).Authenticate(credential string) (Caller, bool)`; `(Caller).Allows(capability string) bool`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import "testing"

func TestCallerRegisterStartsPending(t *testing.T) {
	var s CallerStore
	if err := s.Register("peer-a", "secret-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c, ok := s.Authenticate("secret-1")
	if ok {
		t.Fatalf("pending caller must not authenticate, got %#v", c)
	}
}

func TestCallerApproveThenAuthenticate(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	if !s.Approve("peer-a", []string{"read", "write"}) {
		t.Fatal("Approve returned false")
	}
	c, ok := s.Authenticate("secret-1")
	if !ok {
		t.Fatal("approved caller should authenticate")
	}
	if c.CallerID != "peer-a" {
		t.Fatalf("CallerID = %q", c.CallerID)
	}
	if !c.Allows("read") || !c.Allows("write") {
		t.Fatalf("granted caps missing: %#v", c.GrantedCapabilities)
	}
	if c.Allows("admin") {
		t.Fatal("ungranted capability must be denied")
	}
}

func TestCallerRevokeBlocksAuthentication(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	s.Approve("peer-a", []string{"read"})
	if !s.Revoke("peer-a") {
		t.Fatal("Revoke returned false")
	}
	if _, ok := s.Authenticate("secret-1"); ok {
		t.Fatal("revoked caller must not authenticate")
	}
}

func TestCallerAuthenticateRejectsUnknownCredential(t *testing.T) {
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	s.Approve("peer-a", []string{"read"})
	if _, ok := s.Authenticate("wrong"); ok {
		t.Fatal("unknown credential must not authenticate")
	}
	if _, ok := s.Authenticate(""); ok {
		t.Fatal("empty credential must not authenticate")
	}
}

func TestCallerStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	var s CallerStore
	_ = s.Register("peer-a", "secret-1")
	s.Approve("peer-a", []string{"read"})
	if err := SaveCallers(root, s); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}
	got, err := LoadCallers(root)
	if err != nil {
		t.Fatalf("LoadCallers: %v", err)
	}
	if _, ok := got.Authenticate("secret-1"); !ok {
		t.Fatal("approved caller lost across round-trip")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestCaller -v`
Expected: FAIL — `undefined: CallerStore`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type CallerStatus string

const (
	CallerPending  CallerStatus = "pending"
	CallerApproved CallerStatus = "approved"
	CallerRevoked  CallerStatus = "revoked"
)

// Caller is a registered external A2A peer. Registration is open, but a caller
// stays Pending — and cannot authenticate — until a human approves it.
type Caller struct {
	CallerID            string       `json:"caller_id"`
	Credential          string       `json:"credential"`
	Status              CallerStatus `json:"status"`
	GrantedCapabilities []string     `json:"granted_capabilities"`
}

type CallerStore struct {
	Callers []Caller `json:"callers"`
}

func CallersPath(root string) string { return filepath.Join(root, "callers.json") }

func LoadCallers(root string) (CallerStore, error) {
	var s CallerStore
	if err := ReadJSON(CallersPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CallerStore{}, nil
		}
		return CallerStore{}, err
	}
	return s, nil
}

func SaveCallers(root string, s CallerStore) error {
	return AtomicWriteJSON(CallersPath(root), s)
}

func (s *CallerStore) Register(id, credential string) error {
	if id == "" || credential == "" {
		return fmt.Errorf("caller id and credential are required")
	}
	for _, c := range s.Callers {
		if c.CallerID == id {
			return fmt.Errorf("caller %q already registered", id)
		}
	}
	s.Callers = append(s.Callers, Caller{CallerID: id, Credential: credential, Status: CallerPending})
	return nil
}

func (s *CallerStore) Approve(id string, caps []string) bool {
	for i := range s.Callers {
		if s.Callers[i].CallerID == id {
			s.Callers[i].Status = CallerApproved
			s.Callers[i].GrantedCapabilities = caps
			return true
		}
	}
	return false
}

func (s *CallerStore) Revoke(id string) bool {
	for i := range s.Callers {
		if s.Callers[i].CallerID == id {
			s.Callers[i].Status = CallerRevoked
			return true
		}
	}
	return false
}

// Authenticate resolves a credential to an approved caller. Pending and revoked
// callers never authenticate. Comparison is constant-time.
func (s CallerStore) Authenticate(credential string) (Caller, bool) {
	if credential == "" {
		return Caller{}, false
	}
	for _, c := range s.Callers {
		if c.Status != CallerApproved {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c.Credential), []byte(credential)) == 1 {
			return c, true
		}
	}
	return Caller{}, false
}

// Allows reports whether this caller was granted a capability. The grant list is
// the whole policy: there is no runtime prompt.
func (c Caller) Allows(capability string) bool {
	for _, g := range c.GrantedCapabilities {
		if g == capability {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestCaller -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_callers.go internal/channelagent/a2a_callers_test.go
git commit -m "feat(a2a): caller store with approval gate"
```

---

### Task 3: Task store and state machine

**Files:**
- Create: `internal/channelagent/a2a_tasks.go`
- Test: `internal/channelagent/a2a_tasks_test.go`

**Interfaces:**
- Consumes: `ReadJSON`, `AtomicWriteJSON`, `sanitize`
- Produces: `A2ATask` struct; `TaskState` constants `TaskSubmitted`/`TaskWorking`/`TaskCompleted`/`TaskFailed`/`TaskCanceled`; `TasksPath(root string) string`; `LoadTasks(root string) (TaskStore, error)`; `SaveTasks(root string, s TaskStore) error`; `(*TaskStore).Upsert(t A2ATask)`; `(*TaskStore).ByContext(contextID string) (A2ATask, bool)`; `(TaskStore).ActiveCount() int`; `CanTransition(from, to TaskState) bool`; `SessionNameFor(agent, contextID string) string`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"strings"
	"testing"
)

func TestSessionNameForIsPrefixedAndSanitised(t *testing.T) {
	got := SessionNameFor("codereview", "ctx/with:weird chars")
	if !strings.HasPrefix(got, "aa-codereview-") {
		t.Fatalf("session name must start with aa-<agent>-, got %q", got)
	}
	if strings.ContainsAny(got, "/: ") {
		t.Fatalf("session name not sanitised: %q", got)
	}
	if strings.HasPrefix(got, "cc-") {
		t.Fatal("A2A sessions must never use the cc- prefix")
	}
}

func TestCanTransitionAllowsForwardOnly(t *testing.T) {
	ok := [][2]TaskState{
		{TaskSubmitted, TaskWorking},
		{TaskWorking, TaskCompleted},
		{TaskWorking, TaskFailed},
		{TaskWorking, TaskCanceled},
		{TaskSubmitted, TaskFailed},
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("CanTransition(%s,%s) = false, want true", c[0], c[1])
		}
	}
	bad := [][2]TaskState{
		{TaskCompleted, TaskWorking},
		{TaskFailed, TaskCompleted},
		{TaskCanceled, TaskWorking},
		{TaskCompleted, TaskCompleted},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("CanTransition(%s,%s) = true, want false", c[0], c[1])
		}
	}
}

func TestTaskStoreUpsertReplacesByContext(t *testing.T) {
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", State: TaskSubmitted})
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", State: TaskWorking})
	if len(s.Tasks) != 1 {
		t.Fatalf("Upsert must replace, got %d tasks", len(s.Tasks))
	}
	got, ok := s.ByContext("c1")
	if !ok || got.State != TaskWorking {
		t.Fatalf("ByContext = %#v, %v", got, ok)
	}
}

func TestActiveCountIgnoresTerminalStates(t *testing.T) {
	s := TaskStore{Tasks: []A2ATask{
		{ContextID: "a", State: TaskSubmitted},
		{ContextID: "b", State: TaskWorking},
		{ContextID: "c", State: TaskCompleted},
		{ContextID: "d", State: TaskFailed},
		{ContextID: "e", State: TaskCanceled},
	}}
	if got := s.ActiveCount(); got != 2 {
		t.Fatalf("ActiveCount = %d, want 2", got)
	}
}

func TestTaskStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	s.Upsert(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "codereview", State: TaskWorking, StartedAt: "2026-08-05T00:00:00Z"})
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	got, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	tk, ok := got.ByContext("c1")
	if !ok || tk.Agent != "codereview" {
		t.Fatalf("round-trip lost data: %#v", tk)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestSessionNameFor|TestCanTransition|TestTaskStore|TestActiveCount' -v`
Expected: FAIL — `undefined: SessionNameFor`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"errors"
	"os"
	"path/filepath"
)

type TaskState string

const (
	TaskSubmitted TaskState = "submitted"
	TaskWorking   TaskState = "working"
	TaskCompleted TaskState = "completed"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

// A2ATask is one delegated task, keyed by the A2A contextId. Its sandbox is a
// dedicated tmux session + git worktree.
type A2ATask struct {
	ContextID   string    `json:"context_id"`
	TaskID      string    `json:"task_id"`
	Agent       string    `json:"agent"`
	CallerID    string    `json:"caller_id"`
	Session     string    `json:"session"`
	Worktree    string    `json:"worktree"`
	Branch      string    `json:"branch"`
	State       TaskState `json:"state"`
	StartedAt   string    `json:"started_at"`
	CompletedAt string    `json:"completed_at,omitempty"`
	// Prompt is the caller's original request text. It must be persisted so a
	// task queued at capacity can still be started later by DrainQueue.
	Prompt string `json:"prompt,omitempty"`
	// Detail carries the outcome: the sandbox's reply on success, or the error
	// reason on failure. Never the input — that is Prompt.
	Detail string `json:"detail,omitempty"`
}

type TaskStore struct {
	Tasks []A2ATask `json:"tasks"`
}

func TasksPath(root string) string { return filepath.Join(root, "tasks.json") }

func LoadTasks(root string) (TaskStore, error) {
	var s TaskStore
	if err := ReadJSON(TasksPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TaskStore{}, nil
		}
		return TaskStore{}, err
	}
	return s, nil
}

func SaveTasks(root string, s TaskStore) error {
	return AtomicWriteJSON(TasksPath(root), s)
}

func (s *TaskStore) Upsert(t A2ATask) {
	for i := range s.Tasks {
		if s.Tasks[i].ContextID == t.ContextID {
			s.Tasks[i] = t
			return
		}
	}
	s.Tasks = append(s.Tasks, t)
}

func (s *TaskStore) ByContext(contextID string) (A2ATask, bool) {
	for _, t := range s.Tasks {
		if t.ContextID == contextID {
			return t, true
		}
	}
	return A2ATask{}, false
}

// ActiveCount counts tasks occupying a sandbox slot (submitted or working).
func (s TaskStore) ActiveCount() int {
	n := 0
	for _, t := range s.Tasks {
		if t.State == TaskSubmitted || t.State == TaskWorking {
			n++
		}
	}
	return n
}

// CanTransition enforces the state machine: terminal states are final.
func CanTransition(from, to TaskState) bool {
	switch from {
	case TaskSubmitted:
		return to == TaskWorking || to == TaskFailed || to == TaskCanceled
	case TaskWorking:
		return to == TaskCompleted || to == TaskFailed || to == TaskCanceled
	default:
		return false
	}
}

// SessionNameFor builds the sandbox session name. Never collides with cc-.
func SessionNameFor(agent, contextID string) string {
	return "aa-" + agent + "-" + sanitize(contextID)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestSessionNameFor|TestCanTransition|TestTaskStore|TestActiveCount' -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Verify existing suite still green, then commit**

```bash
cd /home/conray/project/claude_cron
go test ./... 
git add internal/channelagent/a2a_tasks.go internal/channelagent/a2a_tasks_test.go
git commit -m "feat(a2a): task store and state machine"
```

---

# Phase 2 — A2A Protocol Layer

HTTP server with a **stub executor**. At the end of this phase a real A2A client can discover agents and submit tasks, receiving a canned result. Protocol bugs surface before any session machinery exists.

### Task 4: Agent Card

**Files:**
- Create: `internal/channelagent/a2a_card.go`
- Test: `internal/channelagent/a2a_card_test.go`

**Interfaces:**
- Consumes: `AgentStore`, `(AgentStore).Enabled` (Task 1)
- Produces: `AgentCard` struct; `AgentCardSkill` struct; `BuildAgentCard(baseURL string, s AgentStore) AgentCard`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestBuildAgentCard -v`
Expected: FAIL — `undefined: BuildAgentCard`

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestBuildAgentCard -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_card.go internal/channelagent/a2a_card_test.go
git commit -m "feat(a2a): agent card with opt-in exposure"
```

---

### Task 5: JSON-RPC envelope

**Files:**
- Create: `internal/channelagent/a2a_rpc.go`
- Test: `internal/channelagent/a2a_rpc_test.go`

**Interfaces:**
- Produces: `RPCRequest`, `RPCResponse`, `RPCError` structs; error-code constants `RPCParseError` (-32700), `RPCInvalidRequest` (-32600), `RPCMethodNotFound` (-32601), `RPCInvalidParams` (-32602), `RPCInternalError` (-32603), `RPCUnauthorized` (-32001), `RPCForbidden` (-32002), `RPCCapacityFull` (-32003); `ParseRPC(body []byte) (RPCRequest, *RPCError)`; `RPCOK(id any, result any) RPCResponse`; `RPCFail(id any, code int, msg string) RPCResponse`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"encoding/json"
	"testing"
)

func TestParseRPCAcceptsValidRequest(t *testing.T) {
	req, rerr := ParseRPC([]byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"x":1}}`))
	if rerr != nil {
		t.Fatalf("unexpected error: %#v", rerr)
	}
	if req.Method != "message/send" {
		t.Fatalf("Method = %q", req.Method)
	}
}

func TestParseRPCRejectsMalformedJSON(t *testing.T) {
	_, rerr := ParseRPC([]byte(`{not json`))
	if rerr == nil || rerr.Code != RPCParseError {
		t.Fatalf("want parse error, got %#v", rerr)
	}
}

func TestParseRPCRejectsWrongVersionAndMissingMethod(t *testing.T) {
	_, rerr := ParseRPC([]byte(`{"jsonrpc":"1.0","id":1,"method":"m"}`))
	if rerr == nil || rerr.Code != RPCInvalidRequest {
		t.Fatalf("want invalid request for bad version, got %#v", rerr)
	}
	_, rerr = ParseRPC([]byte(`{"jsonrpc":"2.0","id":1}`))
	if rerr == nil || rerr.Code != RPCInvalidRequest {
		t.Fatalf("want invalid request for missing method, got %#v", rerr)
	}
}

func TestRPCOKAndFailShape(t *testing.T) {
	ok := RPCOK(7, map[string]string{"a": "b"})
	blob, _ := json.Marshal(ok)
	var decoded map[string]any
	_ = json.Unmarshal(blob, &decoded)
	if decoded["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v", decoded["jsonrpc"])
	}
	if _, hasErr := decoded["error"]; hasErr {
		t.Fatal("success response must not carry error")
	}

	bad := RPCFail(7, RPCForbidden, "nope")
	blob, _ = json.Marshal(bad)
	decoded = map[string]any{}
	_ = json.Unmarshal(blob, &decoded)
	if _, hasResult := decoded["result"]; hasResult {
		t.Fatal("error response must not carry result")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestParseRPC -v`
Expected: FAIL — `undefined: ParseRPC`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import "encoding/json"

const (
	RPCParseError     = -32700
	RPCInvalidRequest = -32600
	RPCMethodNotFound = -32601
	RPCInvalidParams  = -32602
	RPCInternalError  = -32603
	// Application-defined range.
	RPCUnauthorized = -32001
	RPCForbidden    = -32002
	RPCCapacityFull = -32003
)

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

func ParseRPC(body []byte) (RPCRequest, *RPCError) {
	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return RPCRequest{}, &RPCError{Code: RPCParseError, Message: "malformed JSON"}
	}
	if req.JSONRPC != "2.0" {
		return RPCRequest{}, &RPCError{Code: RPCInvalidRequest, Message: "jsonrpc must be \"2.0\""}
	}
	if req.Method == "" {
		return RPCRequest{}, &RPCError{Code: RPCInvalidRequest, Message: "method is required"}
	}
	return req, nil
}

func RPCOK(id any, result any) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func RPCFail(id any, code int, msg string) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestParseRPC|TestRPCOK' -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_rpc.go internal/channelagent/a2a_rpc_test.go
git commit -m "feat(a2a): json-rpc envelope"
```

---

### Task 6: HTTP server with stub executor

**Files:**
- Create: `internal/channelagent/a2a_server.go`
- Test: `internal/channelagent/a2a_server_test.go`

**Interfaces:**
- Consumes: `LoadAgents`, `LoadCallers`, `LoadTasks`, `SaveTasks`, `BuildAgentCard`, `ParseRPC`, `RPCOK`, `RPCFail`, `SessionNameFor`, `A2ATask`, `TaskSubmitted` (Tasks 1-5)
- Produces: `TaskExecutor` interface with `Start(ctx context.Context, task A2ATask, prompt string) error`; `StubExecutor` struct; `A2AServer` struct with fields `Root string`, `BaseURL string`, `Executor TaskExecutor`; `(*A2AServer).Handler() http.Handler`; `MessageSendParams` struct

- [ ] **Step 1: Write the failing test**

```go
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
	s, _ := newTestA2AServer(t)
	rec := postRPC(t, s.Handler(), "", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCUnauthorized {
		t.Fatalf("want unauthorized, got %#v", resp.Error)
	}
}

func TestUngrantedCapabilityForbidden(t *testing.T) {
	s, root := newTestA2AServer(t)
	agents, _ := LoadAgents(root)
	agents.Agents[0].Capabilities = []string{"write"} // caller only granted "read"
	_ = SaveAgents(root, agents)

	rec := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != RPCForbidden {
		t.Fatalf("want forbidden, got %#v", resp.Error)
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestAgentCardServed|TestUnauthenticated|TestUngranted|TestUnknownAgent|TestValidTask|TestUnknownMethod' -v`
Expected: FAIL — `undefined: A2AServer`

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestAgentCardServed|TestUnauthenticated|TestUngranted|TestUnknownAgent|TestValidTask|TestUnknownMethod' -v`
Expected: PASS (6 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
go test ./...
git add internal/channelagent/a2a_server.go internal/channelagent/a2a_server_test.go
git commit -m "feat(a2a): http server, auth, and stub dispatch"
```

---

# Phase 3 — Execution Layer

Replace the stub with real sandbox execution.

### Task 7: SessionManager interface

**Files:**
- Create: `internal/channelagent/a2a_session.go`
- Test: `internal/channelagent/a2a_session_test.go`

**Interfaces:**
- Consumes: `EnsureWorktree(ctx context.Context, projectDir, branch, worktreePath string) error`, `StartTmuxClaude(ctx context.Context, session, cwd, registryRoot string) error`, `StopTmuxSession(ctx context.Context, session string) error`, `IngestMessages(ctx context.Context, root string, messages []SourceMessage) (int, error)`, `SourceMessage` (all existing)
- Produces: `SessionManager` interface with `EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error`, `Start(ctx context.Context, session, cwd, registryRoot string) error`, `Stop(ctx context.Context, session string) error`, `Inject(ctx context.Context, root string, msg SourceMessage) error`; `TmuxSessionManager` struct; `FakeSessionManager` struct with fields `Started []string`, `Stopped []string`, `Injected []SourceMessage`, `Workspaces []string`, `FailOn string`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"context"
	"testing"
)

func TestFakeSessionManagerRecordsCalls(t *testing.T) {
	var m SessionManager = &FakeSessionManager{}
	f := m.(*FakeSessionManager)
	ctx := context.Background()

	if err := m.EnsureWorkspace(ctx, "/p/x", "br", "/w/x"); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := m.Start(ctx, "aa-x-c1", "/w/x", "/root"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Inject(ctx, "/root", SourceMessage{Content: "go"}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := m.Stop(ctx, "aa-x-c1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(f.Workspaces) != 1 || f.Workspaces[0] != "/w/x" {
		t.Fatalf("Workspaces = %#v", f.Workspaces)
	}
	if len(f.Started) != 1 || f.Started[0] != "aa-x-c1" {
		t.Fatalf("Started = %#v", f.Started)
	}
	if len(f.Injected) != 1 || f.Injected[0].Content != "go" {
		t.Fatalf("Injected = %#v", f.Injected)
	}
	if len(f.Stopped) != 1 {
		t.Fatalf("Stopped = %#v", f.Stopped)
	}
}

func TestFakeSessionManagerCanFail(t *testing.T) {
	f := &FakeSessionManager{FailOn: "start"}
	if err := f.Start(context.Background(), "aa-x-c1", "/w/x", "/root"); err == nil {
		t.Fatal("expected Start to fail when FailOn=start")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestFakeSessionManager -v`
Expected: FAIL — `undefined: SessionManager`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"context"
	"errors"
)

// SessionManager isolates every side effect that touches git or tmux, so tests
// can substitute FakeSessionManager and never spawn a real claude session.
type SessionManager interface {
	EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error
	Start(ctx context.Context, session, cwd, registryRoot string) error
	Stop(ctx context.Context, session string) error
	Inject(ctx context.Context, root string, msg SourceMessage) error
}

// TmuxSessionManager is the production implementation, delegating to the same
// helpers the cc- supervisor uses.
type TmuxSessionManager struct{}

func (TmuxSessionManager) EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error {
	return EnsureWorktree(ctx, projectDir, branch, worktree)
}

func (TmuxSessionManager) Start(ctx context.Context, session, cwd, registryRoot string) error {
	return StartTmuxClaude(ctx, session, cwd, registryRoot)
}

func (TmuxSessionManager) Stop(ctx context.Context, session string) error {
	return StopTmuxSession(ctx, session)
}

func (TmuxSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	_, err := IngestMessages(ctx, root, []SourceMessage{msg})
	return err
}

// FakeSessionManager records calls for assertions. FailOn makes one method
// return an error: "workspace", "start", "stop", or "inject".
type FakeSessionManager struct {
	Workspaces []string
	Started    []string
	Stopped    []string
	Injected   []SourceMessage
	FailOn     string
}

func (f *FakeSessionManager) EnsureWorkspace(_ context.Context, _, _, worktree string) error {
	if f.FailOn == "workspace" {
		return errors.New("fake workspace failure")
	}
	f.Workspaces = append(f.Workspaces, worktree)
	return nil
}

func (f *FakeSessionManager) Start(_ context.Context, session, _, _ string) error {
	if f.FailOn == "start" {
		return errors.New("fake start failure")
	}
	f.Started = append(f.Started, session)
	return nil
}

func (f *FakeSessionManager) Stop(_ context.Context, session string) error {
	if f.FailOn == "stop" {
		return errors.New("fake stop failure")
	}
	f.Stopped = append(f.Stopped, session)
	return nil
}

func (f *FakeSessionManager) Inject(_ context.Context, _ string, msg SourceMessage) error {
	if f.FailOn == "inject" {
		return errors.New("fake inject failure")
	}
	f.Injected = append(f.Injected, msg)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestFakeSessionManager -v`
Expected: PASS (2 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_session.go internal/channelagent/a2a_session_test.go
git commit -m "feat(a2a): session manager interface with test fake"
```

---

### Task 8: Real executor

**Files:**
- Create: `internal/channelagent/a2a_executor.go`
- Test: `internal/channelagent/a2a_executor_test.go`

**Interfaces:**
- Consumes: `SessionManager`, `FakeSessionManager` (Task 7); `A2ATask`, `LoadTasks`, `SaveTasks`, `CanTransition`, `TaskWorking` (Task 3); `LoadAgents` (Task 1); `Init`, `pathIn`, `countJSON`
- Produces: `SandboxExecutor` struct with fields `Root string`, `Sessions SessionManager`; `NewSandboxExecutor(root string, sm SessionManager) *SandboxExecutor`; `(*SandboxExecutor).Start(ctx context.Context, task A2ATask, prompt string) error`; `SandboxRoot(root, session string) string`; `SandboxWorktree(projectDir, session string) string`; `BranchFor(session string) string`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"context"
	"strings"
	"testing"
)

func newExecutorFixture(t *testing.T) (string, *FakeSessionManager, *SandboxExecutor) {
	t.Helper()
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	fake := &FakeSessionManager{}
	return root, fake, NewSandboxExecutor(root, fake)
}

func TestSandboxExecutorCreatesWorkspaceStartsAndInjects(t *testing.T) {
	root, fake, ex := newExecutorFixture(t)
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted}

	if err := ex.Start(context.Background(), task, "please review"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(fake.Workspaces) != 1 {
		t.Fatalf("workspace not created: %#v", fake.Workspaces)
	}
	if len(fake.Started) != 1 || fake.Started[0] != task.Session {
		t.Fatalf("session not started: %#v", fake.Started)
	}
	if len(fake.Injected) != 1 || fake.Injected[0].Content != "please review" {
		t.Fatalf("prompt not injected: %#v", fake.Injected)
	}

	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext("c1")
	if !ok || got.State != TaskWorking {
		t.Fatalf("task should be working, got %#v", got)
	}
	if got.Worktree == "" || got.Branch == "" {
		t.Fatalf("worktree/branch not recorded: %#v", got)
	}
}

func TestSandboxExecutorUsesAAPrefixedIsolatedPaths(t *testing.T) {
	_, _, _ = newExecutorFixture(t)
	session := SessionNameFor("codereview", "c1")
	wt := SandboxWorktree("/p/x", session)
	if !strings.Contains(wt, session) {
		t.Fatalf("worktree %q must be unique per session", wt)
	}
	if strings.HasSuffix(wt, "/x") {
		t.Fatal("sandbox must not reuse the agent project dir itself")
	}
	if br := BranchFor(session); !strings.HasPrefix(br, "aa/") {
		t.Fatalf("branch %q should be namespaced under aa/", br)
	}
}

func TestSandboxExecutorMarksFailedWhenStartFails(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)
	ex := NewSandboxExecutor(root, &FakeSessionManager{FailOn: "start"})

	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted}
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error when session start fails")
	}
	tasks, _ := LoadTasks(root)
	got, _ := tasks.ByContext("c1")
	if got.State != TaskFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

func TestSandboxExecutorRejectsUnknownAgent(t *testing.T) {
	root, _, ex := newExecutorFixture(t)
	_ = root
	task := A2ATask{ContextID: "c1", Agent: "ghost", Session: "aa-ghost-c1", State: TaskSubmitted}
	if err := ex.Start(context.Background(), task, "x"); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSandboxExecutor -v`
Expected: FAIL — `undefined: NewSandboxExecutor`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// SandboxExecutor runs a delegated task in its own worktree + tmux session.
type SandboxExecutor struct {
	Root     string
	Sessions SessionManager
}

func NewSandboxExecutor(root string, sm SessionManager) *SandboxExecutor {
	return &SandboxExecutor{Root: root, Sessions: sm}
}

// SandboxRoot is the per-sandbox state dir (inbox/outbox/locks), separate from
// any binding root.
func SandboxRoot(root, session string) string {
	return filepath.Join(root, "sandboxes", session)
}

// SandboxWorktree places the sandbox checkout beside the project dir, named for
// the session so two contexts never share files.
func SandboxWorktree(projectDir, session string) string {
	if abs, err := filepath.Abs(projectDir); err == nil {
		projectDir = abs
	}
	return filepath.Join(filepath.Dir(projectDir), session)
}

// BranchFor namespaces sandbox branches so they are obvious in git output.
func BranchFor(session string) string { return "aa/" + session }

func (e *SandboxExecutor) markFailed(task A2ATask, detail string) {
	tasks, err := LoadTasks(e.Root)
	if err != nil {
		return
	}
	if cur, ok := tasks.ByContext(task.ContextID); ok {
		task = cur
	}
	task.State = TaskFailed
	task.Detail = detail
	task.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	tasks.Upsert(task)
	_ = SaveTasks(e.Root, tasks)
}

func (e *SandboxExecutor) Start(ctx context.Context, task A2ATask, prompt string) error {
	agents, err := LoadAgents(e.Root)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	agent, ok := agents.Get(task.Agent)
	if !ok {
		err := fmt.Errorf("unknown agent %q", task.Agent)
		e.markFailed(task, err.Error())
		return err
	}

	task.Worktree = SandboxWorktree(agent.ProjectDir, task.Session)
	task.Branch = BranchFor(task.Session)
	sandboxRoot := SandboxRoot(e.Root, task.Session)

	if err := Init(sandboxRoot); err != nil {
		e.markFailed(task, "init sandbox root: "+err.Error())
		return err
	}
	if err := e.Sessions.EnsureWorkspace(ctx, agent.ProjectDir, task.Branch, task.Worktree); err != nil {
		e.markFailed(task, "ensure worktree: "+err.Error())
		return err
	}
	if err := e.Sessions.Start(ctx, task.Session, task.Worktree, sandboxRoot); err != nil {
		e.markFailed(task, "start session: "+err.Error())
		return err
	}

	msg := SourceMessage{
		Platform:  "a2a",
		ChannelID: task.ContextID,
		MessageID: task.Session + "-" + task.ContextID,
		AuthorID:  task.CallerID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Content:   prompt,
	}
	if err := e.Sessions.Inject(ctx, sandboxRoot, msg); err != nil {
		e.markFailed(task, "inject: "+err.Error())
		return err
	}

	tasks, err := LoadTasks(e.Root)
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}
	if cur, ok := tasks.ByContext(task.ContextID); ok && !CanTransition(cur.State, TaskWorking) {
		return fmt.Errorf("cannot move task %s from %s to working", task.ContextID, cur.State)
	}
	task.State = TaskWorking
	tasks.Upsert(task)
	return SaveTasks(e.Root, tasks)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSandboxExecutor -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
go test ./...
git add internal/channelagent/a2a_executor.go internal/channelagent/a2a_executor_test.go
git commit -m "feat(a2a): sandbox executor"
```

---

### Task 9: Result detection

**Files:**
- Create: `internal/channelagent/a2a_result.go`
- Test: `internal/channelagent/a2a_result_test.go`

**Interfaces:**
- Consumes: `SandboxRoot` (Task 8); `LoadTasks`, `SaveTasks`, `A2ATask`, `TaskWorking`, `TaskCompleted`, `CanTransition` (Task 3); `pathIn`, `ReadJSON`, `OutputJob` (existing `types.go`)
- Produces: `CollectResults(root string, now time.Time) (int, error)`; `ResultFor(root string, task A2ATask) (string, bool)`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSandboxResult(t *testing.T, root, session, text string) {
	t.Helper()
	dir := pathIn(SandboxRoot(root, session), "outbox", "pending")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	job := OutputJob{Schema: 1, JobID: "r1", Send: true, Text: text}
	if err := AtomicWriteJSON(filepath.Join(dir, "r1.json"), job); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func TestCollectResultsCompletesTaskWhenResultAppears(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskWorking})
	if err := SaveTasks(root, tasks); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	writeSandboxResult(t, root, session, "done reviewing")

	n, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	if n != 1 {
		t.Fatalf("collected = %d, want 1", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCompleted {
		t.Fatalf("state = %s, want completed", tk.State)
	}
	if tk.Detail != "done reviewing" {
		t.Fatalf("detail = %q", tk.Detail)
	}
	if tk.CompletedAt == "" {
		t.Fatal("CompletedAt not set")
	}
}

func TestCollectResultsIgnoresTaskWithoutResult(t *testing.T) {
	root := t.TempDir()
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskWorking})
	_ = SaveTasks(root, tasks)

	n, err := CollectResults(root, time.Now())
	if err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	if n != 0 {
		t.Fatalf("collected = %d, want 0", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state changed to %s without a result", tk.State)
	}
}

func TestCollectResultsSkipsTerminalTasks(t *testing.T) {
	root := t.TempDir()
	session := SessionNameFor("codereview", "c1")
	var tasks TaskStore
	tasks.Upsert(A2ATask{ContextID: "c1", Agent: "codereview", Session: session, State: TaskCompleted, Detail: "original"})
	_ = SaveTasks(root, tasks)
	writeSandboxResult(t, root, session, "late result")

	if _, err := CollectResults(root, time.Now()); err != nil {
		t.Fatalf("CollectResults: %v", err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.Detail != "original" {
		t.Fatalf("terminal task was overwritten: %q", tk.Detail)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestCollectResults -v`
Expected: FAIL — `undefined: CollectResults`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResultFor reports the sandbox's reply text, if it has written one. Completion
// is detected by the same outbox-file convention every worker already uses —
// never by scraping the tmux pane.
func ResultFor(root string, task A2ATask) (string, bool) {
	dir := pathIn(SandboxRoot(root, task.Session), "outbox", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var job OutputJob
		if err := ReadJSON(filepath.Join(dir, e.Name()), &job); err != nil {
			continue
		}
		return job.Text, true
	}
	return "", false
}

// CollectResults promotes working tasks to completed when their sandbox has
// produced a result. Returns how many tasks were completed.
func CollectResults(root string, now time.Time) (int, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range tasks.Tasks {
		t := tasks.Tasks[i]
		if !CanTransition(t.State, TaskCompleted) {
			continue
		}
		text, ok := ResultFor(root, t)
		if !ok {
			continue
		}
		t.State = TaskCompleted
		t.Detail = text
		t.CompletedAt = now.UTC().Format(time.RFC3339)
		tasks.Tasks[i] = t
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, SaveTasks(root, tasks)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestCollectResults -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
go test ./...
git add internal/channelagent/a2a_result.go internal/channelagent/a2a_result_test.go
git commit -m "feat(a2a): result detection via outbox convention"
```

---

# Phase 4 — Lifecycle

### Task 10: Concurrency cap and queueing

**Files:**
- Modify: `internal/channelagent/a2a_server.go` (add capacity check before `Executor.Start`)
- Create: `internal/channelagent/a2a_lifecycle.go`
- Test: `internal/channelagent/a2a_lifecycle_test.go`

**Interfaces:**
- Consumes: `TaskStore.ActiveCount`, `LoadTasks`, `SaveTasks`, `A2ATask`, `TaskSubmitted`, `TaskWorking` (Task 3); `TaskExecutor` (Task 6)
- Produces: `MaxConcurrentSandboxes` constant (8); `HasCapacity(s TaskStore) bool`; `DrainQueue(ctx context.Context, root string, ex TaskExecutor) (int, error)`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"context"
	"testing"
)

func TestHasCapacityRespectsCap(t *testing.T) {
	var s TaskStore
	for i := 0; i < MaxConcurrentSandboxes; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), State: TaskWorking})
	}
	if HasCapacity(s) {
		t.Fatalf("should be full at %d active", MaxConcurrentSandboxes)
	}
	s.Tasks[0].State = TaskCompleted
	if !HasCapacity(s) {
		t.Fatal("completing a task should free a slot")
	}
}

func TestMaxConcurrentSandboxesIsEight(t *testing.T) {
	if MaxConcurrentSandboxes != 8 {
		t.Fatalf("MaxConcurrentSandboxes = %d, want 8", MaxConcurrentSandboxes)
	}
}

func TestDrainQueueStartsSubmittedTasksUpToCap(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	// One already working, plus three queued.
	s.Upsert(A2ATask{ContextID: "live", Agent: "a", State: TaskWorking})
	for _, id := range []string{"q1", "q2", "q3"} {
		s.Upsert(A2ATask{ContextID: id, Agent: "a", State: TaskSubmitted, Prompt: "work " + id})
	}
	if err := SaveTasks(root, s); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	stub := &StubExecutor{}
	n, err := DrainQueue(context.Background(), root, stub)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if n != 3 {
		t.Fatalf("started = %d, want 3", n)
	}
	if stub.Calls != 3 {
		t.Fatalf("executor calls = %d", stub.Calls)
	}
	if stub.LastPrompt == "" {
		t.Fatal("queued task started with an empty prompt — Prompt was not persisted")
	}
}

func TestDrainQueueStopsAtCapacity(t *testing.T) {
	root := t.TempDir()
	var s TaskStore
	for i := 0; i < MaxConcurrentSandboxes; i++ {
		s.Upsert(A2ATask{ContextID: string(rune('a' + i)), Agent: "a", State: TaskWorking})
	}
	s.Upsert(A2ATask{ContextID: "queued", Agent: "a", State: TaskSubmitted})
	_ = SaveTasks(root, s)

	stub := &StubExecutor{}
	n, err := DrainQueue(context.Background(), root, stub)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if n != 0 || stub.Calls != 0 {
		t.Fatalf("must not start work when full: started=%d calls=%d", n, stub.Calls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestHasCapacity|TestMaxConcurrent|TestDrainQueue' -v`
Expected: FAIL — `undefined: MaxConcurrentSandboxes`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import "context"

// MaxConcurrentSandboxes caps simultaneous aa-*-<ctx> instances. Industry
// guidance for parallel agent worktrees is 8-10; 8 is the conservative end and
// also bounds memory, which has run tight on this host.
const MaxConcurrentSandboxes = 8

func HasCapacity(s TaskStore) bool {
	return s.ActiveCount() < MaxConcurrentSandboxes
}

// DrainQueue starts queued (submitted) tasks while slots remain. Overflow stays
// queued rather than being rejected.
func DrainQueue(ctx context.Context, root string, ex TaskExecutor) (int, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, t := range tasks.Tasks {
		if t.State != TaskSubmitted {
			continue
		}
		cur, err := LoadTasks(root)
		if err != nil {
			return started, err
		}
		if !HasCapacity(cur) {
			break
		}
		if err := ex.Start(ctx, t, t.Prompt); err != nil {
			continue // executor already recorded the failure
		}
		started++
	}
	return started, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestHasCapacity|TestMaxConcurrent|TestDrainQueue' -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Wire the cap into the server**

In `internal/channelagent/a2a_server.go`, immediately before the `s.Executor.Start(...)` call added in Task 6, insert:

```go
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
```

- [ ] **Step 6: Run full suite and commit**

```bash
cd /home/conray/project/claude_cron
go test ./...
git add internal/channelagent/a2a_lifecycle.go internal/channelagent/a2a_lifecycle_test.go internal/channelagent/a2a_server.go
git commit -m "feat(a2a): concurrency cap and queue drain"
```

---

### Task 11: Timeout sweeps and sandbox reclamation

**Files:**
- Modify: `internal/channelagent/a2a_lifecycle.go`
- Test: `internal/channelagent/a2a_lifecycle_test.go` (append)

**Interfaces:**
- Consumes: `SessionManager` (Task 7); `LoadTasks`, `SaveTasks`, `TaskWorking`, `TaskCanceled`, `TaskCompleted` (Task 3); `SandboxRoot`, `SandboxWorktree` (Task 8)
- Produces: `SoftTimeout`, `HardTimeout`, `RetainAfterComplete` duration constants; `SweepTimeouts(ctx context.Context, root string, sm SessionManager, now time.Time) (canceled int, reclaimed int, err error)`

- [ ] **Step 1: Write the failing test (append to a2a_lifecycle_test.go)**

```go
func TestSweepTimeoutsConstants(t *testing.T) {
	if SoftTimeout != 30*time.Minute {
		t.Fatalf("SoftTimeout = %v, want 30m", SoftTimeout)
	}
	if HardTimeout != 2*time.Hour {
		t.Fatalf("HardTimeout = %v, want 2h", HardTimeout)
	}
	if RetainAfterComplete != 10*time.Minute {
		t.Fatalf("RetainAfterComplete = %v, want 10m", RetainAfterComplete)
	}
}

func TestSweepDoesNotCancelBeforeHardTimeout(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		StartedAt: now.Add(-45 * time.Minute).Format(time.RFC3339), // past soft, before hard
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 0 {
		t.Fatalf("canceled = %d, want 0 (soft timeout must not kill)", canceled)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskWorking {
		t.Fatalf("state = %s, want still working", tk.State)
	}
}

func TestSweepCancelsAfterHardTimeout(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskWorking,
		StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	canceled, _, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("session not stopped: %#v", fake.Stopped)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.State != TaskCanceled {
		t.Fatalf("state = %s, want canceled", tk.State)
	}
}

func TestSweepReclaimsCompletedAfterRetention(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskCompleted,
		CompletedAt: now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	s.Upsert(A2ATask{
		ContextID: "c2", Session: "aa-a-c2", State: TaskCompleted,
		CompletedAt: now.Add(-2 * time.Minute).Format(time.RFC3339), // still in retention
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	_, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now)
	if err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != "aa-a-c1" {
		t.Fatalf("wrong session reclaimed: %#v", fake.Stopped)
	}
}

func TestSweepLeavesFailedSandboxForensics(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskFailed,
		CompletedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now); err != nil || reclaimed != 0 {
		t.Fatalf("failed sandboxes must be kept: reclaimed=%d err=%v", reclaimed, err)
	}
	if len(fake.Stopped) != 0 {
		t.Fatalf("failed sandbox must not be torn down: %#v", fake.Stopped)
	}
}
```

Also add `"time"` to the test file's imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSweep -v`
Expected: FAIL — `undefined: SweepTimeouts`

- [ ] **Step 3: Write minimal implementation (append to a2a_lifecycle.go)**

```go
const (
	// SoftTimeout only flips reporting: A2A natively supports long-running
	// tasks, and real agent work routinely exceeds half an hour.
	SoftTimeout = 30 * time.Minute
	// HardTimeout is the backstop against a wedged sandbox holding a worktree
	// and memory forever.
	HardTimeout = 2 * time.Hour
	// RetainAfterComplete keeps the sandbox alive briefly so the caller can ask
	// a follow-up in the same contextId without paying to rebuild it.
	RetainAfterComplete = 10 * time.Minute
)

func parseRFC3339(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// SweepTimeouts cancels tasks past HardTimeout and tears down completed
// sandboxes past RetainAfterComplete. Failed sandboxes are deliberately left in
// place for forensics.
func SweepTimeouts(ctx context.Context, root string, sm SessionManager, now time.Time) (int, int, error) {
	tasks, err := LoadTasks(root)
	if err != nil {
		return 0, 0, err
	}
	canceled, reclaimed, changed := 0, 0, false

	for i := range tasks.Tasks {
		t := tasks.Tasks[i]
		switch t.State {
		case TaskWorking, TaskSubmitted:
			started, ok := parseRFC3339(t.StartedAt)
			if !ok || now.Sub(started) < HardTimeout {
				continue
			}
			_ = sm.Stop(ctx, t.Session)
			t.State = TaskCanceled
			t.Detail = "hard timeout exceeded"
			t.CompletedAt = now.UTC().Format(time.RFC3339)
			tasks.Tasks[i] = t
			canceled++
			changed = true
		case TaskCompleted:
			done, ok := parseRFC3339(t.CompletedAt)
			if !ok || now.Sub(done) < RetainAfterComplete {
				continue
			}
			if t.Session == "" {
				continue
			}
			_ = sm.Stop(ctx, t.Session)
			t.Session = "" // mark reclaimed; branch is kept
			tasks.Tasks[i] = t
			reclaimed++
			changed = true
		}
	}

	if !changed {
		return canceled, reclaimed, nil
	}
	return canceled, reclaimed, SaveTasks(root, tasks)
}
```

Add `"time"` to the file's imports.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestSweep|TestHasCapacity|TestDrainQueue' -v`
Expected: PASS (9 tests)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
go test ./...
git add internal/channelagent/a2a_lifecycle.go internal/channelagent/a2a_lifecycle_test.go
git commit -m "feat(a2a): timeout sweeps and sandbox reclamation"
```

---

### Task 12: Audit log

**Files:**
- Create: `internal/channelagent/a2a_audit.go`
- Test: `internal/channelagent/a2a_audit_test.go`
- Modify: `internal/channelagent/a2a_server.go` (record every accepted/rejected delegation)

**Interfaces:**
- Consumes: `pathIn`; `A2ATask` (Task 3)
- Produces: `AuditEntry` struct; `AuditPath(root string) string`; `AppendAudit(root string, e AuditEntry) error`; `ReadAudit(root string) ([]AuditEntry, error)`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"testing"
	"time"
)

func TestAppendAuditIsAppendOnly(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, e := range []AuditEntry{
		{At: now, CallerID: "peer-a", Agent: "codereview", ContextID: "c1", Summary: "review x", Outcome: "accepted"},
		{At: now, CallerID: "peer-b", Agent: "codereview", ContextID: "c2", Summary: "review y", Outcome: "forbidden"},
	} {
		if err := AppendAudit(root, e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	got, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].ContextID != "c1" || got[1].Outcome != "forbidden" {
		t.Fatalf("audit order or content wrong: %#v", got)
	}
}

func TestReadAuditOnEmptyRootIsEmptyNotError(t *testing.T) {
	got, err := ReadAudit(t.TempDir())
	if err != nil {
		t.Fatalf("ReadAudit on empty root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %d, want 0", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestAppendAudit -v`
Expected: FAIL — `undefined: AuditEntry`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// AuditEntry is one delegation event. The log is append-only JSONL: an
// externally reachable system needs a durable record of who asked for what.
type AuditEntry struct {
	At        string `json:"at"`
	CallerID  string `json:"caller_id"`
	Agent     string `json:"agent"`
	ContextID string `json:"context_id"`
	TaskID    string `json:"task_id,omitempty"`
	Summary   string `json:"summary"`
	Outcome   string `json:"outcome"`
	Branch    string `json:"branch,omitempty"`
}

func AuditPath(root string) string { return filepath.Join(root, "a2a-audit.jsonl") }

func AppendAudit(root string, e AuditEntry) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(AuditPath(root), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	blob, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(blob, '\n'))
	return err
}

func ReadAudit(root string) ([]AuditEntry, error) {
	f, err := os.Open(AuditPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestAppendAudit|TestReadAudit' -v`
Expected: PASS (2 tests)

- [ ] **Step 5: Wire audit into the server**

In `internal/channelagent/a2a_server.go`, add an audit call at each terminal branch of `handleRPC` after the caller is known. Immediately after the successful `SaveTasks` and dispatch, add:

```go
	_ = AppendAudit(s.Root, AuditEntry{
		At:        time.Now().UTC().Format(time.RFC3339),
		CallerID:  caller.CallerID,
		Agent:     agent.Name,
		ContextID: task.ContextID,
		TaskID:    task.TaskID,
		Summary:   p.Text,
		Outcome:   "accepted",
	})
```

And in the capability-denied branch, before `writeRPC(... RPCForbidden ...)`:

```go
			_ = AppendAudit(s.Root, AuditEntry{
				At:        time.Now().UTC().Format(time.RFC3339),
				CallerID:  caller.CallerID,
				Agent:     p.Agent,
				ContextID: p.ContextID,
				Summary:   p.Text,
				Outcome:   "forbidden",
			})
```

- [ ] **Step 6: Add a server test asserting the audit trail**

```go
func TestServerWritesAuditOnAcceptAndDeny(t *testing.T) {
	s, root := newTestA2AServer(t)
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"ok"}}`)

	agents, _ := LoadAgents(root)
	agents.Agents[0].Capabilities = []string{"write"}
	_ = SaveAgents(root, agents)
	postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"agent":"codereview","contextId":"c2","text":"denied"}}`)

	entries, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(entries))
	}
	if entries[0].Outcome != "accepted" || entries[1].Outcome != "forbidden" {
		t.Fatalf("outcomes = %q, %q", entries[0].Outcome, entries[1].Outcome)
	}
}
```

- [ ] **Step 7: Run full suite and commit**

```bash
cd /home/conray/project/claude_cron
go test ./...
go vet ./internal/channelagent/
git add internal/channelagent/a2a_audit.go internal/channelagent/a2a_audit_test.go internal/channelagent/a2a_server.go internal/channelagent/a2a_server_test.go
git commit -m "feat(a2a): append-only delegation audit log"
```

---

### Task 13: Serve wiring on a separate port

**Files:**
- Modify: `internal/channelagent/config.go` (add A2A settings)
- Modify: `cmd/claude-cron/main.go` (start the A2A listener alongside serve)
- Test: `internal/channelagent/a2a_config_test.go`

**Interfaces:**
- Consumes: `Config` (existing `config.go`); `A2AServer`, `NewSandboxExecutor`, `TmuxSessionManager`
- Produces: `Config.A2A` field of type `A2AConfig` with `Listen string`, `BaseURL string`, `Enabled bool`; `(Config).A2AListen() string`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import "testing"

func TestA2AListenDefaultDiffersFromAdminPort(t *testing.T) {
	var c Config
	got := c.A2AListen()
	if got == "" {
		t.Fatal("A2AListen must have a default")
	}
	if got == "127.0.0.1:8787" {
		t.Fatal("A2A listener must not share the admin API port")
	}
}

func TestA2AListenHonoursConfig(t *testing.T) {
	c := Config{A2A: A2AConfig{Listen: "127.0.0.1:9999"}}
	if got := c.A2AListen(); got != "127.0.0.1:9999" {
		t.Fatalf("A2AListen = %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestA2AListen -v`
Expected: FAIL — `unknown field A2A in struct literal`

- [ ] **Step 3: Write minimal implementation**

Add to `internal/channelagent/config.go`:

```go
// A2AConfig configures the agent-to-agent listener. Listen MUST differ from the
// admin API address: the admin API can create shell-capable bindings and must
// never become externally reachable.
type A2AConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// A2AListen resolves the A2A listen address, defaulting to a port distinct from
// the admin default (127.0.0.1:8787).
func (c Config) A2AListen() string {
	if c.A2A.Listen != "" {
		return c.A2A.Listen
	}
	return "127.0.0.1:8790"
}
```

Add the field to the existing `Config` struct:

```go
	A2A A2AConfig `json:"a2a,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestA2AListen -v`
Expected: PASS (2 tests)

- [ ] **Step 5: Start the listener from serve**

In `cmd/claude-cron/main.go`, inside the `serve` subcommand after the admin server is started, add:

```go
		if cfg.A2A.Enabled {
			a2a := &agent.A2AServer{
				Root:     absRoot,
				BaseURL:  cfg.A2A.BaseURL,
				Executor: agent.NewSandboxExecutor(absRoot, agent.TmuxSessionManager{}),
			}
			a2aSrv := &http.Server{Addr: cfg.A2AListen(), Handler: a2a.Handler()}
			go func() {
				if err := a2aSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(stderr, "a2a server: %v\n", err)
				}
			}()
		}
```

Use the same variable names for root and config already present in that function; if they differ, adapt rather than introducing new ones.

- [ ] **Step 6: Add the sweep and drain to the serve cycle**

In the serve loop body (the same place `RunSchedulerOnce` is called), add:

```go
		if cfg.A2A.Enabled {
			if _, err := agent.CollectResults(absRoot, time.Now()); err != nil {
				fmt.Fprintf(stdout, "a2a collect: %v\n", err)
			}
			if _, _, err := agent.SweepTimeouts(ctx, absRoot, agent.TmuxSessionManager{}, time.Now()); err != nil {
				fmt.Fprintf(stdout, "a2a sweep: %v\n", err)
			}
			if _, err := agent.DrainQueue(ctx, absRoot, agent.NewSandboxExecutor(absRoot, agent.TmuxSessionManager{})); err != nil {
				fmt.Fprintf(stdout, "a2a drain: %v\n", err)
			}
		}
```

- [ ] **Step 7: Build, run full suite, commit**

```bash
cd /home/conray/project/claude_cron
go build ./...
go test ./...
go vet ./internal/channelagent/
git add internal/channelagent/config.go internal/channelagent/a2a_config_test.go cmd/claude-cron/main.go
git commit -m "feat(a2a): wire listener and lifecycle into serve"
```

---

## Self-Review Notes

Checked against the spec:

| Spec requirement | Task |
|---|---|
| `agents.json` schema + CRUD | 1 |
| `callers.json`, approval gate, capability grants | 2 |
| `tasks.json`, state machine | 3 |
| Agent Card, opt-in exposure, no binding leakage | 4 |
| JSON-RPC 2.0 envelope + error codes | 5 |
| HTTP server, auth, capability enforcement, stub dispatch | 6 |
| SessionManager interface, no tmux in tests | 7 |
| Worktree per contextId, session start, inject | 8 |
| Completion via outbox convention | 9 |
| Concurrency cap 8, queueing | 10 |
| 30 min soft / 2 h hard / 10 min retention, failed-sandbox retention | 11 |
| Append-only audit log | 12 |
| Separate port from admin API | 13 |
| No `cc-` machinery modified | All — only new `a2a_*.go` files plus additive changes to `config.go` and `main.go` |
| No auto-retry | Enforced by omission: nothing re-queues a failed task |

**Known gaps deliberately left to a follow-up plan** (recorded in the spec as out of scope): container-level isolation for shared databases, and admin UI screens for agent/caller management. The stores and audit log expose everything a UI would need.
