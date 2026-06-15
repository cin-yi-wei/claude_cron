# Claude Cron

Claude Cron watches Discord or Telegram messages, injects each message into a long-running Claude Code tmux session, waits for Claude to write a reply file, then sends the verified reply back.

It is built as a single CLI binary. Users do not need Go unless they want to build from source.

## Install From GitHub Release

Pick the binary for your platform from the latest GitHub release:

```text
claude-cron-linux-amd64
claude-cron-linux-arm64
claude-cron-darwin-amd64
claude-cron-darwin-arm64
claude-cron-windows-amd64.exe
```

Linux:

```bash
curl -L -o claude-cron https://github.com/cin-yi-wei/claude_cron/releases/latest/download/claude-cron-linux-amd64
chmod +x claude-cron
sudo mv claude-cron /usr/local/bin/
claude-cron version
```

macOS Apple Silicon:

```bash
curl -L -o claude-cron https://github.com/cin-yi-wei/claude_cron/releases/latest/download/claude-cron-darwin-arm64
chmod +x claude-cron
sudo mv claude-cron /usr/local/bin/
claude-cron version
```

macOS Intel:

```bash
curl -L -o claude-cron https://github.com/cin-yi-wei/claude_cron/releases/latest/download/claude-cron-darwin-amd64
chmod +x claude-cron
sudo mv claude-cron /usr/local/bin/
claude-cron version
```

Windows PowerShell:

```powershell
Invoke-WebRequest -Uri "https://github.com/cin-yi-wei/claude_cron/releases/latest/download/claude-cron-windows-amd64.exe" -OutFile "$env:USERPROFILE\claude-cron.exe"
& "$env:USERPROFILE\claude-cron.exe" version
```

## Requirements

- Claude Code CLI installed and logged in.
- `tmux` installed on Linux/macOS.
- A Discord bot token or Telegram bot token.

First-time Claude Code login may still require opening Claude manually once:

```bash
claude
```

After that, `claude-cron serve` can auto-start the configured tmux session.

## Quick Start: Discord

```bash
export DISCORD_BOT_TOKEN=...

claude-cron init discord \
  --root .channel-agent \
  --discord-channel-id 1234567890

claude-cron doctor --root .channel-agent
claude-cron serve --root .channel-agent
```

## Quick Start: Telegram

```bash
export TELEGRAM_BOT_TOKEN=...

claude-cron init telegram \
  --root .channel-agent \
  --telegram-chat-id 1234567890

claude-cron doctor --root .channel-agent
claude-cron serve --root .channel-agent
```

## Useful Commands

Run one cycle:

```bash
claude-cron serve --root .channel-agent --once
```

Use mock input:

```bash
claude-cron init mock --root .channel-agent
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

Then:

```bash
claude-cron serve --root .channel-agent --once
```

Advanced manual commands remain available:

```bash
claude-cron watcher --root .channel-agent --source-adapter discord --discord-channel-id 1234567890
claude-cron claude-worker --root .channel-agent --tmux-session claude-cron --timeout 120s
claude-cron sender --root .channel-agent --adapter discord --discord-channel-id 1234567890
```

## Build From Source

Install Go, then:

```bash
make test
make build
./bin/claude-cron version
```

Install into your Go bin:

```bash
make install
```

Build release binaries for Linux, macOS, and Windows:

```bash
make dist
ls dist/
```

## Release

Create and push a tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

GitHub Actions builds release assets for:

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64
- Windows amd64

## Safety Rules

- `watcher` owns `inbox/pending` and `state/seen_message_ids.json`.
- Claude Code only reads `.channel-agent/current_job.json` and writes `outbox/pending/<job_id>.json`.
- `claude-worker` validates `job_id`, `request_id`, and `input_hash` before marking input jobs done.
- `sender` records output hashes only after successful sends.
- `send=false` outputs are marked handled without calling the sender adapter.
