# Channel Agent Queue Go Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a Go CLI that runs a local queue-based watcher, Claude Code interactive worker, and sender for channel message automation.

**Architecture:** Use one Go module with `cmd/channel-agent` for CLI entrypoints and `internal/channelagent` for deterministic queue, hashing, atomic file, lock, injector, and sender logic. Runtime state lives under `.channel-agent/`, with one message per input job and one reply per output job.

**Tech Stack:** Go standard library only for MVP: `encoding/json`, `crypto/sha256`, `os`, `filepath`, `os/exec`, `flag`, and `testing`.

---

### Task 1: Module and Core Types

**Files:**
- Create: `go.mod`
- Create: `internal/channelagent/types.go`
- Create: `internal/channelagent/hash.go`
- Test: `internal/channelagent/hash_test.go`

**Step 1: Write the failing test**

Create tests proving canonical JSON hashing is stable regardless of map key order and changes when source content changes.

**Step 2: Run test to verify it fails**

Run: `go test ./internal/channelagent -run TestCanonicalHash -v`

Expected: FAIL because package/functions do not exist.

**Step 3: Implement minimal code**

Create job/source/output structs and canonical hash helpers using `encoding/json` on concrete structs or maps with stable key ordering.

**Step 4: Run test to verify it passes**

Run: `go test ./internal/channelagent -run TestCanonicalHash -v`

Expected: PASS.

### Task 2: Atomic Files and Locks

**Files:**
- Create: `internal/channelagent/fileutil.go`
- Create: `internal/channelagent/lock.go`
- Test: `internal/channelagent/fileutil_test.go`
- Test: `internal/channelagent/lock_test.go`

**Step 1: Write failing tests**

Test `AtomicWriteJSON` creates the final file only after a successful write, and test lock acquisition rejects a second holder.

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/channelagent -run 'TestAtomic|TestLock' -v`

Expected: FAIL because helpers do not exist.

**Step 3: Implement minimal code**

Write `*.tmp`, fsync the file, close it, rename to final, and fsync the parent directory. Implement lock files with `O_CREATE|O_EXCL`.

**Step 4: Run tests**

Run: `go test ./internal/channelagent -run 'TestAtomic|TestLock' -v`

Expected: PASS.

### Task 3: Queue Directories and Watcher

**Files:**
- Create: `internal/channelagent/init.go`
- Create: `internal/channelagent/watcher.go`
- Test: `internal/channelagent/watcher_test.go`

**Step 1: Write failing tests**

Test init creates all queue directories. Test watcher reads mock messages, creates pending jobs, writes `seen_message_ids.json`, and skips duplicates on the second run.

**Step 2: Run tests**

Run: `go test ./internal/channelagent -run 'TestInit|TestWatcher' -v`

Expected: FAIL.

**Step 3: Implement minimal code**

Add `Init(root string)` and `RunWatcher(root, sourcePath string) error`.

**Step 4: Run tests**

Run: `go test ./internal/channelagent -run 'TestInit|TestWatcher' -v`

Expected: PASS.

### Task 4: Claude Worker Validation

**Files:**
- Create: `internal/channelagent/worker.go`
- Test: `internal/channelagent/worker_test.go`

**Step 1: Write failing tests**

Test worker claims oldest pending job, writes `current_job.json`, calls fake injector, rejects mismatched output, and accepts valid output by moving input job to `inbox/done`.

**Step 2: Run tests**

Run: `go test ./internal/channelagent -run TestWorker -v`

Expected: FAIL.

**Step 3: Implement minimal code**

Add `Injector` interface, fake-testable `RunWorkerOnce`, output validation, and job moves between `pending`, `processing`, `done`, and `failed`.

**Step 4: Run tests**

Run: `go test ./internal/channelagent -run TestWorker -v`

Expected: PASS.

### Task 5: Sender

**Files:**
- Create: `internal/channelagent/sender.go`
- Test: `internal/channelagent/sender_test.go`

**Step 1: Write failing tests**

Test sender sends unsent outputs, skips outputs whose canonical hash is already recorded, records hash only after success, and treats `send=false` as handled without calling the adapter.

**Step 2: Run tests**

Run: `go test ./internal/channelagent -run TestSender -v`

Expected: FAIL.

**Step 3: Implement minimal code**

Add `Sender` interface, stdout sender implementation, hash state update, and outbox moves.

**Step 4: Run tests**

Run: `go test ./internal/channelagent -run TestSender -v`

Expected: PASS.

### Task 6: CLI Entrypoint

**Files:**
- Create: `cmd/channel-agent/main.go`
- Test: `cmd/channel-agent/main_test.go`

**Step 1: Write failing tests**

Test CLI dispatch calls init/watcher/sender/worker command handlers with expected flags. Keep business logic in `internal/channelagent`.

**Step 2: Run tests**

Run: `go test ./cmd/channel-agent -v`

Expected: FAIL.

**Step 3: Implement minimal code**

Add subcommands: `init`, `watcher`, `claude-worker`, and `sender`.

**Step 4: Run tests**

Run: `go test ./cmd/channel-agent -v`

Expected: PASS.

### Task 7: End-to-End Local Flow

**Files:**
- Create: `internal/channelagent/e2e_test.go`
- Modify: `README-channel-agent.md`

**Step 1: Write failing test**

Test a local flow: init, watcher creates job from mock source, fake worker writes output, sender stdout adapter records send and moves output to sent.

**Step 2: Run test**

Run: `go test ./... -run TestLocalQueueFlow -v`

Expected: FAIL.

**Step 3: Implement missing integration glue**

Fix any path, state, or CLI glue issues found by the e2e test.

**Step 4: Run full verification**

Run: `go test ./...`

Expected: PASS.

### Task 8: Commit

**Files:**
- All files above.

**Step 1: Review diff**

Run: `git diff -- docs/plans go.mod internal cmd README-channel-agent.md`

**Step 2: Run verification**

Run: `go test ./...`

Expected: PASS.

**Step 3: Commit**

Run:

```bash
git add docs/plans/2026-06-16-channel-agent-queue-design.md docs/plans/2026-06-16-channel-agent-queue-go.md go.mod internal cmd README-channel-agent.md
git commit -m "feat: add channel agent queue"
```
