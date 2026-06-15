# Channel Agent Queue Design

## Goal

Build a local queue-based automation flow that reads channel messages, turns them into deterministic jobs, asks a long-running Claude Code interactive session to produce replies, and sends verified replies back through a sender adapter.

The first implementation uses Go and a local mock input/output path. Real chat platform adapters can be added after the queue and Claude Code integration are stable.

## Architecture

The system is split into three deterministic processes:

- `watcher`: reads a source adapter and creates input jobs.
- `claude-worker`: claims one pending job, injects a narrow prompt into an interactive Claude Code session, waits for a matching output job, and validates it.
- `sender`: sends verified outbox jobs and records successful sends.

Claude Code only reads the current job and writes an output job. It does not update state files, decide job deduplication, compare hashes, or send messages.

## Runtime Layout

```text
.channel-agent/
  mock/source_messages.json

  inbox/
    pending/
    processing/
    done/
    failed/

  outbox/
    pending/
    sent/
    failed/

  state/
    seen_message_ids.json
    last_out_hashes.json

  locks/
    watcher.lock
    claude.lock
    sender.lock

  logs/
    watcher.log
    claude_inject.log
    sender.log

  current_job.json
```

All important files are written as `*.tmp`, fsynced, closed, then renamed into place.

## Input Jobs

One source message becomes one job file:

```text
inbox/pending/<created_at>-<message_id>-<input_hash>.json
```

Input job schema:

```json
{
  "schema": 1,
  "job_id": "20260616T013012Z-abc123-9f2a...",
  "request_id": "01J...",
  "input_hash": "9f2a...",
  "source": {
    "platform": "mock",
    "channel_id": "local",
    "message_id": "abc123",
    "author_id": "user",
    "created_at": "2026-06-16T01:30:12+08:00",
    "content": "hello",
    "attachments": []
  },
  "attempt": 0,
  "created_at": "2026-06-16T01:30:12+08:00"
}
```

`input_hash` is sha256 over canonical JSON for the normalized source payload, not just message text.

## Output Jobs

Claude Code writes:

```text
outbox/pending/<job_id>.tmp
outbox/pending/<job_id>.json
```

Output job schema:

```json
{
  "schema": 1,
  "job_id": "20260616T013012Z-abc123-9f2a...",
  "request_id": "01J...",
  "input_hash": "9f2a...",
  "send": true,
  "text": "reply"
}
```

The worker validates `job_id`, `request_id`, and `input_hash` against the claimed input job before moving the input job to `done`.

## State Rules

- `seen_message_ids.json` prevents watcher from creating duplicate jobs.
- `last_out_hashes.json` prevents sender from sending the same output twice.
- `send=false` output is recorded as handled and moved to `outbox/sent/`, but no message is sent.
- Failed sends do not update `last_out_hashes.json`.
- Failed or timed-out Claude jobs move to `inbox/failed/` for inspection in the first version.

## Locks

- `watcher.lock`: one watcher writes inbox/state at a time.
- `claude.lock`: one Claude job runs at a time.
- `sender.lock`: one sender sends outbox jobs at a time.

Locks are implemented by creating lock files with `O_CREATE|O_EXCL`. Stale lock recovery is intentionally omitted from the first version.

## Claude Injection

The prompt is fixed and narrow:

```text
請讀取 .channel-agent/current_job.json。

根據目前 Claude Code session / project context，分析裡面的新對話內容。

請將要回覆使用者的 JSON 寫入：
.channel-agent/outbox/pending/<job_id>.tmp

完成後 rename 成：
.channel-agent/outbox/pending/<job_id>.json

JSON 必須包含 schema, job_id, request_id, input_hash, send, text。

不要發送訊息。
不要修改 .channel-agent/state。
不要移動 inbox/outbox job。
不要做 hash 判斷。
```

The Go worker injects this prompt through a pluggable injector. The first concrete injector uses `tmux send-keys`. Tests use a fake injector.

## CLI Shape

```text
channel-agent init
channel-agent watcher --root .channel-agent --source .channel-agent/mock/source_messages.json
channel-agent claude-worker --root .channel-agent --tmux-session channel-agent --timeout 120s
channel-agent sender --root .channel-agent --adapter stdout
```

## Testing

Initial tests cover:

- canonical JSON hashing is stable.
- atomic write does not replace final file on failed temp writes.
- watcher creates one job per unseen message.
- watcher does not duplicate seen messages.
- worker rejects output with mismatched `input_hash`.
- worker moves valid jobs from processing to done.
- sender skips already sent output hashes.
- sender records output hash only after successful send.
