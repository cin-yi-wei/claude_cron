# A2A Sandbox Driver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make delegated A2A tasks actually execute — deliver the prompt into the sandbox, clear the dialogs that block it, stream its output to an agent channel, and close the six defects the whole-branch review found.

**Architecture:** Sandboxes reuse the existing root-generic `RunWorkerOnce` rather than a new worker; one goroutine per sandbox keeps them off the cron scheduler's goroutine. A package mutex closes the `tasks.json` lost-update. Each `aa-<agent>` identity gets one output-only Discord channel via the existing activity mirror.

**Tech Stack:** Go 1.26, module `claude_cron`, package `channelagent`. Standard library only.

**Spec:** `docs/superpowers/specs/2026-08-06-a2a-sandbox-driver-design.md` (commit a5a07f9)
**Predecessor:** `docs/superpowers/specs/2026-08-05-a2a-integration-design.md`, implemented in commits 6daacee..7906b50

## Global Constraints

- **Never modify `cc-` machinery**: `bindings.json`, `registry.go`, `supervisor.go`, `reap.go` behaviour must be unchanged. The `cc-` confirm watchdog in particular keeps asking the user; only `aa-` auto-answers.
- **The agent channel is output-only.** It must never be registered in any poll or push ingest path. Ingesting it would let anyone who can type in Discord drive a sandbox, bypassing A2A authentication and capability grants entirely. Treat any change that makes it readable as a security defect.
- **Tests must never start a tmux session or a `claude` process.** Use `FakeSessionManager` and fake injectors.
- **A test asserting "message delivered" must assert delivery, not that a staging call happened.** That exact gap let the missing-delivery defect pass thirteen reviews.
- All A2A behaviour stays gated behind `cfg.A2A.Enabled` (default false); with it off, `serve` must be byte-for-byte unchanged.
- Reuse existing helpers: `RunWorkerOnce`, `TmuxInjector`, `SandboxRoot`, `SandboxWorktree`, `LoadTasks`/`SaveTasks`, `pathIn`, `AtomicWriteJSON`, `moveFile`.

---

## File Structure

| File | Responsibility |
|---|---|
| `a2a_store.go` (new) | The `tasks.json` mutex and guarded load/save helpers |
| `a2a_driver.go` (new) | Per-sandbox goroutine: runs `RunWorkerOnce`, answers built-in dialogs |
| `a2a_trust.go` (new) | Pre-seeding folder trust so the dialog does not fire |
| `a2a_channel.go` (new) | Agent channel resolution + output-only activity wiring |
| `a2a_executor.go` (edit) | Unique `MessageID` per injected message (I1) |
| `a2a_server.go` (edit) | Ownership check regardless of state (I2); dispatch context (I3) |
| `a2a_lifecycle.go` (edit) | Worktree reclamation + failed-sandbox retention cap (I5) |
| `a2a_agents.go` (edit) | `ChannelID` on `Agent` |
| `cmd/claude-cron/main.go` (edit) | A2A work off the cron goroutine (I4); driver lifecycle |

---

### Task 1: Serialize tasks.json access (C2)

**Files:**
- Create: `internal/channelagent/a2a_store.go`
- Test: `internal/channelagent/a2a_store_test.go`

**Interfaces:**
- Consumes: `LoadTasks(root string) (TaskStore, error)`, `SaveTasks(root string, s TaskStore) error` (`a2a_tasks.go`)
- Produces: `WithTasks(root string, fn func(*TaskStore) error) error`

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"sync"
	"testing"
)

// WithTasks must serialize read-modify-write. Without it, concurrent
// increments lose updates — the exact bug that let a completed task revert to
// working after its result file had already been consumed.
func TestWithTasksSerializesConcurrentUpdates(t *testing.T) {
	root := t.TempDir()
	if err := SaveTasks(root, TaskStore{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = WithTasks(root, func(s *TaskStore) error {
				s.Upsert(A2ATask{ContextID: string(rune('A' + i%26)), State: TaskWorking})
				return nil
			})
		}(i)
	}
	wg.Wait()

	got, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(got.Tasks) != 26 {
		t.Fatalf("tasks = %d, want 26 distinct contextIds (lost update)", len(got.Tasks))
	}
}

func TestWithTasksDoesNotSaveWhenCallbackErrors(t *testing.T) {
	root := t.TempDir()
	_ = SaveTasks(root, TaskStore{Tasks: []A2ATask{{ContextID: "keep", State: TaskWorking}}})

	wantErr := errTaskAlreadyTerminal
	err := WithTasks(root, func(s *TaskStore) error {
		s.Upsert(A2ATask{ContextID: "should-not-persist", State: TaskWorking})
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	got, _ := LoadTasks(root)
	if len(got.Tasks) != 1 || got.Tasks[0].ContextID != "keep" {
		t.Fatalf("callback error must discard the mutation, got %#v", got.Tasks)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestWithTasks -race -v`
Expected: FAIL — `undefined: WithTasks`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import "sync"

// tasksMu serializes read-modify-write cycles on tasks.json. AtomicWriteJSON
// prevents torn files but not lost updates: the HTTP handler, the executor,
// CollectResults and SweepTimeouts all load, mutate and save, and after the
// listener and the lifecycle loop began running concurrently a stale snapshot
// could overwrite a completion whose result file had already been consumed —
// leaving a task that can never finish. Only `serve` writes tasks.json, so an
// in-process mutex is sufficient.
var tasksMu sync.Mutex

// WithTasks runs fn against the current task store under the lock and saves the
// result. If fn returns an error, nothing is written.
//
// Callers MUST NOT perform slow work inside fn — in particular never dispatch
// to an executor, whose session start can block for a minute or more. Holding
// this lock across that would stall every other sandbox.
func WithTasks(root string, fn func(*TaskStore) error) error {
	tasksMu.Lock()
	defer tasksMu.Unlock()

	tasks, err := LoadTasks(root)
	if err != nil {
		return err
	}
	if err := fn(&tasks); err != nil {
		return err
	}
	return SaveTasks(root, tasks)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestWithTasks -race -v`
Expected: PASS (2 tests, no race warnings)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_store.go internal/channelagent/a2a_store_test.go
git commit -m "fix(a2a): serialize tasks.json read-modify-write"
```

---

### Task 2: Route every tasks.json mutation through WithTasks (C2)

**Files:**
- Modify: `internal/channelagent/a2a_server.go`, `a2a_executor.go`, `a2a_result.go`, `a2a_lifecycle.go`
- Test: `internal/channelagent/a2a_store_test.go` (append)

**Interfaces:**
- Consumes: `WithTasks` (Task 1)
- Produces: no new exported symbols; all four call sites converted

- [ ] **Step 1: Write the failing test**

```go
// No LoadTasks/SaveTasks pair may remain outside WithTasks: an unguarded
// read-modify-write reintroduces the lost update this task exists to close.
func TestNoUnguardedTaskStoreMutations(t *testing.T) {
	files := []string{
		"a2a_server.go", "a2a_executor.go", "a2a_result.go", "a2a_lifecycle.go",
	}
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if bytes.Contains(src, []byte("SaveTasks(")) {
			t.Errorf("%s calls SaveTasks directly; route it through WithTasks", name)
		}
	}
}
```

Add `"bytes"` and `"os"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestNoUnguardedTaskStoreMutations -v`
Expected: FAIL — all four files still call `SaveTasks`

- [ ] **Step 3: Convert each call site**

In each of the four files, replace every `LoadTasks` → mutate → `SaveTasks` sequence with a `WithTasks` call. The mutation body moves inside the callback unchanged. Example shape, for `CollectResults` in `a2a_result.go`:

```go
func CollectResults(root string, now time.Time) (int, error) {
	n := 0
	err := WithTasks(root, func(tasks *TaskStore) error {
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			if !CanTransition(t.State, TaskCompleted) {
				continue
			}
			text, path, ok := pendingResultFile(root, t)
			if !ok {
				continue
			}
			t.State = TaskCompleted
			t.Detail = text
			t.CompletedAt = now.UTC().Format(time.RFC3339)
			tasks.Tasks[i] = t
			consumeResultFile(path)
			n++
		}
		return nil
	})
	return n, err
}
```

**Critical for `a2a_server.go`:** the handler currently loads, upserts and saves, then calls `Executor.Start`. Keep the dispatch OUTSIDE the `WithTasks` callback. `SandboxExecutor.Start` performs `git worktree add` and waits for the session, which can take a minute; holding the lock across it would stall every sandbox. The shape is: `WithTasks(...)` to persist the submitted task, return from it, then dispatch.

**Preserve existing behaviour:** `CollectResults` must still return 0 and write nothing when it promoted nothing; `SweepTimeouts` must still return its two counts; the executor's terminal-state guard must still refuse to revive a terminal row.

- [ ] **Step 4: Run the full suite to verify nothing regressed**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -race -v 2>&1 | tail -30`
Expected: PASS, including every pre-existing A2A test, with no race warnings

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/
git commit -m "fix(a2a): route all task-store mutations through WithTasks"
```

---

### Task 3: Pre-seed folder trust (C1 upstream)

**Files:**
- Create: `internal/channelagent/a2a_trust.go`
- Test: `internal/channelagent/a2a_trust_test.go`

**Interfaces:**
- Produces: `ClaudeConfigPath() string`; `EnsureFolderTrusted(configPath, projectDir string) error`

**Discovered mechanism — verify before relying on it.** Claude Code records trust in `~/.claude.json` under `projects[<absolute path>].hasTrustDialogAccepted`. Observation on this machine: 26 entries are `true` and every one is a **main project directory**; no git-worktree path appears. `/home/conray/project/calc` is `true` while its worktree `/home/conray/project/calc-dev` has no entry at all — yet the trust dialog *did* fire in that worktree on 2026-08-06 and was answered. The consistent reading is that Claude resolves trust to the git common root and records it there.

**Your first step is to confirm or refute that**, because the fix differs: if trust resolves to the git root, seeding the agent's `ProjectDir` once covers every sandbox cut from it; if it is per-directory, each sandbox worktree needs its own entry. Report which you found.

- [ ] **Step 1: Verify the trust mechanism and record the finding**

Run these and record the output in your report:

```bash
python3 -c "
import json
d=json.load(open('/home/conray/.claude.json'))['projects']
t=[k for k in d if d[k].get('hasTrustDialogAccepted')]
print('trusted entries:', len(t))
print('any worktree-shaped path trusted?', [k for k in t if '-jfg-' in k or k.endswith('-dev')])
"
```

If no worktree-shaped path is ever trusted, the git-root hypothesis holds. State your conclusion explicitly.

- [ ] **Step 2: Write the failing test**

```go
package channelagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readTrust(t *testing.T, path, project string) (bool, bool) {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	p, ok := cfg.Projects[project]
	if !ok {
		return false, false
	}
	v, _ := p["hasTrustDialogAccepted"].(bool)
	return v, true
}

func TestEnsureFolderTrustedAddsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(path, []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
		t.Fatalf("EnsureFolderTrusted: %v", err)
	}
	trusted, present := readTrust(t, path, "/p/x")
	if !present || !trusted {
		t.Fatalf("trust not recorded: present=%v trusted=%v", present, trusted)
	}
}

// The config file is shared with every running claude process and holds far
// more than trust. Seeding must preserve everything else byte-for-byte in
// meaning — clobbering it would break unrelated sessions.
func TestEnsureFolderTrustedPreservesOtherData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	original := `{"numStartups":42,"projects":{"/p/other":{"hasTrustDialogAccepted":true,"lastCost":1.5}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
		t.Fatalf("EnsureFolderTrusted: %v", err)
	}

	blob, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["numStartups"] != float64(42) {
		t.Fatalf("unrelated top-level key lost: %#v", got["numStartups"])
	}
	other := got["projects"].(map[string]any)["/p/other"].(map[string]any)
	if other["lastCost"] != 1.5 || other["hasTrustDialogAccepted"] != true {
		t.Fatalf("unrelated project data lost: %#v", other)
	}
}

func TestEnsureFolderTrustedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	_ = os.WriteFile(path, []byte(`{"projects":{}}`), 0o600)
	for i := 0; i < 3; i++ {
		if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	trusted, _ := readTrust(t, path, "/p/x")
	if !trusted {
		t.Fatal("trust lost across repeated calls")
	}
}

func TestEnsureFolderTrustedRejectsMissingConfig(t *testing.T) {
	err := EnsureFolderTrusted(filepath.Join(t.TempDir(), "absent.json"), "/p/x")
	if err == nil {
		t.Fatal("a missing config must error rather than create a fresh one")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestEnsureFolderTrusted -v`
Expected: FAIL — `undefined: EnsureFolderTrusted`

- [ ] **Step 4: Write minimal implementation**

```go
package channelagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeConfigPath is where Claude Code keeps per-project state, including the
// folder-trust flag.
func ClaudeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// EnsureFolderTrusted marks projectDir as trusted so a sandbox session does not
// block on the trust dialog. That dialog is not PreToolUse-gated, and a sandbox
// has no channel to ask, so without this the session would hang until the hard
// timeout.
//
// The file is decoded into a generic map and re-encoded so every unrelated key
// survives: it is shared with every running claude process and holds far more
// than trust. It is never created if absent — a missing config means something
// is wrong with the environment, and writing a stub could mask it.
func EnsureFolderTrusted(configPath, projectDir string) error {
	blob, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read claude config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return fmt.Errorf("decode claude config: %w", err)
	}

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[projectDir].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[projectDir] = entry
	}
	if trusted, _ := entry["hasTrustDialogAccepted"].(bool); trusted {
		return nil // already trusted; leave the file untouched
	}
	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, 0o600)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestEnsureFolderTrusted -v`
Expected: PASS (4 tests)

- [ ] **Step 6: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_trust.go internal/channelagent/a2a_trust_test.go
git commit -m "feat(a2a): pre-seed folder trust for sandboxes"
```

**Concurrency note for your report:** `~/.claude.json` is rewritten by every running `claude` process. A read-modify-write here can lose against one of them. Seeding happens once per sandbox creation, before its session starts, so the window is small — but record whether you consider that acceptable, and flag it if not.

---

### Task 4: Sandbox driver (C1)

**Files:**
- Create: `internal/channelagent/a2a_driver.go`
- Test: `internal/channelagent/a2a_driver_test.go`

**Interfaces:**
- Consumes: `RunWorkerOnce(ctx context.Context, root string, injector Injector, timeout time.Duration) (bool, error)` (`worker.go:70`); `TmuxInjector{Session, Root, AutoStart}` (`adapters.go:39`); `SandboxRoot` (`a2a_executor.go`); `Injector` interface
- Produces: `SandboxDriver` struct; `NewSandboxDriver(root string, timeout time.Duration) *SandboxDriver`; `(*SandboxDriver).Ensure(ctx context.Context, task A2ATask, inj Injector)`; `(*SandboxDriver).Stop(session string)`; `(*SandboxDriver).StopAll()`; `(*SandboxDriver).Running() []string`

**Why this is the whole point of the plan:** `Inject` only stages a job file in the sandbox's `inbox/pending`. Nothing drains that into the tmux pane. `RunWorkerOnce` is the code that does — and it is root-generic, taking only a root and an injector, with no reference to any registry or binding. The three existing callers pass binding roots by convention, not by requirement.

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingInjector stands in for TmuxInjector: it records what it was asked to
// deliver instead of typing into a real pane.
type recordingInjector struct {
	mu       sync.Mutex
	Injected []InputJob
}

func (r *recordingInjector) Inject(_ context.Context, job InputJob, outputPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Injected = append(r.Injected, job)
	return nil
}

func (r *recordingInjector) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Injected)
}

// The defect this plan exists to fix: a staged job must actually be delivered.
// Asserting that Inject was CALLED is not enough — that is what let the missing
// delivery slip through thirteen reviews.
func TestSandboxDriverDeliversStagedJob(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskWorking}
	sandbox := SandboxRoot(root, task.Session)
	if err := Init(sandbox); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := IngestMessages(context.Background(), sandbox, []SourceMessage{{
		Platform: "a2a", ChannelID: "c1", MessageID: "m1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Content: "review this",
	}}); err != nil {
		t.Fatalf("stage job: %v", err)
	}

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, inj)
	defer d.StopAll()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && inj.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if inj.count() == 0 {
		t.Fatal("driver never delivered the staged job to the injector")
	}
	if got := inj.Injected[0].Content; got != "review this" {
		t.Fatalf("delivered content = %q, want %q", got, "review this")
	}
}

func TestSandboxDriverIsIdempotentPerSession(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "a", Session: SessionNameFor("a", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 5; i++ {
		d.Ensure(ctx, task, &recordingInjector{})
	}
	defer d.StopAll()

	if got := d.Running(); len(got) != 1 {
		t.Fatalf("Running() = %#v, want exactly one driver for the session", got)
	}
}

func TestSandboxDriverStopEndsTheLoop(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "a", Session: SessionNameFor("a", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	d.Stop(task.Session)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(d.Running()) != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.Running(); len(got) != 0 {
		t.Fatalf("Running() = %#v after Stop, want empty", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSandboxDriver -race -v`
Expected: FAIL — `undefined: NewSandboxDriver`

- [ ] **Step 3: Write minimal implementation**

```go
package channelagent

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// SandboxDriver runs one goroutine per active sandbox, each repeatedly calling
// RunWorkerOnce against that sandbox's own root.
//
// One goroutine per sandbox rather than a shared loop: RunWorkerOnce blocks for
// the length of a job, and with up to 8 sandboxes a shared loop would serialize
// them behind each other — and if that loop were the cron scheduler's, it would
// also stall scheduling for every production binding.
type SandboxDriver struct {
	root    string
	timeout time.Duration

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func NewSandboxDriver(root string, timeout time.Duration) *SandboxDriver {
	return &SandboxDriver{root: root, timeout: timeout, running: map[string]context.CancelFunc{}}
}

// Ensure starts a driver for the task's sandbox if one is not already running.
// Safe to call every cycle: a session already being driven is left alone.
func (d *SandboxDriver) Ensure(ctx context.Context, task A2ATask, inj Injector) {
	if task.Session == "" {
		return
	}
	d.mu.Lock()
	if _, live := d.running[task.Session]; live {
		d.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	d.running[task.Session] = cancel
	d.mu.Unlock()

	go d.loop(loopCtx, task.Session, inj)
}

func (d *SandboxDriver) loop(ctx context.Context, session string, inj Injector) {
	defer func() {
		d.mu.Lock()
		delete(d.running, session)
		d.mu.Unlock()
	}()

	sandbox := SandboxRoot(d.root, session)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		processed, err := RunWorkerOnce(ctx, sandbox, inj, d.timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "a2a driver %s: %v\n", session, err)
		}
		if processed {
			continue // more may be queued; drain before idling
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// Stop ends the driver for one sandbox. Safe on an unknown session.
func (d *SandboxDriver) Stop(session string) {
	d.mu.Lock()
	cancel, ok := d.running[session]
	d.mu.Unlock()
	if ok {
		cancel()
	}
}

// StopAll ends every driver — used on serve shutdown.
func (d *SandboxDriver) StopAll() {
	d.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(d.running))
	for _, c := range d.running {
		cancels = append(cancels, c)
	}
	d.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// Running lists the sessions currently being driven.
func (d *SandboxDriver) Running() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.running))
	for s := range d.running {
		out = append(out, s)
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSandboxDriver -race -v`
Expected: PASS (3 tests, no race warnings)

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_driver.go internal/channelagent/a2a_driver_test.go
git commit -m "feat(a2a): per-sandbox driver delivering staged jobs"
```

---

### Task 5: Unique MessageID per injected message (I1)

**Files:**
- Modify: `internal/channelagent/a2a_executor.go`
- Test: `internal/channelagent/a2a_executor_test.go` (append)

**Interfaces:**
- Consumes: `IngestMessages`, `SourceMessage`
- Produces: no new exported symbols

**The defect:** the executor builds `MessageID` from the session and contextId, so it is identical for every message in a context. `IngestMessages` dedups on `platform:channel:messageID` (`watcher.go:57-60`), so a second send in the same contextId returns `created=0` with no error and is silently dropped — which breaks exactly the follow-up-within-retention behaviour the design promises.

- [ ] **Step 1: Write the failing test**

```go
func TestSandboxExecutorSecondMessageInSameContextIsDelivered(t *testing.T) {
	root, fake, ex := newExecutorFixture(t)
	_ = fake
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskSubmitted}

	if err := ex.Start(context.Background(), task, "first"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := ex.Start(context.Background(), task, "second"); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	dir := pathIn(SandboxRoot(root, task.Session), "inbox", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("inbox has %d job(s); the second message was deduped away", len(entries))
	}
}
```

Add `"os"` to the test file's imports if absent.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSandboxExecutorSecondMessage -v`
Expected: FAIL — only one job present; the second was deduped

- [ ] **Step 3: Write minimal implementation**

In `a2a_executor.go`, make the injected `MessageID` unique per message rather than per context:

```go
	msg := SourceMessage{
		Platform:  "a2a",
		ChannelID: task.ContextID,
		// Unique per message, not per context: IngestMessages dedups on
		// platform:channel:messageID, so a constant ID would silently drop
		// every follow-up in the same contextId.
		MessageID: fmt.Sprintf("%s-%d", task.Session, time.Now().UnixNano()),
		AuthorID:  task.CallerID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Content:   prompt,
	}
```

- [ ] **Step 4: Run tests to verify**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestSandboxExecutor -v`
Expected: PASS, including all pre-existing executor tests

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_executor.go internal/channelagent/a2a_executor_test.go
git commit -m "fix(a2a): unique message id so follow-ups are not deduped"
```

---

### Task 6: contextId ownership regardless of state (I2)

**Files:**
- Modify: `internal/channelagent/a2a_server.go`
- Test: `internal/channelagent/a2a_server_test.go` (append)

**The defect:** ownership is enforced only for non-terminal tasks. `SessionNameFor` and `SandboxWorktree` are caller-independent, and `EnsureWorktree` no-ops when the path already exists, so a second caller reusing a terminal contextId inherits the first caller's checkout — its uncommitted files, its branch, its sandbox root.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestTerminalContextID|TestSameCallerMayReuse' -v`
Expected: FAIL — the takeover currently succeeds

- [ ] **Step 3: Write minimal implementation**

In `a2a_server.go`, drop the non-terminal condition from the ownership check so it applies to any existing record:

```go
		// Ownership applies regardless of state. Session and worktree names are
		// caller-independent and EnsureWorktree no-ops on an existing path, so a
		// different caller reusing even a FINISHED contextId would inherit the
		// original caller's checkout, branch and sandbox root.
		if existing, ok := tasks.ByContext(p.ContextID); ok && existing.CallerID != caller.CallerID {
			// audit + RPCForbidden, as the existing hijack branch does
		}
```

Keep the existing audit entry for this branch; its outcome string does not change.

- [ ] **Step 4: Run tests to verify**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -v 2>&1 | tail -20`
Expected: PASS, including the pre-existing hijack test

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_server.go internal/channelagent/a2a_server_test.go
git commit -m "fix(a2a): enforce contextId ownership in terminal states too"
```

---

### Task 7: Detach dispatch from the request, add listener timeouts (I3)

**Files:**
- Modify: `internal/channelagent/a2a_server.go`, `cmd/claude-cron/main.go`
- Test: `internal/channelagent/a2a_server_test.go` (append)

**The defect:** dispatch runs under `r.Context()`, so a client disconnect cancels `git worktree add` or the tmux start midway, leaving a half-built worktree that the forensics rule then preserves forever. The listener also sets no timeouts, on an internet-facing server.

**Interfaces:**
- Produces: `A2AServer.DispatchContext context.Context` field (nil means `context.Background()`)

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestDispatchSurvivesClientDisconnect -v`
Expected: FAIL — `sawDone` is true

- [ ] **Step 3: Write minimal implementation**

Add the field and use it for dispatch only:

```go
type A2AServer struct {
	Root     string
	BaseURL  string
	Executor TaskExecutor
	// DispatchContext scopes sandbox creation. It must NOT be the request's
	// context: a client disconnect would cancel git worktree add or the tmux
	// start midway, leaving a half-built sandbox that the forensics rule then
	// keeps forever. Nil means context.Background().
	DispatchContext context.Context
}

func (s *A2AServer) dispatchCtx() context.Context {
	if s.DispatchContext != nil {
		return s.DispatchContext
	}
	return context.Background()
}
```

Replace `s.Executor.Start(r.Context(), ...)` with `s.Executor.Start(s.dispatchCtx(), ...)`.

In `cmd/claude-cron/main.go`, give the A2A listener timeouts:

```go
			a2aSrv := &http.Server{
				Addr:              cfg.A2AListen(),
				Handler:           a2a.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
```

- [ ] **Step 4: Run tests to verify**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./internal/channelagent/ -v 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_server.go internal/channelagent/a2a_server_test.go cmd/claude-cron/main.go
git commit -m "fix(a2a): detach dispatch from request context, add listener timeouts"
```

---

### Task 8: Reclaim worktrees, cap failed-sandbox retention (I5)

**Files:**
- Modify: `internal/channelagent/a2a_lifecycle.go`
- Test: `internal/channelagent/a2a_lifecycle_test.go` (append)

**Interfaces:**
- Consumes: `SessionManager`, `SandboxRoot`, `SandboxWorktree`, `WithTasks` (Task 1)
- Produces: `MaxRetainedFailedSandboxes` constant (value 20); `RemoveWorkspace(ctx context.Context, projectDir, worktree string) error` added to the `SessionManager` interface, implemented by `TmuxSessionManager` via the existing `RemoveWorktree`, and recorded by `FakeSessionManager` in a new `Removed []string` field

**The defects:** reclamation clears only `Session`, never the worktree or sandbox root, so ~80MB accumulates per task with no bound — and `contextId` is caller-chosen, so one approved caller can drive that indefinitely. Separately, the forensics rule keeps every failed sandbox forever, which is itself an unbounded growth path.

- [ ] **Step 1: Write the failing test**

```go
func TestSweepRemovesWorktreeOnReclaim(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", State: TaskCompleted,
		Worktree:    "/p/aa-a-c1",
		CompletedAt: now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now); err != nil || reclaimed != 1 {
		t.Fatalf("reclaimed = %d err = %v", reclaimed, err)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "/p/aa-a-c1" {
		t.Fatalf("worktree not removed: %#v", fake.Removed)
	}
}

func TestSweepCapsRetainedFailedSandboxes(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	for i := 0; i < MaxRetainedFailedSandboxes+5; i++ {
		s.Upsert(A2ATask{
			ContextID: fmt.Sprintf("c%d", i),
			Session:   fmt.Sprintf("aa-a-c%d", i),
			Worktree:  fmt.Sprintf("/p/aa-a-c%d", i),
			State:     TaskFailed,
			// oldest first
			CompletedAt: now.Add(-time.Duration(200-i) * time.Hour).Format(time.RFC3339),
		})
	}
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	if _, _, err := SweepTimeouts(context.Background(), root, fake, now); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if len(fake.Removed) != 5 {
		t.Fatalf("removed %d failed sandboxes, want 5 (the oldest beyond the cap)", len(fake.Removed))
	}
	for _, r := range fake.Removed {
		if r == "/p/aa-a-c24" {
			t.Fatal("newest failed sandbox was reclaimed; the cap must drop the OLDEST")
		}
	}
}

func TestSweepKeepsFailedSandboxesUnderTheCap(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", Session: "aa-a-c1", Worktree: "/p/aa-a-c1", State: TaskFailed,
		CompletedAt: now.Add(-300 * time.Hour).Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	fake := &FakeSessionManager{}
	_, _, _ = SweepTimeouts(context.Background(), root, fake, now)
	if len(fake.Removed) != 0 {
		t.Fatalf("a single old failed sandbox must be kept for forensics, got %#v", fake.Removed)
	}
}
```

Add `"fmt"` to the test imports if absent.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestSweepRemoves|TestSweepCaps|TestSweepKeepsFailed' -v`
Expected: FAIL — `fake.Removed` undefined and no removal happens

- [ ] **Step 3: Write minimal implementation**

Add to the `SessionManager` interface and both implementations:

```go
	// RemoveWorkspace tears down a sandbox checkout. Reclaiming only the tmux
	// session leaves ~80MB per task on disk, and contextId is caller-chosen, so
	// without this one approved caller can grow the disk without bound.
	RemoveWorkspace(ctx context.Context, projectDir, worktree string) error
```

```go
func (TmuxSessionManager) RemoveWorkspace(ctx context.Context, projectDir, worktree string) error {
	return RemoveWorktree(ctx, projectDir, worktree)
}

func (f *FakeSessionManager) RemoveWorkspace(_ context.Context, _, worktree string) error {
	if f.FailOn == "remove" {
		return errors.New("fake remove failure")
	}
	f.Removed = append(f.Removed, worktree)
	return nil
}
```

In `SweepTimeouts`, on the reclaim branch also remove the worktree, and add a pass that trims failed sandboxes beyond the cap, oldest first:

```go
// MaxRetainedFailedSandboxes bounds the forensics rule. Keeping every failed
// sandbox forever is itself an unbounded disk-growth path; the newest are the
// ones worth inspecting.
const MaxRetainedFailedSandboxes = 20
```

- [ ] **Step 4: Run tests to verify**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -v 2>&1 | tail -20`
Expected: PASS, including the pre-existing forensics tests

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/
git commit -m "fix(a2a): reclaim worktrees and cap failed-sandbox retention"
```

---

### Task 9: Agent channels — output only

**Files:**
- Modify: `internal/channelagent/a2a_agents.go`
- Create: `internal/channelagent/a2a_channel.go`, `internal/channelagent/a2a_channel_test.go`

**Interfaces:**
- Consumes: `Agent`, `LoadAgents`; `discordThrottle` and the sender in `discord.go`
- Produces: `Agent.ChannelID string` field; `AgentChannelFor(root, agentName string) (string, bool)`; `SandboxOutputPrefix(contextID string) string`

**Requirements:** one channel per `aa-<agent>` identity, shared by that agent's concurrent tasks. Output-only. Every line carries a contextId label, because concurrent tasks interleave in one channel and unlabelled output is unreadable. Reuse the existing activity mirror — do not add a second send path; `discord.go` already carries the per-channel throttle and 429 `retry_after` backoff added after the June flood.

- [ ] **Step 1: Write the failing test**

```go
package channelagent

import (
	"strings"
	"testing"
)

func TestAgentChannelResolves(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "pm", ProjectDir: "/p/x", ChannelID: "chan-pm", Enabled: true})
	_ = SaveAgents(root, agents)

	got, ok := AgentChannelFor(root, "pm")
	if !ok || got != "chan-pm" {
		t.Fatalf("AgentChannelFor = %q, %v", got, ok)
	}
	if _, ok := AgentChannelFor(root, "ghost"); ok {
		t.Fatal("unknown agent must not resolve a channel")
	}
}

// Concurrent tasks of one agent share its channel, so every line must say which
// context it came from or the stream is unreadable.
func TestSandboxOutputPrefixIdentifiesContext(t *testing.T) {
	a := SandboxOutputPrefix("ctxAAA")
	b := SandboxOutputPrefix("ctxBBB")
	if a == b {
		t.Fatal("different contexts must produce different prefixes")
	}
	if !strings.Contains(a, "ctxAAA") {
		t.Fatalf("prefix %q does not identify its context", a)
	}
}

// The agent channel is output-only. If it were ingested, anyone who can type in
// Discord could drive a sandbox directly, bypassing A2A authentication and
// capability grants. Nothing may register it as an input source.
func TestAgentChannelIsNeverAnInputSource(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "pm", ProjectDir: "/p/x", ChannelID: "chan-pm", Enabled: true})
	_ = SaveAgents(root, agents)

	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, ok := reg.BindingByChannel("chan-pm"); ok {
		t.Fatal("an agent channel must never resolve to a binding — that would make it ingestible")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestAgentChannel|TestSandboxOutputPrefix' -v`
Expected: FAIL — `Agent.ChannelID` and `AgentChannelFor` undefined

- [ ] **Step 3: Write minimal implementation**

Add to `Agent` in `a2a_agents.go`:

```go
	// ChannelID is this agent identity's output-only Discord channel. All of the
	// agent's concurrent tasks stream there so an operator can see whether work
	// is actually progressing. It is NEVER ingested: reading it would let anyone
	// who can type in Discord drive a sandbox, bypassing A2A auth entirely.
	ChannelID string `json:"channel_id,omitempty"`
```

Create `a2a_channel.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run 'TestAgentChannel|TestSandboxOutputPrefix' -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Verify the throttle is per-channel, not per-binding**

Read `discordThrottle` in `internal/channelagent/discord.go` and confirm it keys on channel ID. Up to 8 sandboxes streaming into a few agent channels is a shape it has not carried before. Record your finding in the report; if it turns out to key on anything binding-scoped, STOP and report rather than proceeding.

- [ ] **Step 6: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/a2a_agents.go internal/channelagent/a2a_channel.go internal/channelagent/a2a_channel_test.go
git commit -m "feat(a2a): output-only agent channels with context labels"
```

---

### Task 10: Wire driver and A2A cycle off the cron goroutine (I4)

**Files:**
- Modify: `cmd/claude-cron/main.go`
- Test: `internal/channelagent/a2a_config_test.go` (append)

**The defect:** `collect → sweep → drain` currently runs inside the cron scheduler's 30-second ticker goroutine, and `DrainQueue` can start up to 8 sandboxes synchronously at up to 90 seconds each — stalling scheduling for all 40 production bindings.

- [ ] **Step 1: Write the failing test**

```go
// The A2A cycle must not share the cron scheduler's goroutine: DrainQueue can
// block for minutes starting sandboxes, which would stall scheduling for every
// production binding.
func TestA2ACycleIntervalIsIndependent(t *testing.T) {
	var c Config
	if c.A2ACycleInterval() <= 0 {
		t.Fatal("A2ACycleInterval must have a positive default")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/conray/project/claude_cron && go test ./internal/channelagent/ -run TestA2ACycleInterval -v`
Expected: FAIL — `A2ACycleInterval` undefined

- [ ] **Step 3: Write minimal implementation**

Add to `config.go`:

```go
// A2ACycleInterval is how often the A2A lifecycle runs. It has its own ticker
// and goroutine, separate from the cron scheduler's: DrainQueue starts sandboxes
// synchronously and would otherwise stall scheduling for every cc- binding.
func (c Config) A2ACycleInterval() time.Duration {
	if c.A2A.CycleSeconds > 0 {
		return time.Duration(c.A2A.CycleSeconds) * time.Second
	}
	return 10 * time.Second
}
```

Add `CycleSeconds int \`json:"cycle_seconds,omitempty"\`` to `A2AConfig`.

In `main.go`, move the A2A block out of the scheduler goroutine into its own, and start drivers for working tasks each cycle:

```go
		if cfg.A2A.Enabled {
			driver := agent.NewSandboxDriver(*root, *timeout)
			go func() {
				defer driver.StopAll()
				t := time.NewTicker(cfg.A2ACycleInterval())
				defer t.Stop()
				for {
					select {
					case <-supCtx.Done():
						return
					case <-t.C:
					}
					if _, err := agent.CollectResults(*root, time.Now()); err != nil {
						fmt.Fprintf(stdout, "a2a collect: %v\n", err)
					}
					if _, _, err := agent.SweepTimeouts(supCtx, *root, agent.TmuxSessionManager{}, time.Now()); err != nil {
						fmt.Fprintf(stdout, "a2a sweep: %v\n", err)
					}
					if _, err := agent.DrainQueue(supCtx, *root, ex); err != nil {
						fmt.Fprintf(stdout, "a2a drain: %v\n", err)
					}
					agent.EnsureSandboxDrivers(supCtx, *root, driver)
				}
			}()
		}
```

Add `EnsureSandboxDrivers(ctx, root, d *SandboxDriver)` to `a2a_driver.go`: it loads tasks, calls `d.Ensure` for every `working` task with a `TmuxInjector` for that sandbox, and `d.Stop` for every session whose task has reached a terminal state.

**`DrainQueue` still must not run concurrently with itself** — it appears exactly once, in this single goroutine.

- [ ] **Step 4: Verify the disabled path is still inert**

Run: `cd /home/conray/project/claude_cron && go build ./... && go test ./... 2>&1 | tail -5`
Expected: PASS. Then read the diff and confirm every added line in `main.go` sits inside `if cfg.A2A.Enabled`.

- [ ] **Step 5: Commit**

```bash
cd /home/conray/project/claude_cron
git add internal/channelagent/ cmd/claude-cron/main.go
git commit -m "fix(a2a): run the A2A cycle and drivers off the cron goroutine"
```

---

## Self-Review Notes

| Spec requirement | Task |
|---|---|
| C1 sandbox driver delivers the prompt | 4 |
| C1 upstream — trust dialog pre-seed | 3 |
| C1 upstream — auto-answer remaining dialogs | 4 (driver loop) |
| C2 tasks.json lost-update | 1, 2 |
| I1 follow-ups deduped away | 5 |
| I2 terminal contextId takeover | 6 |
| I3 dispatch context + listener timeouts | 7 |
| I4 DrainQueue blocking cron | 10 |
| I5 worktree reclamation + retention cap | 8 |
| Agent channels, output-only, context-labelled | 9 |
| No `cc-` machinery changed | All — new `a2a_*` files plus additive edits |

**Known gap deliberately left open:** the driver's auto-answer of built-in dialogs (task 4) is specified but its detection logic reuses `parseConfirmDialog`, which was written against binding panes. Task 4's implementer must confirm it parses a sandbox pane identically; if not, that becomes a follow-up task rather than being bodged inline.
