# `notify` Subcommand Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add `claude-cron notify <channel-id> <text>` to post a Discord message from the shell, and teach agents (via the per-job prompt) to use it to report long background-task completion.

**Architecture:** Thin CLI over the existing `DiscordSender`; one instruction added to `BuildClaudePrompt`.

**Tech Stack:** Go stdlib, Discord REST. Tests use `httptest` + `t.Setenv`.

---

## Task 1: `notify` subcommand

**Files:**
- Modify: `cmd/claude-cron/main.go`
- Test: `cmd/claude-cron/main_test.go`

- [ ] **Step 1: Write the failing test** (append to `main_test.go`; ensure imports include `bytes`, `encoding/json`, `net/http`, `net/http/httptest`, `path/filepath`, `strings`, `testing`, and `agent "claude_cron/internal/channelagent"`)

```go
func TestNotifySubcommandPostsMessage(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer server.Close()

	t.Setenv("NOTIFY_TEST_TOKEN", "tok")
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := agent.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, _ := agent.DefaultConfig("discord")
	cfg.Discord.TokenEnv = "NOTIFY_TEST_TOKEN"
	cfg.Discord.BaseURL = server.URL + "/api/v10"
	cfg.Discord.ChannelID = "ignored"
	if err := agent.SaveConfig(root, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"notify", "chan42", "all", "done", "--root", root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if gotPath != "/api/v10/channels/chan42/messages" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["content"] != "all done" {
		t.Fatalf("content = %q, want 'all done'", gotBody["content"])
	}
}

func TestNotifySubcommandRequiresArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"notify"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/claude-cron/ -run TestNotifySubcommand`
Expected: FAIL — `notify` not handled.

- [ ] **Step 3: Implement**

In `main.go` `switch args[0]`, add:
```go
	case "notify":
		return runNotifyCommand(args[1:], stdout, stderr)
```
Add the function (ensure `context`, `fmt`, `io`, `os`, `path/filepath`, `strings`, and the `agent` alias are imported):
```go
func runNotifyCommand(rest []string, stdout, stderr io.Writer) int {
	root := ".channel-agent"
	var pos []string
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--root" {
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--root requires a value")
				return 2
			}
			root = rest[i+1]
			i++
			continue
		}
		pos = append(pos, rest[i])
	}
	if len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: claude-cron notify <channel-id> <text...> [--root <root>]")
		return 2
	}
	channelID := pos[0]
	text := strings.Join(pos[1:], " ")

	if absRoot, err := filepath.Abs(root); err == nil {
		root = absRoot
	}
	cfg, err := agent.LoadConfig(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sender := agent.DiscordSender{
		BaseURL:   cfg.Discord.BaseURL,
		Token:     os.Getenv(cfg.Discord.TokenEnv),
		ChannelID: channelID,
	}
	if err := sender.Send(context.Background(), agent.OutputJob{Send: true, Text: text}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./cmd/claude-cron/ -run TestNotifySubcommand`
Expected: PASS. Then `go test ./... && go build ./...` green.

- [ ] **Step 5: Commit**

```bash
git add cmd/claude-cron/main.go cmd/claude-cron/main_test.go
git commit -m "feat: add notify subcommand to post a discord message from shell"
```

---

## Task 2: Teach agents via BuildClaudePrompt

**Files:**
- Modify: `internal/channelagent/adapters.go` (`BuildClaudePrompt`)
- Test: `internal/channelagent/adapters_test.go`

- [ ] **Step 1: Write the failing test** (append to `adapters_test.go`; it likely already tests BuildClaudePrompt — match existing style, add `strings` import if missing)

```go
func TestBuildClaudePromptTeachesNotify(t *testing.T) {
	job := InputJob{
		Schema:    1,
		JobID:     "j1",
		RequestID: "r1",
		InputHash: "h1",
		Source:    SourceMessage{Platform: "discord", ChannelID: "chan99", Content: "hi"},
	}
	p := BuildClaudePrompt(".channel-agent", job, ".channel-agent/outbox/pending/j1.json")
	if !strings.Contains(p, "claude-cron notify") {
		t.Fatalf("prompt should teach notify:\n%s", p)
	}
	if !strings.Contains(p, "chan99") {
		t.Fatalf("prompt should include the job's channel id:\n%s", p)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/channelagent/ -run TestBuildClaudePromptTeachesNotify`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `BuildClaudePrompt` (adapters.go), after the existing absolute-path resolution of `root` and before/within the `fmt.Sprintf`, append a notify instruction that uses `job.Source.ChannelID` and the absolute `root`. Concretely, add a trailing paragraph to the prompt format string and pass the channel id + root as args. For example change the final instruction block to include:
```
若你啟動了長時間的背景任務（detached，例如安裝、編譯），請在指令鏈最後加上：
&& claude-cron notify %s "完成訊息" --root %s
這樣任務跑完會自動通知使用者；不要為了等它而卡住這次回覆。
```
where the first `%s` is `job.Source.ChannelID` and the second is the absolute `root`. Add these two args to the existing `fmt.Sprintf(...)` argument list (after the current ones) and add the paragraph to the format string. Keep all existing instructions intact.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/channelagent/ -run TestBuildClaudePromptTeachesNotify`
Expected: PASS. Then full `go test ./... && go build ./...` green (existing BuildClaudePrompt tests must still pass — if one asserts exact prompt text, update it to match the new trailing paragraph).

- [ ] **Step 5: Commit**

```bash
git add internal/channelagent/adapters.go internal/channelagent/adapters_test.go
git commit -m "feat: teach agents to use claude-cron notify for long background tasks"
```

---

## Self-Review
- Spec coverage: `notify` subcommand → Task 1; teach agents via BuildClaudePrompt → Task 2. ✓
- Placeholder scan: full code in Task 1; Task 2 step 3 describes the exact edit with the format-string paragraph and the two `%s` args — implementer must wire them into the existing Sprintf (the only step that edits an existing multi-arg format string). ✓
- Type consistency: uses `agent.DiscordSender{BaseURL,Token,ChannelID}`, `agent.OutputJob{Send,Text}`, `agent.LoadConfig`, `BuildClaudePrompt(root, job, outputPath)`, `job.Source.ChannelID` — all match existing signatures. ✓
