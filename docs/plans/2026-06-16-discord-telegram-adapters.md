# Discord Telegram Adapters Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add Discord and Telegram source/sender adapters to Claude Cron.

**Architecture:** Introduce a `MessageSource` interface and keep watcher queue logic independent of platforms. Implement Discord and Telegram adapters with standard-library HTTP clients and wire them into the existing CLI.

**Tech Stack:** Go standard library: `net/http`, `net/http/httptest`, `encoding/json`, `flag`, `os`, `time`, `testing`.

---

### Task 1: Source Interface

**Files:**
- Modify: `internal/channelagent/watcher.go`
- Create: `internal/channelagent/source.go`
- Test: `internal/channelagent/source_test.go`

**Steps:**
1. Write failing tests for `RunWatcherWithSource` using a fake source.
2. Run `go test ./internal/channelagent -run TestRunWatcherWithSource -v`.
3. Implement `MessageSource`, `MockFileSource`, and `RunWatcherWithSource`.
4. Re-run the targeted tests.

### Task 2: Discord Adapter

**Files:**
- Create: `internal/channelagent/discord.go`
- Test: `internal/channelagent/discord_test.go`

**Steps:**
1. Write failing `httptest` tests for fetching channel messages and sending a message.
2. Run `go test ./internal/channelagent -run TestDiscord -v`.
3. Implement Discord source and sender.
4. Re-run the targeted tests.

### Task 3: Telegram Adapter

**Files:**
- Create: `internal/channelagent/telegram.go`
- Test: `internal/channelagent/telegram_test.go`

**Steps:**
1. Write failing `httptest` tests for `getUpdates` and `sendMessage`.
2. Run `go test ./internal/channelagent -run TestTelegram -v`.
3. Implement Telegram source and sender.
4. Re-run the targeted tests.

### Task 4: CLI Wiring

**Files:**
- Modify: `cmd/claude-cron/main.go`
- Modify: `cmd/claude-cron/main_test.go`
- Modify: `README.md`

**Steps:**
1. Write failing CLI tests for adapter flag dispatch.
2. Run `go test ./cmd/claude-cron -v`.
3. Wire `--source-adapter`, `--adapter`, token env flags, channel/chat flags, and hidden base URL flags.
4. Update README.
5. Re-run CLI tests.

### Task 5: Verification and Commit

**Steps:**
1. Run `gofmt -w cmd/claude-cron internal/channelagent`.
2. Run `go test ./...`.
3. Run `go build ./cmd/claude-cron`.
4. Remove generated binary if present.
5. Commit with `feat: add discord telegram adapters`.
