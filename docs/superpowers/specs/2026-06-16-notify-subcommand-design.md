# `notify` Subcommand Design

**Date:** 2026-06-16
**Status:** Approved (design)

## Problem

Binding/control agents are request-response: after kicking off a long detached
background task they go idle and cannot proactively tell the user when it
finishes. We need a way for a detached shell task to post a Discord message at
completion, decoupled from the agent's lifecycle.

## Decision

Add a thin `claude-cron notify` CLI subcommand that posts a message to a Discord
channel using the existing `DiscordSender`, and teach agents (via the per-job
prompt) to end long background tasks with it.

## Components

### 1. `notify` subcommand (`cmd/claude-cron/main.go`)
```
claude-cron notify <channel-id> <text...> [--root <root>]
```
- Parse: first positional = channel id; remaining positionals joined by space = text; `--root` (default `.channel-agent`).
- `LoadConfig(root)` → token from `os.Getenv(cfg.Discord.TokenEnv)`.
- `DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: channelID}.Send(ctx, OutputJob{Send: true, Text: text})`.
- Print nothing on success, exit 0; on error print to stderr, exit 1.
- Missing channel id or empty text → usage error, exit 2.

### 2. Teach agents (`internal/channelagent/adapters.go`, `BuildClaudePrompt`)
`BuildClaudePrompt(root, job, outputPath)` already has `root` (resolved to
absolute) and `job.Source.ChannelID`. Append one instruction block telling the
agent: for a long-running detached background task, end the command chain with
`claude-cron notify <job.Source.ChannelID> "<message>" --root <absRoot>` so the
user is notified on completion. This reaches both binding and control agents
since both drive jobs through `BuildClaudePrompt`.

## Error handling
- `notify` with a bad/unreachable channel → DiscordSender returns the HTTP error → exit 1.
- No token configured → Send fails with "discord token is required" → exit 1.

## Testing
- `cmd/claude-cron/main_test.go`: `notify` posts to the right channel with the
  right body — point `DiscordSender` at an `httptest` server by setting
  `cfg.Discord.BaseURL` in a saved config under a temp root; assert the captured
  POST path/body. Also: missing args → exit 2.
- `internal/channelagent/adapters_test.go`: `BuildClaudePrompt` output contains
  `claude-cron notify` and the job's channel id.

## Out of scope
- Proactive scheduling/cron (option 1) and agent-held notify tool (option 2).
- Attachments/images via notify (text only).
