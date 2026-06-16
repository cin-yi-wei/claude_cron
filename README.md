# Claude Cron

Claude Cron watches Discord or Telegram messages, injects each message into a long-running Claude Code tmux session, waits for Claude to write a reply file, then sends the verified reply back.

It also runs a **multi-project control plane**: a single `serve` supervisor can manage many `Discord channel ↔ tmux Claude session` bindings, dispatched from a control channel with `/bind`, `/unbind`, `/list`, and `/status` commands. Each binding is isolated on its own git worktree branch. The control channel is also an AI assistant — free-text messages are answered by a dedicated Claude session that can manage bindings and run shell tasks. See [Multi-Project Control Plane](#multi-project-control-plane).

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
- `tmux` installed on Linux/macOS (3.x; the control assistant uses `tmux new-session -e`).
- A Discord bot token or Telegram bot token.
- For the multi-project control plane (auto-creating channels): a Discord bot with the **Manage Channels** permission, and `discord.guild_id` set in the config.

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

## Multi-Project Control Plane

Run one `serve` against a control channel and dispatch many project bindings from it. Each binding gets its own Discord channel, its own tmux Claude session, and an isolated git worktree on a chosen branch.

Set up the control config (the `discord.channel_id` is the control channel; `discord.guild_id` is required so the bot can create channels):

```bash
export DISCORD_BOT_TOKEN=...

claude-cron init discord \
  --root .channel-agent \
  --discord-channel-id <control-channel-id>

# add "guild_id": "<server-id>" under "discord" in .channel-agent/config.json
claude-cron serve --root .channel-agent
```

Then, in the control channel, use slash commands:

```text
/bind <name> <project-dir> <branch>   # create a channel + worktree + session for a project
/unbind <name> [--delete-channel]     # tear it down (keeps the Discord channel unless --delete-channel)
/list                                  # list bindings
/status <name>                         # session + queue status for one binding
/help
```

`/bind myproj /home/me/proj feature-x` creates a Discord channel `myproj`, a git worktree of `/home/me/proj` on branch `feature-x` (created from HEAD if new), and a tmux session `cc-myproj` running Claude in that worktree. Messages posted in the new `#myproj` channel are answered by that project's Claude session.

The same actions are available as CLI subcommands (used by the control assistant, or directly):

```bash
claude-cron bind <name> <project-dir> <branch> --root .channel-agent
claude-cron unbind <name> [--delete-channel] --root .channel-agent
claude-cron list --root .channel-agent
```

### Control Channel AI Assistant

Non-command (free-text) messages in the control channel are handled by a dedicated `cc-control` Claude session running in `<root>/control-workspace`. Ask it questions, tell it to do things ("create a folder", "scaffold X"), or manage bindings in natural language (it runs the `claude-cron bind/...` CLI for you).

> Security: the control assistant has shell access and the bot token in its environment, and can create/delete bindings. Restrict the control channel to trusted users via Discord channel permissions.

### Notifying When Long Tasks Finish

Agents are request/response — after launching a long detached task they go idle and cannot proactively message you. Use `notify` to post to a channel from the shell, so a background task can report its own completion:

```bash
claude-cron notify <channel-id> "build finished ✅" --root .channel-agent
```

Agents are instructed (in their job prompt) to end long detached background tasks with `&& claude-cron notify <their-channel-id> "..." --root <root>` so you get pinged when the work is done.

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
