# Admin Backend — Staged Plan

Status: roadmap (not started). Goal: a web admin to manage many `Discord/Telegram channel ↔ Claude session` bindings, evolving the current control-channel-only management into a dashboard.

## Core principle

Do **not** reimplement logic. `bind`/`unbind`/`list`/`status` already live as pure functions in `HandleCommand` (`internal/channelagent/control.go`); the CLI and the Discord control channel both wrap them. The admin API wraps the same functions. The registry (`bindings` JSON) stays the single source of truth.

## Why the current architecture already fits

- Registry is JSON, one source of truth → the API reads it directly.
- `HandleCommand(bind/unbind/list/status)` is reusable as-is.
- State is file-based (`inbox/outbox/state`) → queue depth / failed counts already computed by `handleStatus`.
- `platform` × `mode` are first-class on each binding → dashboard can show/switch.
- A push (Telegram webhook) HTTP server already exists → an admin API can be co-hosted (but keep auth boundaries clear).

## Phases (each independently shippable, TDD, reversible)

### Phase 0 — read-only API (smallest, zero risk)
- `claude-cron admin` subcommand (or `serve --admin-listen`) starts an HTTP server.
- `GET /api/bindings` → list; `GET /api/bindings/{name}` → status (pending/processing/failed counts + tmux session alive?).
- Bind `127.0.0.1` only. Goal: prove the read path works and does not race the supervisor.

### Phase 1 — auth (hard prerequisite before any write)
- Bearer-token middleware; token from env/config. Refuse to start if unset or bound non-loopback without a token.
- This controls shell+token-capable sessions — running it unauthenticated/public is a disaster. Non-negotiable.

### Phase 2 — write API
- `POST /api/bindings` (bind), `DELETE /api/bindings/{name}` (unbind), `POST /api/bindings/{name}/restart`.
- Wrap `HandleCommand`. Add a registry write-lock: the supervisor reloads the registry each cycle, so atomic write + next-cycle pickup already works; the lock prevents API/supervisor clobbering.
- Audit log of admin actions (who/when/what).

### Phase 3 — observability
- Per-binding: recent messages, last error, session uptime.
- Live queue updates via SSE/websocket (optional).

### Phase 4 — Web UI
- SPA served by the same server: bindings table with `[platform/mode]` badges, queue depths, create/delete form, restart button.

## Cross-cutting concerns

- **Security first:** loopback by default; external access only behind a reverse proxy + strong auth. Tokens via env.
- **Concurrency:** registry operations need a lock, coordinated with the supervisor's per-cycle reload.
- **Deployment shape:** prefer the admin server as a **separate process** from `serve`, so a crash there does not stop message send/receive. (A goroutine inside `serve` is simpler but couples failure domains.)

## Suggested order

0 → 1 → 2 first (manage `dc-poll`, which is already verified and the common case). UI last. Defer anything depending on the not-yet-live-verified push cells.
