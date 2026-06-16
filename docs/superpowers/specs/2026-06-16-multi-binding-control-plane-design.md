# Multi-Binding Control Plane Design

**Date:** 2026-06-16
**Status:** Approved (design)

## Problem

Today `claude-cron serve` handles exactly one Discord channel bound to one tmux
Claude Code session, configured statically in `config.json`. A user juggling
many projects or tickets cannot run them all in a single Claude session, and
running one `serve` process per project by hand is unmanageable.

We want a **control channel** (`#claude_cron`) from which the user dispatches and
manages many `Discord channel ↔ tmux Claude session` bindings — one per
project/ticket — each isolated on its own git branch.

## Decisions (from brainstorming)

| Topic | Decision |
|---|---|
| How bindings are created | Command-based, issued in the control channel |
| Process model | Single supervisor process managing many bindings |
| Command interpretation | Built-in deterministic parser (not a Claude session) |
| Discord channel provisioning | Bot auto-creates the channel via Discord API |
| `/bind` parameters | `name`, `project-dir`, `branch` |
| Branch handling | `git worktree` isolation per binding |

## Architecture

One `claude-cron serve` process (the **supervisor**) owns a **binding registry**
and on each poll interval iterates over all bindings:

```
claude-cron serve  (single supervisor process)
│
├─ registry: <root>/bindings.json        # all bindings, persisted
│
├─ [control binding]  #claude_cron channel
│     each poll: read new messages → parse commands → execute → reply
│     commands: /bind  /unbind  /list  /status  /help
│
└─ for each [project binding] each poll:
      watcher → worker → sender   (existing single-binding pipeline, scoped)
        ├─ channel_id   : auto-created Discord channel
        ├─ tmux_session : cc-<name>           (runs `claude` in the worktree)
        ├─ worktree     : <root>/worktrees/<name>  (checkout of <branch>)
        └─ runtime root : <root>/bindings/<name>/{inbox,outbox,state,locks,current_job.json}
```

A single Discord bot token is shared across all channels. Each binding's
`DiscordSource`/`DiscordSender` uses that token plus its own `channel_id`.

### Binding record (one entry in `bindings.json`)

```json
{
  "name": "proj-a",
  "channel_id": "1516...",
  "project_dir": "/home/conray/proj/a",
  "branch": "ticket-123",
  "worktree": "/home/conray/project/claude_cron/.channel-agent/worktrees/proj-a",
  "tmux_session": "cc-proj-a",
  "root": "/home/conray/project/claude_cron/.channel-agent/bindings/proj-a",
  "created_at": "2026-06-16T10:00:00Z"
}
```

`name` is the key: it derives the channel name, tmux session (`cc-<name>`),
worktree dir, and runtime root. Names are restricted to `[a-z0-9-]+` so they are
safe as Discord channel names, tmux session names, and path segments.

## Configuration

The existing `config.json` becomes the **control config**:

```json
{
  "platform": "discord",
  "poll_interval": "10s",
  "discord": {
    "token_env": "DISCORD_BOT_TOKEN",
    "channel_id": "<control channel id>",
    "guild_id": "<guild id>"
  },
  "claude": { "timeout": "120s", "auto_start": true }
}
```

New field: `discord.guild_id` — required for the bot to create channels.
`discord.channel_id` now means the **control** channel.

## Components

### 1. Registry (`registry.go`)
- `LoadRegistry(root) ([]Binding, error)` / `SaveRegistry(root, []Binding) error`
- Atomic write (reuse `AtomicWriteJSON`). Helpers: `Add`, `Remove(name)`,
  `Get(name)`, `Names()`. Duplicate-name add is an error.

### 2. Control command processor (`control.go`)
- Polls the control channel via the existing `DiscordSource` (reusing the
  bot-message skip filter so it ignores its own replies).
- Parses each new message. Only messages beginning with `/` are commands; other
  messages are ignored (a short usage hint is posted for unknown `/` commands).
- Commands:
  - `/bind <name> <project-dir> <branch>` — provision a binding (see flow below).
  - `/unbind <name>` — tear down a binding. Stops the tmux session, runs
    `git worktree remove`, deletes the runtime root, removes the registry entry.
    The Discord channel is **kept by default**; pass `--delete-channel` to also
    delete it.
  - `/list` — list all bindings with live status.
  - `/status <name>` — session alive?, queue depths, last activity for one binding.
  - `/help` — usage.
- Each command posts a result message back to the control channel.

### 3. Discord channel admin (extend `discord.go`)
- `CreateChannel(guildID, name) (channelID string, err error)` — `POST
  /guilds/{guild}/channels` (type 0, text). Returns the new channel id.
- `DeleteChannel(channelID) error` — `DELETE /channels/{id}` (only used by
  `/unbind --delete-channel`).

### 4. Worktree manager (`worktree.go`)
- `EnsureWorktree(projectDir, branch, worktreePath) error` — if `worktreePath`
  is missing, `git -C <projectDir> worktree add <worktreePath> <branch>`
  (creating the branch from current HEAD if it does not exist). Idempotent.
- `RemoveWorktree(projectDir, worktreePath) error` — `git -C <projectDir>
  worktree remove --force <worktreePath>`.

### 5. Supervisor loop (rework `serve` entrypoint)
- Load control config + registry.
- On startup, **reconcile**: for every binding, ensure its worktree exists and
  its tmux session is running (recreate missing sessions; existing worktrees are
  reused). This makes restart safe.
- Each poll interval:
  1. Run the control processor against the control channel.
  2. For each project binding, run `RunServeOnce` scoped to that binding
     (its own root, channel, tmux session). Errors in one binding are logged and
     do not stop the others.

`RunServeOnce` is unchanged in spirit; it is parameterized so a binding supplies
its own root, source (channel), injector (tmux session), and sender (channel).

## `/bind` data flow

```
/bind proj-a /home/conray/proj/a ticket-123
  1. validate name [a-z0-9-]+, project-dir is a git repo, not already bound
  2. CreateChannel(guild, "proj-a")            → channel_id
  3. EnsureWorktree(project-dir, ticket-123, <root>/worktrees/proj-a)
  4. Init(<root>/bindings/proj-a)              # inbox/outbox/state/locks
  5. tmux new-session -d -s cc-proj-a (cwd = worktree) "claude"
  6. registry.Add(binding); SaveRegistry
  7. reply in control channel: "✅ bound proj-a → #proj-a (branch ticket-123)"
  (on any step failure: roll back prior steps, reply with the error)
```

## Per-binding message flow (unchanged pipeline)

Discord channel poll → job written to that binding's `inbox/pending` → worker
injects the binding's tmux session (single-line prompt, `C-c` clear, `-l` paste,
Enter, `TMUX` stripped, absolute paths — as already implemented) → agent writes
`outbox/pending/<job>.json` with `send:true,text:...` → sender posts to that
channel and moves the file to `outbox/sent`. Each binding's runtime root is
isolated, so concurrent bindings never collide on queues, locks, or
`current_job.json`.

## Error handling

- Invalid command / args → usage hint in the control channel; no state change.
- `/bind` partial failure → roll back created channel/worktree/session/dir so a
  retry is clean.
- A project binding that errors during a poll cycle is logged and skipped; other
  bindings and the control channel keep working.
- Stale `processing/` jobs are recovered per binding by the existing
  `requeueProcessing` logic at the start of each worker cycle.

## Testing

- `registry_test.go`: add/remove/get/duplicate, atomic round-trip.
- `control_test.go`: command parsing (valid/invalid/unknown), and `/bind`
  orchestration with mocked channel-create, worktree, and tmux calls (reuse the
  `runExternalCommand` override pattern); assert registry + reply.
- `worktree_test.go`: EnsureWorktree/RemoveWorktree command construction via the
  `runExternalCommand` mock (idempotent when path exists).
- `discord_test.go`: CreateChannel/DeleteChannel request shape via `httptest`.
- Supervisor reconcile: a unit test that, given a registry with one binding and
  a missing session, issues the expected `tmux new-session` and worktree calls.

## Out of scope (YAGNI)

- Natural-language control commands (deterministic parser only).
- Telegram control plane (Discord first; the binding model is platform-agnostic
  and can extend later).
- Per-binding access control / permissions.
- Auto-deleting Discord channels on `/unbind` unless `--delete-channel` is given.
```
