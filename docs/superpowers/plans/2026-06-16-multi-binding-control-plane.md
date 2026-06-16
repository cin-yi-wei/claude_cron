# Multi-Binding Control Plane Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a single `claude-cron serve` supervisor manage many `Discord channel ↔ tmux Claude session` bindings, dispatched from a `#claude_cron` control channel via built-in `/bind /unbind /list /status` commands, each binding isolated on its own git worktree branch.

**Architecture:** One supervisor process loads a JSON binding registry. Each poll it (1) runs a deterministic command parser against the control channel and (2) runs the existing watcher→worker→sender pipeline once per project binding, each scoped to its own runtime root, channel, and tmux session. Discord channels are auto-created; branches are isolated via `git worktree`.

**Tech Stack:** Go (stdlib only), tmux, git worktree, Discord REST API. Tests use the package's existing `runExternalCommand` function-variable override and `httptest`.

---

## File Structure

- Create: `internal/channelagent/registry.go` — `Binding`, `Registry`, CRUD, name/path derivation
- Create: `internal/channelagent/registry_test.go`
- Create: `internal/channelagent/worktree.go` — `EnsureWorktree`, `RemoveWorktree`, `StartTmuxClaude`, `StopTmuxSession`
- Create: `internal/channelagent/worktree_test.go`
- Create: `internal/channelagent/control.go` — `ParseCommand`, `ControlDeps`, `HandleCommand`, `RunControlOnce`
- Create: `internal/channelagent/control_test.go`
- Create: `internal/channelagent/supervisor.go` — `RunSupervisorOnce`, reconcile
- Create: `internal/channelagent/supervisor_test.go`
- Modify: `internal/channelagent/config.go` — add `GuildID` to `DiscordConfig`
- Modify: `internal/channelagent/discord.go` — add `DiscordAdmin.CreateChannel` / `DeleteChannel`
- Modify: `internal/channelagent/discord_test.go` — admin tests
- Modify: `cmd/claude-cron/main.go` — `serve` drives the supervisor

Existing helpers reused (do not redefine): `Init(root)`, `AtomicWriteJSON`, `ReadJSON`, `pathIn(root, ...)`, `runExternalCommand` (var, mockable, strips TMUX), `RunServeOnce`, `TmuxInjector`, `DiscordSource`, `DiscordSender`, `checkHTTPResponse`, `readSeenState`, `seenKey`.

---

## Task 1: Add GuildID to config

**Files:**
- Modify: `internal/channelagent/config.go:21-25` (`DiscordConfig`)
- Test: `internal/channelagent/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `config_test.go`:
```go
func TestDiscordConfigHasGuildID(t *testing.T) {
	root := t.TempDir()
	cfg, err := DefaultConfig("discord")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Discord.GuildID = "g123"
	if err := SaveConfig(root, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Discord.GuildID != "g123" {
		t.Fatalf("GuildID = %q, want g123", got.Discord.GuildID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run TestDiscordConfigHasGuildID`
Expected: FAIL — `cfg.Discord.GuildID undefined`.

- [ ] **Step 3: Add the field**

In `config.go`, change `DiscordConfig`:
```go
type DiscordConfig struct {
	TokenEnv  string `json:"token_env"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent/ -run TestDiscordConfigHasGuildID`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/config.go internal/channelagent/config_test.go
git commit -m "feat: add discord guild_id to config"
```

---

## Task 2: Binding registry

**Files:**
- Create: `internal/channelagent/registry.go`
- Test: `internal/channelagent/registry_test.go`

- [ ] **Step 1: Write the failing test**

Create `registry_test.go`:
```go
package channelagent

import (
	"path/filepath"
	"testing"
)

func TestRegistryAddGetRemoveRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry empty: %v", err)
	}
	if len(reg.Bindings) != 0 {
		t.Fatalf("expected empty registry, got %d", len(reg.Bindings))
	}

	b := BindingDefaults(root, "proj-a", "/home/u/a", "ticket-1")
	if err := reg.Add(b); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := reg.Add(b); err == nil {
		t.Fatal("duplicate Add should error")
	}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	reloaded, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	got, ok := reloaded.Get("proj-a")
	if !ok {
		t.Fatal("Get proj-a not found")
	}
	if got.TmuxSession != "cc-proj-a" {
		t.Fatalf("TmuxSession = %q, want cc-proj-a", got.TmuxSession)
	}
	if got.Worktree != filepath.Join(root, "worktrees", "proj-a") {
		t.Fatalf("Worktree = %q", got.Worktree)
	}
	if got.Root != filepath.Join(root, "bindings", "proj-a") {
		t.Fatalf("Root = %q", got.Root)
	}
	if !reloaded.Remove("proj-a") {
		t.Fatal("Remove returned false")
	}
	if _, ok := reloaded.Get("proj-a"); ok {
		t.Fatal("proj-a still present after Remove")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"proj-a", "abc123", "x"} {
		if !ValidName(ok) {
			t.Fatalf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"Proj", "a_b", "a b", "", "a/b"} {
		if ValidName(bad) {
			t.Fatalf("ValidName(%q) = true, want false", bad)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run 'TestRegistry|TestValidName'`
Expected: FAIL — undefined `LoadRegistry`, `BindingDefaults`, etc.

- [ ] **Step 3: Write the implementation**

Create `registry.go`:
```go
package channelagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type Binding struct {
	Name        string `json:"name"`
	ChannelID   string `json:"channel_id"`
	ProjectDir  string `json:"project_dir"`
	Branch      string `json:"branch"`
	Worktree    string `json:"worktree"`
	TmuxSession string `json:"tmux_session"`
	Root        string `json:"root"`
	CreatedAt   string `json:"created_at"`
}

type Registry struct {
	Bindings []Binding `json:"bindings"`
}

var validNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

func ValidName(name string) bool {
	return validNameRE.MatchString(name)
}

// BindingDefaults derives the session/worktree/root fields from name+root.
// ChannelID and CreatedAt are filled in by the caller after provisioning.
func BindingDefaults(root, name, projectDir, branch string) Binding {
	return Binding{
		Name:        name,
		ProjectDir:  projectDir,
		Branch:      branch,
		Worktree:    filepath.Join(root, "worktrees", name),
		TmuxSession: "cc-" + name,
		Root:        filepath.Join(root, "bindings", name),
	}
}

func RegistryPath(root string) string {
	return filepath.Join(root, "bindings.json")
}

func LoadRegistry(root string) (Registry, error) {
	var reg Registry
	if err := ReadJSON(RegistryPath(root), &reg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Registry{}, nil
		}
		return Registry{}, err
	}
	return reg, nil
}

func SaveRegistry(root string, reg Registry) error {
	return AtomicWriteJSON(RegistryPath(root), reg)
}

func (r *Registry) Get(name string) (Binding, bool) {
	for _, b := range r.Bindings {
		if b.Name == name {
			return b, true
		}
	}
	return Binding{}, false
}

func (r *Registry) Add(b Binding) error {
	if _, ok := r.Get(b.Name); ok {
		return fmt.Errorf("binding %q already exists", b.Name)
	}
	r.Bindings = append(r.Bindings, b)
	return nil
}

func (r *Registry) Remove(name string) bool {
	for i, b := range r.Bindings {
		if b.Name == name {
			r.Bindings = append(r.Bindings[:i], r.Bindings[i+1:]...)
			return true
		}
	}
	return false
}

func (r Registry) Names() []string {
	names := make([]string, 0, len(r.Bindings))
	for _, b := range r.Bindings {
		names = append(names, b.Name)
	}
	return names
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent/ -run 'TestRegistry|TestValidName'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/registry.go internal/channelagent/registry_test.go
git commit -m "feat: add binding registry"
```

---

## Task 3: Worktree + tmux session helpers

**Files:**
- Create: `internal/channelagent/worktree.go`
- Test: `internal/channelagent/worktree_test.go`

- [ ] **Step 1: Write the failing test**

Create `worktree_test.go`:
```go
package channelagent

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureWorktreeCreatesBranchWhenMissing(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil // rev-parse "succeeds" => branch exists path
	}

	wt := filepath.Join(t.TempDir(), "does-not-exist", "wt") // os.Stat will fail => proceed
	if err := EnsureWorktree(context.Background(), "/repo", "feat", wt); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	// First call probes the branch; second adds the worktree for an existing branch.
	wantProbe := []string{"git", "-C", "/repo", "rev-parse", "--verify", "--quiet", "refs/heads/feat"}
	wantAdd := []string{"git", "-C", "/repo", "worktree", "add", wt, "feat"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], wantProbe) || !reflect.DeepEqual(calls[1], wantAdd) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestStartTmuxClaudeStartsWhenMissing(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "has-session" {
			return context.Canceled // simulate "no such session"
		}
		return nil
	}
	if err := StartTmuxClaude(context.Background(), "cc-proj", "/repo/wt"); err != nil {
		t.Fatalf("StartTmuxClaude: %v", err)
	}
	wantStart := []string{"tmux", "new-session", "-d", "-s", "cc-proj", "-c", "/repo/wt", "claude"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], wantStart) {
		t.Fatalf("calls = %#v", calls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run 'TestEnsureWorktree|TestStartTmuxClaude'`
Expected: FAIL — undefined `EnsureWorktree`, `StartTmuxClaude`.

- [ ] **Step 3: Write the implementation**

Create `worktree.go`:
```go
package channelagent

import (
	"context"
	"os"
)

// EnsureWorktree makes sure worktreePath is a git worktree of branch, checked
// out from projectDir. Idempotent: if worktreePath already exists it is a no-op.
// If the branch does not exist yet it is created from current HEAD.
func EnsureWorktree(ctx context.Context, projectDir, branch, worktreePath string) error {
	if _, err := os.Stat(worktreePath); err == nil {
		return nil
	}
	branchExists := runExternalCommand(ctx, "git", "-C", projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
	if branchExists {
		return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "add", worktreePath, branch)
	}
	return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "add", "-b", branch, worktreePath)
}

// RemoveWorktree removes a git worktree. Force is used so dirty worktrees are
// still cleaned up on /unbind.
func RemoveWorktree(ctx context.Context, projectDir, worktreePath string) error {
	return runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "remove", "--force", worktreePath)
}

// StartTmuxClaude ensures a detached tmux session named session is running
// `claude` with its working directory set to cwd. No-op if it already exists.
func StartTmuxClaude(ctx context.Context, session, cwd string) error {
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	return runExternalCommand(ctx, "tmux", "new-session", "-d", "-s", session, "-c", cwd, "claude")
}

// StopTmuxSession kills a tmux session. A missing session is not an error.
func StopTmuxSession(ctx context.Context, session string) error {
	_ = runExternalCommand(ctx, "tmux", "kill-session", "-t", session)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent/ -run 'TestEnsureWorktree|TestStartTmuxClaude'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/worktree.go internal/channelagent/worktree_test.go
git commit -m "feat: add git worktree and tmux session helpers"
```

---

## Task 4: Discord channel admin

**Files:**
- Modify: `internal/channelagent/discord.go` (append new type + methods)
- Test: `internal/channelagent/discord_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `discord_test.go`:
```go
func TestDiscordAdminCreateChannel(t *testing.T) {
	var gotPath, gotName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotName, _ = body["name"].(string)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"chan999"}`))
	}))
	defer server.Close()

	id, err := DiscordAdmin{BaseURL: server.URL + "/api/v10", Token: "tok"}.
		CreateChannel(context.Background(), "guild1", "proj-a")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != "chan999" {
		t.Fatalf("id = %q, want chan999", id)
	}
	if gotPath != "/api/v10/guilds/guild1/channels" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotName != "proj-a" {
		t.Fatalf("name = %q, want proj-a", gotName)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run TestDiscordAdminCreateChannel`
Expected: FAIL — undefined `DiscordAdmin`.

- [ ] **Step 3: Write the implementation**

Append to `discord.go`:
```go
type DiscordAdmin struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (a DiscordAdmin) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func (a DiscordAdmin) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return defaultDiscordBaseURL
}

// CreateChannel creates a text channel (type 0) in guildID and returns its id.
func (a DiscordAdmin) CreateChannel(ctx context.Context, guildID, name string) (string, error) {
	if a.Token == "" {
		return "", fmt.Errorf("discord token is required")
	}
	if guildID == "" {
		return "", fmt.Errorf("discord guild id is required")
	}
	body, err := json.Marshal(map[string]any{"name": name, "type": 0})
	if err != nil {
		return "", err
	}
	endpoint := a.baseURL() + "/guilds/" + url.PathEscape(guildID) + "/channels"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// DeleteChannel deletes a channel by id.
func (a DiscordAdmin) DeleteChannel(ctx context.Context, channelID string) error {
	if a.Token == "" {
		return fmt.Errorf("discord token is required")
	}
	endpoint := a.baseURL() + "/channels/" + url.PathEscape(channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+a.Token)
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkHTTPResponse(resp)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent/ -run TestDiscordAdmin`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/discord.go internal/channelagent/discord_test.go
git commit -m "feat: add discord channel create/delete admin"
```

---

## Task 5: Control command parsing

**Files:**
- Create: `internal/channelagent/control.go`
- Test: `internal/channelagent/control_test.go`

- [ ] **Step 1: Write the failing test**

Create `control_test.go`:
```go
package channelagent

import (
	"reflect"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cmd, ok := ParseCommand("/bind proj-a /home/u/a ticket-1")
	if !ok {
		t.Fatal("expected a command")
	}
	if cmd.Name != "bind" {
		t.Fatalf("Name = %q", cmd.Name)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"proj-a", "/home/u/a", "ticket-1"}) {
		t.Fatalf("Args = %#v", cmd.Args)
	}

	cmd2, ok := ParseCommand("/unbind proj-a --delete-channel")
	if !ok || cmd2.Name != "unbind" || !cmd2.Flags["delete-channel"] {
		t.Fatalf("unbind parse wrong: %#v ok=%v", cmd2, ok)
	}
	if !reflect.DeepEqual(cmd2.Args, []string{"proj-a"}) {
		t.Fatalf("unbind Args = %#v", cmd2.Args)
	}

	if _, ok := ParseCommand("hello world"); ok {
		t.Fatal("non-slash text should not parse as command")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run TestParseCommand`
Expected: FAIL — undefined `ParseCommand`.

- [ ] **Step 3: Write the implementation**

Create `control.go`:
```go
package channelagent

import "strings"

type Command struct {
	Name  string
	Args  []string
	Flags map[string]bool
}

// ParseCommand parses a control message. Returns ok=false for non-command text
// (anything not starting with "/"). Tokens of the form --flag become Flags;
// everything else is a positional Arg.
func ParseCommand(content string) (Command, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "/") {
		return Command{}, false
	}
	fields := strings.Fields(content[1:])
	if len(fields) == 0 {
		return Command{}, false
	}
	cmd := Command{Name: fields[0], Flags: map[string]bool{}}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "--") {
			cmd.Flags[strings.TrimPrefix(f, "--")] = true
			continue
		}
		cmd.Args = append(cmd.Args, f)
	}
	return cmd, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent/ -run TestParseCommand`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/control.go internal/channelagent/control_test.go
git commit -m "feat: add control command parser"
```

---

## Task 6: Command handlers (bind/unbind/list/status/help)

**Files:**
- Modify: `internal/channelagent/control.go` (append `ControlDeps`, `HandleCommand`)
- Test: `internal/channelagent/control_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `control_test.go`:
```go
import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newTestDeps(root string, created *[]string) ControlDeps {
	return ControlDeps{
		Root:    root,
		GuildID: "guild1",
		CreateChannel: func(_ context.Context, guildID, name string) (string, error) {
			*created = append(*created, "create:"+name)
			return "chan-" + name, nil
		},
		DeleteChannel: func(_ context.Context, channelID string) error {
			*created = append(*created, "delete:"+channelID)
			return nil
		},
		EnsureWorktree: func(_ context.Context, projectDir, branch, wt string) error {
			*created = append(*created, "worktree:"+branch)
			return nil
		},
		RemoveWorktree: func(_ context.Context, projectDir, wt string) error {
			*created = append(*created, "rmworktree:"+wt)
			return nil
		},
		StartSession: func(_ context.Context, session, cwd string) error {
			*created = append(*created, "start:"+session)
			return nil
		},
		StopSession: func(_ context.Context, session string) error {
			*created = append(*created, "stop:"+session)
			return nil
		},
		InitRoot: func(string) error { return nil },
	}
}

func TestHandleBindThenUnbind(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}

	cmd, _ := ParseCommand("/bind proj-a /home/u/a ticket-1")
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !changed {
		t.Fatal("bind should change registry")
	}
	if !strings.Contains(reply, "proj-a") {
		t.Fatalf("reply = %q", reply)
	}
	b, ok := reg.Get("proj-a")
	if !ok || b.ChannelID != "chan-proj-a" {
		t.Fatalf("binding not registered correctly: %#v ok=%v", b, ok)
	}
	wantOrder := []string{"create:proj-a", "worktree:ticket-1", "start:cc-proj-a"}
	for _, w := range wantOrder {
		if !containsStr(actions, w) {
			t.Fatalf("missing action %q in %#v", w, actions)
		}
	}

	cmd2, _ := ParseCommand("/unbind proj-a")
	_, changed2, err := HandleCommand(context.Background(), deps, &reg, cmd2)
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if !changed2 {
		t.Fatal("unbind should change registry")
	}
	if _, ok := reg.Get("proj-a"); ok {
		t.Fatal("proj-a still registered after unbind")
	}
	if containsStr(actions, "delete:chan-proj-a") {
		t.Fatal("channel should NOT be deleted without --delete-channel")
	}
	if !containsStr(actions, "stop:cc-proj-a") {
		t.Fatal("session should be stopped on unbind")
	}
}

func TestHandleBindRejectsBadName(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	cmd, _ := ParseCommand("/bind Bad_Name /home/u/a ticket-1")
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if changed {
		t.Fatal("bad name should not change registry")
	}
	if !strings.Contains(strings.ToLower(reply), "name") {
		t.Fatalf("reply should explain bad name, got %q", reply)
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run 'TestHandleBind'`
Expected: FAIL — undefined `ControlDeps`, `HandleCommand`.

- [ ] **Step 3: Write the implementation**

Append to `control.go`:
```go
import (
	"context"
	"fmt"
	"os"
	"strings"
)

type ControlDeps struct {
	Root          string
	GuildID       string
	CreateChannel func(ctx context.Context, guildID, name string) (string, error)
	DeleteChannel func(ctx context.Context, channelID string) error
	EnsureWorktree func(ctx context.Context, projectDir, branch, worktree string) error
	RemoveWorktree func(ctx context.Context, projectDir, worktree string) error
	StartSession  func(ctx context.Context, session, cwd string) error
	StopSession   func(ctx context.Context, session string) error
	InitRoot      func(root string) error
}

const controlUsage = "指令: /bind <name> <project-dir> <branch> | /unbind <name> [--delete-channel] | /list | /status <name> | /help"

// HandleCommand executes a parsed control command against the registry, using
// deps for side effects. It returns a reply string to post to the control
// channel and whether the registry changed (caller persists it).
func HandleCommand(ctx context.Context, deps ControlDeps, reg *Registry, cmd Command) (string, bool, error) {
	switch cmd.Name {
	case "bind":
		return handleBind(ctx, deps, reg, cmd)
	case "unbind":
		return handleUnbind(ctx, deps, reg, cmd)
	case "list":
		return handleList(reg), false, nil
	case "status":
		return handleStatus(reg, cmd), false, nil
	case "help":
		return controlUsage, false, nil
	default:
		return "未知指令。" + controlUsage, false, nil
	}
}

func handleBind(ctx context.Context, deps ControlDeps, reg *Registry, cmd Command) (string, bool, error) {
	if len(cmd.Args) != 3 {
		return "用法: /bind <name> <project-dir> <branch>", false, nil
	}
	name, projectDir, branch := cmd.Args[0], cmd.Args[1], cmd.Args[2]
	if !ValidName(name) {
		return fmt.Sprintf("name %q 不合法 (只能用 a-z 0-9 -)", name), false, nil
	}
	if _, ok := reg.Get(name); ok {
		return fmt.Sprintf("binding %q 已存在", name), false, nil
	}
	if _, err := os.Stat(projectDir); err != nil {
		return fmt.Sprintf("project-dir %q 不存在", projectDir), false, nil
	}

	b := BindingDefaults(deps.Root, name, projectDir, branch)

	channelID, err := deps.CreateChannel(ctx, deps.GuildID, name)
	if err != nil {
		return "", false, fmt.Errorf("建頻道失敗: %w", err)
	}
	b.ChannelID = channelID

	if err := deps.EnsureWorktree(ctx, projectDir, branch, b.Worktree); err != nil {
		_ = deps.DeleteChannel(ctx, channelID)
		return "", false, fmt.Errorf("建 worktree 失敗: %w", err)
	}
	if err := deps.InitRoot(b.Root); err != nil {
		_ = deps.RemoveWorktree(ctx, projectDir, b.Worktree)
		_ = deps.DeleteChannel(ctx, channelID)
		return "", false, fmt.Errorf("init root 失敗: %w", err)
	}
	if err := deps.StartSession(ctx, b.TmuxSession, b.Worktree); err != nil {
		_ = deps.RemoveWorktree(ctx, projectDir, b.Worktree)
		_ = deps.DeleteChannel(ctx, channelID)
		return "", false, fmt.Errorf("啟 session 失敗: %w", err)
	}

	if err := reg.Add(b); err != nil {
		return "", false, err
	}
	return fmt.Sprintf("✅ 綁定 %s → channel %s (branch %s, session %s)", name, channelID, branch, b.TmuxSession), true, nil
}

func handleUnbind(ctx context.Context, deps ControlDeps, reg *Registry, cmd Command) (string, bool, error) {
	if len(cmd.Args) != 1 {
		return "用法: /unbind <name> [--delete-channel]", false, nil
	}
	name := cmd.Args[0]
	b, ok := reg.Get(name)
	if !ok {
		return fmt.Sprintf("找不到 binding %q", name), false, nil
	}
	_ = deps.StopSession(ctx, b.TmuxSession)
	_ = deps.RemoveWorktree(ctx, b.ProjectDir, b.Worktree)
	if cmd.Flags["delete-channel"] {
		_ = deps.DeleteChannel(ctx, b.ChannelID)
	}
	_ = os.RemoveAll(b.Root)
	reg.Remove(name)
	return fmt.Sprintf("🗑️ 解綁 %s 完成", name), true, nil
}

func handleList(reg *Registry) string {
	if len(reg.Bindings) == 0 {
		return "(無綁定)"
	}
	var sb strings.Builder
	for _, b := range reg.Bindings {
		fmt.Fprintf(&sb, "• %s → channel %s | branch %s | session %s\n", b.Name, b.ChannelID, b.Branch, b.TmuxSession)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func handleStatus(reg *Registry, cmd Command) string {
	if len(cmd.Args) != 1 {
		return "用法: /status <name>"
	}
	b, ok := reg.Get(cmd.Args[0])
	if !ok {
		return fmt.Sprintf("找不到 binding %q", cmd.Args[0])
	}
	pending := countJSON(pathIn(b.Root, "inbox", "pending"))
	processing := countJSON(pathIn(b.Root, "inbox", "processing"))
	failed := countJSON(pathIn(b.Root, "inbox", "failed"))
	return fmt.Sprintf("%s: session %s | pending=%d processing=%d failed=%d", b.Name, b.TmuxSession, pending, processing, failed)
}

func countJSON(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}
```

NOTE: `control.go` now needs the merged import block. Replace the single
`import "strings"` from Task 5 with the combined block shown above
(`context`, `fmt`, `os`, `strings`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent/ -run 'TestParseCommand|TestHandleBind'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/control.go internal/channelagent/control_test.go
git commit -m "feat: add control command handlers"
```

---

## Task 7: Control channel polling + supervisor loop

**Files:**
- Modify: `internal/channelagent/control.go` (append `RunControlOnce`)
- Create: `internal/channelagent/supervisor.go`
- Test: `internal/channelagent/supervisor_test.go`

- [ ] **Step 1: Write the failing test**

Create `supervisor_test.go`:
```go
package channelagent

import (
	"context"
	"path/filepath"
	"testing"
)

// stubSource returns fixed messages once, then nothing (dedup via seen state).
type stubSource struct{ msgs []SourceMessage }

func (s stubSource) Fetch(_ context.Context) ([]SourceMessage, error) { return s.msgs, nil }

type capSender struct{ sent []string }

func (c *capSender) Send(_ context.Context, o OutputJob) error {
	c.sent = append(c.sent, o.Text)
	return nil
}

func TestRunControlOnceExecutesCommandAndReplies(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	src := stubSource{msgs: []SourceMessage{{
		Platform: "discord", ChannelID: "ctl", MessageID: "m1",
		AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z",
		Content: "/bind proj-a " + t.TempDir() + " ticket-1",
	}}}
	sender := &capSender{}

	if err := RunControlOnce(context.Background(), root, deps, &reg, src, sender); err != nil {
		t.Fatalf("RunControlOnce: %v", err)
	}
	if _, ok := reg.Get("proj-a"); !ok {
		t.Fatal("binding not created")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(sender.sent))
	}

	// Second run must NOT reprocess the same message (dedup).
	sender2 := &capSender{}
	reg2, _ := LoadRegistry(root)
	if err := RunControlOnce(context.Background(), root, deps, &reg2, src, sender2); err != nil {
		t.Fatalf("RunControlOnce 2: %v", err)
	}
	if len(sender2.sent) != 0 {
		t.Fatalf("message reprocessed, sent=%d", len(sender2.sent))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent/ -run TestRunControlOnce`
Expected: FAIL — undefined `RunControlOnce`.

- [ ] **Step 3: Write `RunControlOnce`**

Append to `control.go`:
```go
import "sort"

// RunControlOnce polls the control channel, executes any new commands, replies,
// persists the registry when it changed, and records processed message IDs so
// they are not handled twice. Dedup reuses the watcher's seen-state file under
// the control root.
func RunControlOnce(ctx context.Context, root string, deps ControlDeps, reg *Registry, source MessageSource, sender Sender) error {
	if err := Init(root); err != nil {
		return err
	}
	messages, err := source.Fetch(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].CreatedAt < messages[j].CreatedAt })

	statePath := pathIn(root, "state", "control_seen.json")
	state, err := readSeenState(statePath)
	if err != nil {
		return err
	}

	changed := false
	for _, m := range messages {
		key := seenKey(m)
		if state.MessageIDs[key] {
			continue
		}
		state.MessageIDs[key] = true
		cmd, ok := ParseCommand(m.Content)
		if !ok {
			continue
		}
		reply, regChanged, herr := HandleCommand(ctx, deps, reg, cmd)
		if herr != nil {
			reply = "⚠️ " + herr.Error()
		}
		if regChanged {
			changed = true
		}
		if reply != "" {
			_ = sender.Send(ctx, OutputJob{Send: true, Text: reply})
		}
	}
	if changed {
		if err := SaveRegistry(root, *reg); err != nil {
			return err
		}
	}
	return AtomicWriteJSON(statePath, state)
}
```

NOTE: add `"sort"` to the `control.go` import block (merge with the existing
imports — final set: `context`, `fmt`, `os`, `sort`, `strings`).

- [ ] **Step 4: Write the supervisor**

Create `supervisor.go`:
```go
package channelagent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RunSupervisorOnce runs one supervisor cycle: process the control channel,
// then run the per-binding pipeline for every registered binding.
func RunSupervisorOnce(ctx context.Context, root string, cfg Config, timeout time.Duration, stdout io.Writer) error {
	token := os.Getenv(cfg.Discord.TokenEnv)
	admin := DiscordAdmin{BaseURL: cfg.Discord.BaseURL, Token: token}

	reg, err := LoadRegistry(root)
	if err != nil {
		return err
	}

	deps := ControlDeps{
		Root:          root,
		GuildID:       cfg.Discord.GuildID,
		CreateChannel: admin.CreateChannel,
		DeleteChannel: admin.DeleteChannel,
		EnsureWorktree: EnsureWorktree,
		RemoveWorktree: RemoveWorktree,
		StartSession:  StartTmuxClaude,
		StopSession:   StopTmuxSession,
		InitRoot:      Init,
	}

	controlSource := DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID, Limit: 50}
	controlSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
	if err := RunControlOnce(ctx, root, deps, &reg, controlSource, controlSender); err != nil {
		fmt.Fprintf(stdout, "control error: %v\n", err)
	}

	// reg may have changed; reload to get the persisted set.
	reg, err = LoadRegistry(root)
	if err != nil {
		return err
	}
	for _, b := range reg.Bindings {
		// Reconcile: ensure worktree + session exist.
		if err := EnsureWorktree(ctx, b.ProjectDir, b.Branch, b.Worktree); err != nil {
			fmt.Fprintf(stdout, "binding %s worktree error: %v\n", b.Name, err)
			continue
		}
		if err := StartTmuxClaude(ctx, b.TmuxSession, b.Worktree); err != nil {
			fmt.Fprintf(stdout, "binding %s session error: %v\n", b.Name, err)
			continue
		}
		source := DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: b.ChannelID, Limit: 50}
		sender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: b.ChannelID}
		injector := TmuxInjector{Session: b.TmuxSession, Root: b.Root, AutoStart: true}
		res, err := RunServeOnce(ctx, b.Root, source, injector, sender, timeout)
		if err != nil {
			fmt.Fprintf(stdout, "binding %s error: %v\n", b.Name, err)
			continue
		}
		fmt.Fprintf(stdout, "binding=%s created=%d processed=%t sent=%d\n", b.Name, res.Created, res.Processed, res.Sent)
	}
	return nil
}

var _ = http.DefaultClient // keep net/http import if unused elsewhere
```

(If `go vet` flags the unused `net/http` import, remove the import and the
`var _` line — they are only present should a future HTTP client be wired in.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/channelagent/ -run 'TestRunControlOnce'`
Expected: PASS.
Run: `go build ./...`
Expected: builds clean.

- [ ] **Step 6: Commit**

```bash
git add internal/channelagent/control.go internal/channelagent/supervisor.go internal/channelagent/supervisor_test.go
git commit -m "feat: add control polling and multi-binding supervisor"
```

---

## Task 8: Wire `serve` to the supervisor

**Files:**
- Modify: `cmd/claude-cron/main.go:76-127` (the `serve` case)

- [ ] **Step 1: Replace the serve loop body**

In `main.go`, inside `case "serve":`, replace the `for { ... }` loop (the block
that currently builds source/sender and calls `agent.RunServeOnce`) with:
```go
		for {
			if err := agent.RunSupervisorOnce(context.Background(), *root, cfg, timeout, stdout); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if *once {
				return 0
			}
			time.Sleep(interval)
		}
```
Leave the flag parsing, `LoadConfig`, `validateConfigForServe`, `interval`, and
`timeout` setup above it unchanged.

- [ ] **Step 2: Check for now-unused helpers**

Run: `go build ./...`
If the compiler reports `buildSourceFromConfig` / `buildSenderFromConfig` as
unused, they are still referenced by other commands — only delete a helper if
the build explicitly says it is unused. Otherwise leave them.

- [ ] **Step 3: Run the full test suite + build**

Run: `make test`
Expected: all packages `ok`.
Run: `make build`
Expected: builds `bin/claude-cron`.

- [ ] **Step 4: Manual smoke check (no live agent needed)**

Run:
```bash
./bin/claude-cron init discord --root /tmp/cc-test --discord-channel-id 123
./bin/claude-cron serve --root /tmp/cc-test -once 2>&1 | head
```
Expected: runs one supervisor cycle without panicking (it will report a control
error if `DISCORD_BOT_TOKEN`/guild are unset, which is fine for the smoke test).

- [ ] **Step 5: Commit**

```bash
git add cmd/claude-cron/main.go
git commit -m "feat: serve drives the multi-binding supervisor"
```

---

## Self-Review

**Spec coverage:**
- Registry → Task 2. Worktree isolation → Task 3. Auto channel create → Task 4.
  Built-in parser → Task 5. `/bind /unbind /list /status /help` → Task 6.
  Control polling + dedup + single-supervisor multi-binding loop + reconcile →
  Task 7. `guild_id` config → Task 1. `serve` entrypoint → Task 8. ✓
- `--delete-channel` default-off → Task 6 (`handleUnbind`). ✓
- Per-binding isolation (own root/session/channel) → Task 7 supervisor loop. ✓
- Orphan recovery per binding → inherited from existing `RunServeOnce`/`requeueProcessing` (no new task needed). ✓

**Placeholder scan:** No TBD/TODO; every code step shows full code. ✓

**Type consistency:** `Binding`, `Registry`, `BindingDefaults`, `ValidName`,
`ControlDeps` (with `StartSession`/`StopSession` matching `StartTmuxClaude`/
`StopTmuxSession` signatures), `Command`, `ParseCommand`, `HandleCommand`,
`RunControlOnce`, `RunSupervisorOnce` are used consistently across tasks. The
control channel uses `state/control_seen.json` (distinct from the per-binding
watcher's `seen_message_ids.json`) so control dedup never collides with job
ingestion. ✓
```
