# Claude Cron

Local Go CLI for queueing channel messages, asking a long-running Claude Code interactive session to generate replies, and sending verified replies through an adapter.

## Build

```bash
go test ./...
go build ./cmd/claude-cron
```

## Local Mock Flow

Create runtime directories:

```bash
go run ./cmd/claude-cron init --root .channel-agent
```

Create runtime directories and config in one command:

```bash
go run ./cmd/claude-cron init discord --root .channel-agent --discord-channel-id 1234567890
go run ./cmd/claude-cron init telegram --root .channel-agent --telegram-chat-id 1234567890
```

Create `.channel-agent/mock/source_messages.json`:

```json
[
  {
    "platform": "mock",
    "channel_id": "local",
    "message_id": "m1",
    "author_id": "user",
    "created_at": "2026-06-16T01:30:12+08:00",
    "content": "hello",
    "attachments": []
  }
]
```

Create pending jobs:

```bash
go run ./cmd/claude-cron watcher --root .channel-agent --source .channel-agent/mock/source_messages.json
```

Discord watcher:

```bash
export DISCORD_BOT_TOKEN=...
go run ./cmd/claude-cron watcher \
  --root .channel-agent \
  --source-adapter discord \
  --discord-channel-id 1234567890
```

Telegram watcher:

```bash
export TELEGRAM_BOT_TOKEN=...
go run ./cmd/claude-cron watcher \
  --root .channel-agent \
  --source-adapter telegram \
  --telegram-chat-id 1234567890
```

Check config before serving:

```bash
go run ./cmd/claude-cron doctor --root .channel-agent
```

Run a Claude Code interactive session in tmux:

```bash
tmux new -s channel-agent
claude
```

In another shell, inject one pending job:

```bash
go run ./cmd/claude-cron claude-worker --root .channel-agent --tmux-session channel-agent --timeout 120s
```

Or run the whole watcher -> Claude -> sender loop:

```bash
go run ./cmd/claude-cron serve --root .channel-agent
```

For a single cycle:

```bash
go run ./cmd/claude-cron serve --root .channel-agent --once
```

`serve` auto-starts the configured tmux session with `claude` when needed. First-time Claude Code login may still require opening the session manually once.

Send pending outputs to stdout:

```bash
go run ./cmd/claude-cron sender --root .channel-agent --adapter stdout
```

Send pending outputs to Discord:

```bash
export DISCORD_BOT_TOKEN=...
go run ./cmd/claude-cron sender \
  --root .channel-agent \
  --adapter discord \
  --discord-channel-id 1234567890
```

Send pending outputs to Telegram:

```bash
export TELEGRAM_BOT_TOKEN=...
go run ./cmd/claude-cron sender \
  --root .channel-agent \
  --adapter telegram \
  --telegram-chat-id 1234567890
```

## Safety Rules

- `watcher` owns `inbox/pending` and `state/seen_message_ids.json`.
- Claude Code only reads `.channel-agent/current_job.json` and writes `outbox/pending/<job_id>.json`.
- `claude-worker` validates `job_id`, `request_id`, and `input_hash` before marking input jobs done.
- `sender` records output hashes only after successful sends.
- `send=false` outputs are marked handled without calling the sender adapter.
