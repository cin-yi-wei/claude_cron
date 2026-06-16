# Control Channel AI Assistant Design

**Date:** 2026-06-16
**Status:** Approved (design)

## Problem

The `#claude_cron` control channel currently only reacts to `/` commands; any
other message is silently ignored. The user wants to also *talk* to the control
channel — ask what it can do, and tell it to do things ("create a folder",
"bind project X") in natural language. The deterministic command parser should
stay, but free-text messages should be handled by an AI assistant that can act.

## Decisions (from brainstorming)

| Topic | Decision |
|---|---|
| Control channel behavior | Hybrid: `/commands` stay deterministic; free text → AI assistant |
| AI working directory | Dedicated sandbox `<root>/control-workspace` |
| AI scope | May also execute management (bind/unbind/list), not just advise |

## Architecture

Each supervisor poll, the control channel is processed as a hybrid:

```
RunControlOnce (control channel):
  fetch new messages (dedup via state/control_seen.json)
  for each new message:
    /command   → HandleCommand (deterministic) → reply           (existing)
    free text  → write an InputJob into the control binding's inbox  (NEW)

Supervisor, after RunControlOnce, runs the control binding's worker+sender
(NOT a watcher — RunControlOnce already enqueued the chat jobs):
  RunWorkerOnce(controlRoot, cc-control injector) → RunSenderOnce(controlRoot, control-channel sender)

control binding (reserved, not a registry entry):
  session   : cc-control   (claude launched with an --append-system-prompt and DISCORD_BOT_TOKEN in env)
  workspace : <root>/control-workspace   (the agent's cwd / sandbox)
  root      : <root>/control             (inbox/outbox/state/locks)
```

The control AI executes management by running the new `claude-cron` CLI
subcommands from its Bash tool; those subcommands reuse the exact same
`HandleCommand`/`ControlDeps` logic as the `/` commands.

## Components

### 1. Management CLI subcommands (`cmd/claude-cron/main.go`)
New subcommands that share logic with the channel parser:
- `claude-cron bind <name> <project-dir> <branch> [--root <root>]`
- `claude-cron unbind <name> [--delete-channel] [--root <root>]`
- `claude-cron list [--root <root>]`

Each: `LoadConfig(root)` → build a real `ControlDeps` (DiscordAdmin from
`os.Getenv(cfg.Discord.TokenEnv)` + `cfg.Discord.GuildID`, real worktree/tmux
funcs) → `LoadRegistry` → construct the corresponding `Command` →
`HandleCommand(ctx, deps, &reg, cmd)` → if changed, `SaveRegistry` → print the
reply to stdout, exit non-zero on error. `--root` defaults to `.channel-agent`.

This means the AI's `claude-cron bind ...` does exactly what `/bind` does.

### 2. Control binding helpers (`internal/channelagent/control.go`)
- `ControlBinding(root string) Binding` — reserved binding:
  `Name:"control"`, `TmuxSession:"cc-control"`, `Root:<root>/control`,
  `Worktree:<root>/control-workspace` (used as the session cwd; no git worktree
  is created for it), `ProjectDir:""`.
- `controlSystemPrompt(root, workspace string) string` — the append-system-prompt
  text: explains the assistant is the claude_cron control assistant, its
  workspace is `<workspace>`, and that it manages bindings by running
  `claude-cron bind/unbind/list --root <root>` (absolute root). It also states
  the reply must follow the existing outbox-JSON contract (the agent is driven
  by the same `BuildClaudePrompt`).

### 3. Free-text routing in `RunControlOnce` (`control.go`)
Change the non-command branch from "ignore" to "enqueue a job into the control
binding's inbox". Build the `InputJob` exactly as the watcher does
(`HashSource`, `buildJobID`, `buildRequestID`) and `AtomicWriteJSON` to
`<controlRoot>/inbox/pending/<jobID>.json`. The message is still marked seen.
`RunControlOnce` gains a `controlRoot string` parameter (the supervisor passes
`ControlBinding(root).Root`).

### 4. Control session startup (`internal/channelagent/worktree.go`)
- `StartControlSession(ctx, session, cwd, token, systemPrompt string) error` —
  like `StartTmuxClaude` but launches
  `claude --append-system-prompt <systemPrompt>` with `DISCORD_BOT_TOKEN`
  (the configured token-env name) injected via `tmux new-session -e NAME=value`,
  so the AI's `claude-cron` calls can authenticate. Also `EnsureAgentSettings(cwd)`.

### 5. Supervisor wiring (`supervisor.go`)
After `RunControlOnce`:
- Ensure control workspace dir exists (`os.MkdirAll`), `Init(controlRoot)`.
- `StartControlSession(cc-control, control-workspace, token, controlSystemPrompt)`.
- `RunWorkerOnce(ctx, controlRoot, TmuxInjector{Session:"cc-control", Root:controlRoot, AutoStart:false}, timeout)`.
- `RunSenderOnce(ctx, controlRoot, DiscordSender{... ChannelID: control channel})`.
Control errors are logged and do not abort the per-binding loop that follows.

## Data flow (free-text message)

```
user types "幫我在 workspace 建一個 logs 資料夾" in #claude_cron
 → RunControlOnce: not a /command → InputJob written to <root>/control/inbox/pending
 → RunWorkerOnce: inject prompt into cc-control (agent runs `mkdir logs` in workspace,
   writes reply JSON {send:true,text:"已建好 logs/"})
 → RunSenderOnce: posts the reply to #claude_cron

user types "綁定 myproj /home/u/p dev"
 → free text → cc-control agent runs `claude-cron bind myproj /home/u/p dev --root <abs>`
   → that reuses HandleCommand → creates channel/worktree/session, updates registry
 → agent replies "已綁定 myproj"
```

## Error handling

- A `/command` and a free-text message in the same poll are both handled; order
  preserved by the existing CreatedAt sort.
- Free-text job creation failure is logged; the message stays unseen only if the
  enqueue fails before marking seen (enqueue happens before mark-seen, mirroring
  the failed-command retry semantics).
- Control worker/sender errors are logged; they never stop the registry-binding
  loop.
- CLI subcommands print the `HandleCommand` reply and exit non-zero on error so
  the AI sees failures.

## Security (explicit)

The control AI has Bash, runs management commands, and has the bot token in its
environment. Anyone who can post in the control channel can make the AI run
shell commands on this machine and create/delete bindings. This is the accepted
trade-off of the "AI may execute management" decision. The control channel
should be restricted to trusted users via Discord channel permissions.

## Testing

- `control_test.go`: `RunControlOnce` routes a `/command` to `HandleCommand` AND
  a free-text message to a job file in the control inbox (assert the job exists
  with the right content); a second poll does not re-enqueue (dedup).
- `cmd/claude-cron/main_test.go`: `bind`/`list` subcommands invoke the shared
  handler — run `list` against a temp root with a seeded registry and assert
  stdout lists the binding; `bind` with a bad name exits non-zero with the
  usage/error text (mock the Discord/worktree side effects via the existing
  `runExternalCommand` override where reachable, or assert the validation-only
  path that needs no network).
- `worktree_test.go`: `StartControlSession` issues `tmux new-session ... -e
  DISCORD_BOT_TOKEN=... claude --append-system-prompt ...` (assert args via the
  `runExternalCommand` mock).
- `ControlBinding` derivation unit test (session/root/workspace fields).

## Out of scope (YAGNI)

- Telegram control AI (Discord only for now).
- Restricting the AI's Bash to the workspace (it can `cd` out; documented risk).
- Persisting control-AI conversation memory beyond what the long-lived cc-control
  session already retains.
- Natural-language parsing in Go — interpretation is the AI's job, not the
  supervisor's.
