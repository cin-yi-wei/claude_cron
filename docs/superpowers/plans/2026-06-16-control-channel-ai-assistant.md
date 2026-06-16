# Control Channel AI Assistant Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `#claude_cron` control channel a hybrid — `/commands` stay deterministic, while free-text messages are handled by an AI assistant (a `cc-control` Claude session in a dedicated workspace) that can also run management via new `claude-cron` CLI subcommands.

**Architecture:** `RunControlOnce` keeps handling `/commands` and now enqueues free-text messages as jobs into a reserved control binding's inbox. The supervisor then drives the `cc-control` session (worker) and posts its reply (sender). New `claude-cron bind/unbind/list` subcommands reuse the existing `HandleCommand` logic so the AI can manage bindings from its Bash tool.

**Tech Stack:** Go (stdlib), tmux 3.x (`-e` env flag), git, Discord REST API. Tests use the package's `runExternalCommand` function-variable override.

---

## File Structure

- Modify: `internal/channelagent/control.go` — `ControlBinding`, `controlSystemPrompt`, `BuildControlDeps`, free-text routing in `RunControlOnce`
- Modify: `internal/channelagent/control_test.go`
- Modify: `internal/channelagent/worktree.go` — `StartControlSession`
- Modify: `internal/channelagent/worktree_test.go`
- Modify: `internal/channelagent/supervisor.go` — call `BuildControlDeps`, drive the control binding worker+sender
- Modify: `cmd/claude-cron/main.go` — `bind`/`unbind`/`list` subcommands
- Modify: `cmd/claude-cron/main_test.go`

Reused (do not redefine): `HandleCommand`, `ControlDeps`, `Command`, `Registry`, `Binding`, `LoadRegistry`, `SaveRegistry`, `LoadConfig`, `Config`, `DiscordAdmin`, `DiscordSender`, `EnsureWorktree`, `RemoveWorktree`, `StartTmuxClaude`, `StopTmuxSession`, `EnsureAgentSettings`, `Init`, `pathIn`, `HashSource`, `buildJobID`, `buildRequestID`, `readSeenState`, `seenKey`, `AtomicWriteJSON`, `RunWorkerOnce`, `RunSenderOnce`, `TmuxInjector`, `InputJob`, `runExternalCommand`.

---

## Task 1: Control binding + system prompt helpers

**Files:**
- Modify: `internal/channelagent/control.go`
- Test: `internal/channelagent/control_test.go`

- [ ] **Step 1: Write the failing test** (append to `control_test.go`)

```go
func TestControlBindingDerivation(t *testing.T) {
	b := ControlBinding("/abs/root")
	if b.Name != "control" {
		t.Fatalf("Name = %q", b.Name)
	}
	if b.TmuxSession != "cc-control" {
		t.Fatalf("TmuxSession = %q", b.TmuxSession)
	}
	if b.Root != "/abs/root/control" {
		t.Fatalf("Root = %q", b.Root)
	}
	if b.Worktree != "/abs/root/control-workspace" {
		t.Fatalf("Worktree (workspace) = %q", b.Worktree)
	}
}

func TestControlSystemPromptMentionsCommands(t *testing.T) {
	p := controlSystemPrompt("/abs/root", "/abs/root/control-workspace")
	for _, want := range []string{"claude-cron bind", "claude-cron unbind", "claude-cron list", "/abs/root/control-workspace"} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, p)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/channelagent/ -run 'TestControlBinding|TestControlSystemPrompt'`
Expected: FAIL — undefined `ControlBinding`, `controlSystemPrompt`.

- [ ] **Step 3: Implement** (append to `control.go`; `path/filepath` and `fmt` — add `path/filepath` to the import block, `fmt` is already imported)

```go
// ControlBinding returns the reserved binding describing the control channel's
// own AI assistant session. It is not stored in the registry. The Worktree
// field is reused as the session's working directory (a plain sandbox dir, not
// a git worktree).
func ControlBinding(root string) Binding {
	return Binding{
		Name:        "control",
		TmuxSession: "cc-control",
		Root:        filepath.Join(root, "control"),
		Worktree:    filepath.Join(root, "control-workspace"),
	}
}

// controlSystemPrompt is appended to the cc-control Claude session so it knows
// its role, its workspace, and how to manage bindings.
func controlSystemPrompt(root, workspace string) string {
	return fmt.Sprintf(`你是 claude_cron 的控管助理，透過 Discord 控管頻道與使用者對話。
你的工作目錄（沙盒）是：%s
你可以在這裡執行 shell 指令、建立檔案/資料夾、回答關於這個系統的問題。

要管理「Discord 頻道 ↔ Claude session」綁定時，用以下 CLI（root 用絕對路徑 %s）：
  claude-cron bind <name> <project-dir> <branch> --root %s
  claude-cron unbind <name> [--delete-channel] --root %s
  claude-cron list --root %s

name 只能用小寫字母、數字、減號。回覆使用者時直接用一般文字即可。`,
		workspace, root, root, root, root)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/channelagent/ -run 'TestControlBinding|TestControlSystemPrompt'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/control.go internal/channelagent/control_test.go
git commit -m "feat: add control binding and system prompt helpers"
```

---

## Task 2: BuildControlDeps helper (shared by supervisor + CLI)

**Files:**
- Modify: `internal/channelagent/control.go`
- Modify: `internal/channelagent/supervisor.go` (use the helper)
- Test: `internal/channelagent/control_test.go`

- [ ] **Step 1: Write the failing test** (append to `control_test.go`)

```go
func TestBuildControlDepsWiresConfig(t *testing.T) {
	cfg := Config{}
	cfg.Discord.GuildID = "g1"
	cfg.Discord.TokenEnv = "DISCORD_BOT_TOKEN"
	deps := BuildControlDeps("/abs/root", cfg)
	if deps.Root != "/abs/root" {
		t.Fatalf("Root = %q", deps.Root)
	}
	if deps.GuildID != "g1" {
		t.Fatalf("GuildID = %q", deps.GuildID)
	}
	// The function fields must be wired (non-nil) so HandleCommand can call them.
	if deps.CreateChannel == nil || deps.EnsureWorktree == nil || deps.StartSession == nil || deps.InitRoot == nil {
		t.Fatal("control deps function fields must be non-nil")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/channelagent/ -run TestBuildControlDeps`
Expected: FAIL — undefined `BuildControlDeps`.

- [ ] **Step 3: Implement** (append to `control.go`; needs `os` — already imported)

```go
// BuildControlDeps assembles a ControlDeps wired to the real Discord/worktree/
// tmux implementations. Shared by the supervisor and the management CLI so
// /bind and `claude-cron bind` behave identically.
func BuildControlDeps(root string, cfg Config) ControlDeps {
	token := os.Getenv(cfg.Discord.TokenEnv)
	admin := DiscordAdmin{BaseURL: cfg.Discord.BaseURL, Token: token}
	return ControlDeps{
		Root:           root,
		GuildID:        cfg.Discord.GuildID,
		CreateChannel:  admin.CreateChannel,
		DeleteChannel:  admin.DeleteChannel,
		EnsureWorktree: EnsureWorktree,
		RemoveWorktree: RemoveWorktree,
		StartSession:   StartTmuxClaude,
		StopSession:    StopTmuxSession,
		InitRoot:       Init,
	}
}
```

- [ ] **Step 4: Refactor supervisor to use it**

In `supervisor.go` `RunSupervisorOnce`, replace the inline `deps := ControlDeps{...}` block (the one wiring CreateChannel/EnsureWorktree/etc) with:
```go
	deps := BuildControlDeps(root, cfg)
```
Keep everything else (the `token`, `admin` for any other use — if `admin` becomes unused after this, remove the now-unused `admin :=` line and the now-unused `token` only if nothing else uses them; the control source/sender below still need `token`, so keep `token`). Run `go build ./...` and fix any unused-variable error by removing only the truly-unused locals.

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/channelagent/ -run TestBuildControlDeps && go build ./...`
Expected: PASS + clean build. Also `go test ./internal/channelagent/` (full package) stays green.

- [ ] **Step 6: Commit**

```bash
git add internal/channelagent/control.go internal/channelagent/supervisor.go
git commit -m "feat: extract BuildControlDeps shared by supervisor and CLI"
```

---

## Task 3: Management CLI subcommands (bind/unbind/list)

**Files:**
- Modify: `cmd/claude-cron/main.go`
- Test: `cmd/claude-cron/main_test.go`

- [ ] **Step 1: Write the failing test** (append to `main_test.go`)

Read the top of `main_test.go` first to match how it invokes `run(...)` (it calls `run(args, stdout, stderr)` with `bytes.Buffer`s). Then add:
```go
func TestListSubcommandPrintsBindings(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	reg := agent.Registry{}
	_ = reg.Add(agent.BindingDefaults(root, "proj-a", "/p/a", "dev"))
	if err := agent.SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"list", "--root", root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proj-a") {
		t.Fatalf("stdout = %q, want it to list proj-a", stdout.String())
	}
}

func TestBindSubcommandRejectsBadName(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Minimal config so LoadConfig succeeds.
	cfg, _ := agent.DefaultConfig("discord")
	cfg.Discord.ChannelID = "c1"
	cfg.Discord.GuildID = "g1"
	_ = agent.SaveConfig(root, cfg)

	var stdout, stderr bytes.Buffer
	code := run([]string{"bind", "Bad_Name", "/p/a", "dev", "--root", root}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for bad name; stdout=%s", stdout.String())
	}
	if !strings.Contains(stdout.String()+stderr.String(), "name") {
		t.Fatalf("expected name error, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
```
Ensure `main_test.go` imports `bytes`, `path/filepath`, `strings`, `testing`, and the `agent` alias (match the existing alias in main_test.go — it is `agent "claude_cron/internal/channelagent"`; if main_test.go has no import block yet for it, add it).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/claude-cron/ -run 'TestListSubcommand|TestBindSubcommand'`
Expected: FAIL — `run` does not handle `list`/`bind` (falls through to usage, exit 2, or wrong output).

- [ ] **Step 3: Implement the subcommands**

In `main.go`, in the `switch args[0]` in `run(...)`, add these cases (place them near the other cases). They reuse `agent.HandleCommand`:
```go
	case "bind", "unbind", "list":
		return runManageCommand(args[0], args[1:], stdout, stderr)
```
Then add this function to `main.go`:
```go
func runManageCommand(name string, rest []string, stdout, stderr io.Writer) int {
	// Extract --root and --delete-channel; everything else is a positional arg.
	root := ".channel-agent"
	deleteChannel := false
	var pos []string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--root":
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--root requires a value")
				return 2
			}
			root = rest[i+1]
			i++
		case "--delete-channel":
			deleteChannel = true
		default:
			pos = append(pos, rest[i])
		}
	}

	absRoot, err := filepath.Abs(root)
	if err == nil {
		root = absRoot
	}

	cfg, err := agent.LoadConfig(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reg, err := agent.LoadRegistry(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	deps := agent.BuildControlDeps(root, cfg)

	cmd := agent.Command{Name: name, Flags: map[string]bool{}}
	cmd.Args = pos
	if deleteChannel {
		cmd.Flags["delete-channel"] = true
	}

	reply, changed, herr := agent.HandleCommand(context.Background(), deps, &reg, cmd)
	if changed {
		if serr := agent.SaveRegistry(root, reg); serr != nil {
			fmt.Fprintln(stderr, serr)
			return 1
		}
	}
	if reply != "" {
		fmt.Fprintln(stdout, reply)
	}
	if herr != nil {
		fmt.Fprintln(stderr, herr)
		return 1
	}
	// Validation rejections (bad name, missing dir) come back as a reply with
	// changed=false and no error. Treat "not changed and not a read-only list"
	// as a failure exit so the AI/script sees it.
	if !changed && name != "list" {
		return 1
	}
	return 0
}
```
Confirm `main.go` already imports `context`, `fmt`, `io`, `path/filepath`, and the `agent` alias. Add `path/filepath` if missing.

NOTE on `filepath.Abs`: the variable name `absRoot` must not shadow incorrectly — written as above it is fine.

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./cmd/claude-cron/ -run 'TestListSubcommand|TestBindSubcommand'`
Expected: PASS. (`list` prints proj-a, exit 0; `bind Bad_Name` exits 1 with a name error and changes nothing.)
Run: `go test ./... && go build ./...`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add cmd/claude-cron/main.go cmd/claude-cron/main_test.go
git commit -m "feat: add bind/unbind/list CLI subcommands reusing HandleCommand"
```

---

## Task 4: StartControlSession (tmux with env + system prompt)

**Files:**
- Modify: `internal/channelagent/worktree.go`
- Test: `internal/channelagent/worktree_test.go`

- [ ] **Step 1: Write the failing test** (append to `worktree_test.go`)

```go
func TestStartControlSessionInjectsTokenAndPrompt(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "has-session" {
			return context.Canceled // not running yet
		}
		return nil
	}

	cwd := t.TempDir()
	if err := StartControlSession(context.Background(), "cc-control", cwd, "DISCORD_BOT_TOKEN", "tok123", "SYS PROMPT"); err != nil {
		t.Fatalf("StartControlSession: %v", err)
	}
	// Find the new-session call.
	var start []string
	for _, c := range calls {
		if len(c) > 1 && c[1] == "new-session" {
			start = c
		}
	}
	if start == nil {
		t.Fatalf("no new-session call: %#v", calls)
	}
	joined := strings.Join(start, " ")
	for _, want := range []string{"-e DISCORD_BOT_TOKEN=tok123", "-c " + cwd, "--append-system-prompt", "SYS PROMPT", "claude"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("new-session missing %q: %v", want, start)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/channelagent/ -run TestStartControlSession`
Expected: FAIL — undefined `StartControlSession`.

- [ ] **Step 3: Implement** (append to `worktree.go`)

```go
// StartControlSession starts the control channel's AI assistant session: a
// detached tmux session running `claude` with the given system prompt appended
// and the Discord bot token injected into the session environment (so the
// assistant's `claude-cron` management calls can authenticate). No-op if the
// session already exists. tokenEnv is the env var name, tokenValue its value.
func StartControlSession(ctx context.Context, session, cwd, tokenEnv, tokenValue, systemPrompt string) error {
	if err := EnsureAgentSettings(cwd); err != nil {
		return err
	}
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	return runExternalCommand(ctx, "tmux", "new-session", "-d", "-s", session,
		"-c", cwd, "-e", tokenEnv+"="+tokenValue,
		"claude", "--append-system-prompt", systemPrompt)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/channelagent/ -run TestStartControlSession`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/worktree.go internal/channelagent/worktree_test.go
git commit -m "feat: add StartControlSession with token env and system prompt"
```

---

## Task 5: Route free-text control messages into the control inbox

**Files:**
- Modify: `internal/channelagent/control.go` (`RunControlOnce` gains a `controlRoot` param + free-text enqueue)
- Modify: `internal/channelagent/supervisor.go` (pass controlRoot)
- Test: `internal/channelagent/control_test.go`, `internal/channelagent/supervisor_test.go`

- [ ] **Step 1: Update existing callers' tests to the new signature**

`RunControlOnce` will gain a `controlRoot string` parameter after `root`. First update the existing test in `supervisor_test.go` (`TestRunControlOnceExecutesCommandAndReplies`) and any other `RunControlOnce(...)` call to pass a control root. Change each call:
```go
RunControlOnce(context.Background(), root, ControlBinding(root).Root, deps, &reg, src, sender)
```
(Insert `ControlBinding(root).Root` as the 3rd argument.)

- [ ] **Step 2: Write the failing test** (append to `control_test.go`)

```go
func TestRunControlOnceRoutesFreeTextToInbox(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	controlRoot := ControlBinding(root).Root
	if err := Init(controlRoot); err != nil {
		t.Fatalf("Init controlRoot: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	src := stubSource{msgs: []SourceMessage{{
		Platform: "discord", ChannelID: "ctl", MessageID: "m1",
		AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z",
		Content: "幫我建一個 logs 資料夾",
	}}}
	sender := &capSender{}

	if err := RunControlOnce(context.Background(), root, controlRoot, deps, &reg, src, sender); err != nil {
		t.Fatalf("RunControlOnce: %v", err)
	}
	// Free text should NOT be sent a reply directly and should produce a job.
	pending, _ := os.ReadDir(pathIn(controlRoot, "inbox", "pending"))
	if len(pending) != 1 {
		t.Fatalf("expected 1 queued control job, got %d", len(pending))
	}

	// Second poll: message already seen, no duplicate job.
	if err := RunControlOnce(context.Background(), root, controlRoot, deps, &reg, src, sender); err != nil {
		t.Fatalf("RunControlOnce 2: %v", err)
	}
	pending2, _ := os.ReadDir(pathIn(controlRoot, "inbox", "pending"))
	if len(pending2) != 1 {
		t.Fatalf("free-text message re-enqueued, pending=%d", len(pending2))
	}
}
```
(`control_test.go` will need `os` imported — add it.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/channelagent/ -run TestRunControlOnceRoutesFreeText`
Expected: FAIL — signature mismatch / no job enqueued.

- [ ] **Step 4: Implement**

Change `RunControlOnce` signature and the non-command branch in `control.go`:
```go
func RunControlOnce(ctx context.Context, root, controlRoot string, deps ControlDeps, reg *Registry, source MessageSource, sender Sender) error {
```
Inside the loop, replace the current `cmd, ok := ParseCommand(...)` / `if !ok { ... }` handling so that:
- if NOT a command → enqueue a job into controlRoot, mark seen, continue
- if a command → existing HandleCommand path

Concretely, the loop body becomes:
```go
	for _, m := range messages {
		key := seenKey(m)
		if state.MessageIDs[key] {
			continue
		}
		cmd, ok := ParseCommand(m.Content)
		if !ok {
			// Free text → hand to the control AI assistant via its job queue.
			if err := enqueueControlJob(controlRoot, m); err != nil {
				// leave unseen so it retries next poll
				continue
			}
			state.MessageIDs[key] = true
			continue
		}
		reply, regChanged, herr := HandleCommand(ctx, deps, reg, cmd)
		if herr != nil {
			_ = sender.Send(ctx, OutputJob{Send: true, Text: "⚠️ " + herr.Error()})
			continue
		}
		state.MessageIDs[key] = true
		if regChanged {
			changed = true
		}
		if reply != "" {
			_ = sender.Send(ctx, OutputJob{Send: true, Text: reply})
		}
	}
```
Add the helper to `control.go`:
```go
// enqueueControlJob writes a free-text control message into the control
// binding's inbox so the cc-control assistant session processes it through the
// normal worker/sender pipeline. Mirrors the watcher's job construction.
func enqueueControlJob(controlRoot string, m SourceMessage) error {
	if err := Init(controlRoot); err != nil {
		return err
	}
	hash, err := HashSource(m)
	if err != nil {
		return err
	}
	job := InputJob{
		Schema:    1,
		JobID:     buildJobID(m, hash),
		RequestID: buildRequestID(m, hash),
		InputHash: hash,
		Source:    m,
		CreatedAt: m.CreatedAt,
	}
	return AtomicWriteJSON(pathIn(controlRoot, "inbox", "pending", job.JobID+".json"), job)
}
```

- [ ] **Step 5: Run tests to verify pass**

Run: `go test ./internal/channelagent/ -run 'TestRunControlOnce'`
Expected: PASS (both the command and free-text routing tests).
Run: `go test ./... && go build ./...`
Expected: all green (the supervisor caller is updated in Task 6; if `supervisor.go` still calls the old signature it will not build yet — update it now too: in `supervisor.go` change the `RunControlOnce(ctx, root, deps, &reg, controlSource, controlSender)` call to `RunControlOnce(ctx, root, ControlBinding(root).Root, deps, &reg, controlSource, controlSender)`).

- [ ] **Step 6: Commit**

```bash
git add internal/channelagent/control.go internal/channelagent/control_test.go internal/channelagent/supervisor.go internal/channelagent/supervisor_test.go
git commit -m "feat: route free-text control messages to the control AI inbox"
```

---

## Task 6: Supervisor drives the control assistant session

**Files:**
- Modify: `internal/channelagent/supervisor.go`
- Test: `internal/channelagent/supervisor_test.go`

- [ ] **Step 1: Write the failing test** (append to `supervisor_test.go`)

This tests the new exported helper `runControlAssistant`, which starts the session (mocked), runs the worker against a queued job (using a fake injector that writes the reply), and sends via a capturing sender.
```go
func TestRunControlAssistantProcessesQueuedJob(t *testing.T) {
	oldRun := runExternalCommand
	defer func() { runExternalCommand = oldRun }()
	runExternalCommand = func(_ context.Context, _ string, _ ...string) error { return nil }

	root := filepath.Join(t.TempDir(), ".channel-agent")
	controlRoot := ControlBinding(root).Root
	if err := Init(controlRoot); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Seed a queued control job.
	msg := SourceMessage{Platform: "discord", ChannelID: "ctl", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z", Content: "hi"}
	if err := enqueueControlJob(controlRoot, msg); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	injector := fakeInjector{write: func(job InputJob, outputPath string) error {
		return AtomicWriteJSON(outputPath, OutputJob{Schema: 1, JobID: job.JobID, RequestID: job.RequestID, InputHash: job.InputHash, Send: true, Text: "你好"})
	}}
	sender := &capSender{}

	if err := runControlAssistant(context.Background(), root, "DISCORD_BOT_TOKEN", "tok", injector, sender, time.Second); err != nil {
		t.Fatalf("runControlAssistant: %v", err)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "你好" {
		t.Fatalf("expected reply 你好, got %#v", sender.sent)
	}
}
```
(`fakeInjector` already exists in `worker_test.go`, same package. `time` is already imported in supervisor_test.go.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/channelagent/ -run TestRunControlAssistant`
Expected: FAIL — undefined `runControlAssistant`.

- [ ] **Step 3: Implement** (in `supervisor.go`)

Add the helper:
```go
// runControlAssistant ensures the control assistant session exists and drives
// one worker+sender cycle for any queued free-text control jobs. injector and
// the session lifecycle are parameterized for testing; in production the
// supervisor passes a TmuxInjector bound to cc-control.
func runControlAssistant(ctx context.Context, root, tokenEnv, tokenValue string, injector Injector, sender Sender, timeout time.Duration) error {
	cb := ControlBinding(root)
	if err := os.MkdirAll(cb.Worktree, 0o755); err != nil {
		return err
	}
	if err := Init(cb.Root); err != nil {
		return err
	}
	if _, err := RunWorkerOnce(ctx, cb.Root, injector, timeout); err != nil {
		return err
	}
	if _, err := RunSenderOnce(ctx, cb.Root, sender); err != nil {
		return err
	}
	return nil
}
```

Then wire it into `RunSupervisorOnce`, AFTER the `RunControlOnce(...)` call and the registry reload, BEFORE the per-binding loop:
```go
	cb := ControlBinding(root)
	if err := StartControlSession(ctx, cb.TmuxSession, cb.Worktree, cfg.Discord.TokenEnv, token, controlSystemPrompt(root, cb.Worktree)); err != nil {
		fmt.Fprintf(stdout, "control session error: %v\n", err)
	} else {
		controlInjector := TmuxInjector{Session: cb.TmuxSession, Root: cb.Root, AutoStart: false}
		controlChatSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
		if err := runControlAssistant(ctx, root, cfg.Discord.TokenEnv, token, controlInjector, controlChatSender, timeout); err != nil {
			fmt.Fprintf(stdout, "control assistant error: %v\n", err)
		}
	}
```
(`token` is the `os.Getenv(cfg.Discord.TokenEnv)` value already computed at the top of `RunSupervisorOnce`.)

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/channelagent/ -run TestRunControlAssistant`
Expected: PASS.
Run: `go test ./... && go build ./...`
Expected: all green.

- [ ] **Step 5: Smoke check (no token)**

Run:
```bash
make build
rm -rf /tmp/cc-ai && ./bin/claude-cron init discord --root /tmp/cc-ai --discord-channel-id 123
./bin/claude-cron serve --root /tmp/cc-ai -once 2>&1 | head
```
Expected: one cycle runs without panic. With no token it prints a control error line and exits 0 (graceful), same as before.

- [ ] **Step 6: Commit**

```bash
git add internal/channelagent/supervisor.go internal/channelagent/supervisor_test.go
git commit -m "feat: supervisor drives the control assistant session"
```

---

## Self-Review

**Spec coverage:**
- Hybrid control channel (commands + free-text AI) → Task 5 (`RunControlOnce` routing) + Task 6 (drive session). ✓
- Dedicated workspace `<root>/control-workspace` → Task 1 (`ControlBinding`) + Task 6 (`os.MkdirAll`). ✓
- AI executes management via `claude-cron` CLI → Task 3 (subcommands) + Task 1 (system prompt advertises them). ✓
- Token injected into control session → Task 4 (`StartControlSession -e`). ✓
- Shared `HandleCommand` logic between `/commands` and CLI → Task 2 (`BuildControlDeps`) + Task 3. ✓
- System prompt explains role/workspace/commands → Task 1. ✓
- Control worker+sender separate from watcher (RunControlOnce already enqueues) → Task 6 (`runControlAssistant` calls only worker+sender). ✓
- Testing items from spec → covered in Tasks 1–6. ✓

**Placeholder scan:** No TBD/TODO; every code step has full code. ✓

**Type consistency:** `RunControlOnce` new signature `(ctx, root, controlRoot, deps, reg, source, sender)` updated at both call sites (supervisor.go in Task 5, tests in Task 5 Step 1). `ControlBinding`/`controlSystemPrompt`/`BuildControlDeps`/`StartControlSession`/`enqueueControlJob`/`runControlAssistant` signatures are used consistently across tasks. `ControlBinding.Worktree` is intentionally the workspace dir (documented). CLI `runManageCommand` uses `agent.Command`/`agent.HandleCommand`/`agent.BuildControlDeps` matching their real signatures. ✓
