# Discord and Telegram Adapter Design

## Goal

Add Discord and Telegram adapters to Claude Cron so the watcher can fetch channel/chat messages and the sender can post Claude output back to the same platform.

## Scope

The first version supports text-first polling:

- Discord source: `GET /api/v10/channels/{channel_id}/messages`.
- Discord sender: `POST /api/v10/channels/{channel_id}/messages`.
- Telegram source: Bot API `getUpdates`.
- Telegram sender: Bot API `sendMessage`.

The implementation uses Go standard library HTTP clients and `httptest` tests. No real external messages are sent by tests.

## CLI

```bash
claude-cron watcher --source-adapter mock --source .channel-agent/mock/source_messages.json

claude-cron watcher --source-adapter discord \
  --discord-token-env DISCORD_BOT_TOKEN \
  --discord-channel-id 123

claude-cron watcher --source-adapter telegram \
  --telegram-token-env TELEGRAM_BOT_TOKEN \
  --telegram-chat-id 123

claude-cron sender --adapter stdout

claude-cron sender --adapter discord \
  --discord-token-env DISCORD_BOT_TOKEN \
  --discord-channel-id 123

claude-cron sender --adapter telegram \
  --telegram-token-env TELEGRAM_BOT_TOKEN \
  --telegram-chat-id 123
```

Tests may override base URLs with hidden flags so real APIs are not contacted.

## Source Interface

Add a `MessageSource` interface:

```go
type MessageSource interface {
    Fetch(ctx context.Context) ([]SourceMessage, error)
}
```

Existing mock JSON behavior becomes `MockFileSource`. `RunWatcher` remains as a compatibility wrapper and delegates to `RunWatcherWithSource`.

## Discord Mapping

Discord input fields:

- `platform`: `discord`
- `channel_id`: configured channel ID
- `message_id`: Discord message `id`
- `author_id`: `author.id`
- `created_at`: message `timestamp`
- `content`: message `content`
- `attachments`: attachment `id`, `url`, and `content_type`

Discord sender sends:

```json
{"content": "..."}
```

with:

```text
Authorization: Bot <token>
Content-Type: application/json
```

## Telegram Mapping

Telegram input fields:

- `platform`: `telegram`
- `channel_id`: configured chat ID
- `message_id`: `update_id` as a stable polling identifier
- `author_id`: `message.from.id`
- `created_at`: Unix message date formatted as RFC3339 UTC
- `content`: `message.text` or `message.caption`
- `attachments`: omitted in first version

Telegram sender calls `sendMessage` with `chat_id` and `text`.

## State

The current `seen_message_ids.json` keeps deduping all sources by:

```text
platform:channel_id:message_id
```

No separate Discord/TG offsets are needed for the first polling version because queue dedupe handles repeats.

## Error Handling

Adapters treat non-2xx responses as errors and include a short response body snippet. Missing token or channel/chat ID fails before making HTTP calls.

## References

- Discord Developer Documentation: `GET /channels/{channel.id}/messages`, `POST /channels/{channel.id}/messages`
- Telegram Bot API: `getUpdates`, `sendMessage`
