# claude_cron — 2026-06-17 status & TODO

## Shipped today (master, deployed v0.1.0-67)
- Ingestion as **platform × mode** (discord/telegram × poll/push) behind an `Ingester` seam.
- **dc-push** (Discord Gateway websocket) — live-verified (calc on push; real MESSAGE_CREATE captured).
- **tg-poll** / **tg-push** (webhook + setWebhook) — tg-push only locally verified, never run against a real Telegram bot.
- **Control channel** Gateway + always-on poll backstop (lifeline can't drop).
- **Worktree sibling layout** — worktrees live next to the project repo, not under `.channel-agent`.
- **Orphan-session reap** — supervisor kills `cc-*` sessions with no binding each cycle (fixes unbind/serve race).
- **Permission gate** — PreToolUse hook routes risky Bash (install/download/sudo/destructive) + MCP to the channel for y/n; ordinary Bash + Read/Write/Edit auto-allowed. Hook contract validated.
- **Inject verify+retry** — after submit, check the input box cleared; re-run the full C-c→type→pause→Enter recipe (≤3×) if the Enter dropped. Submit delay 800→1200ms.
- **Admin backend** phases 0-4 (read API / bearer auth / write API bind·unbind·restart / observability / minimal UI).
- Fixes: notify auto-loads `.env`; notify/manage resolve `config.json` by walking up.

## Open / to decide
1. **tg-push real run** — needs a Telegram bot token + a public hook URL; only local-tested so far.
2. **Admin backend not permanently running** — the demo was a temp process (now dead). Productionize: run as a service + set `admin.token`.
3. **Permission gate memory** — risky commands ask every time; add "remember/allow for this session".
4. **`unbind` does not actually remove the worktree** — `RemoveWorktree` errors are swallowed, leaving orphan worktree dirs + git worktree registrations + branches behind (found 5 today: fgtest, openobserve2-5; cleaned by hand via `git worktree remove --force` + `branch -d`). Fix: surface/verify removal in unbind; prune on cleanup.
5. **Backlog/robustness** — Gateway reconnect backoff; a single shared Gateway connection instead of one ws per push binding; Telegram long-polling.

## Done in this cleanup pass
- Pushed master to origin (through `aeb105d`).
- Deleted stale branches cc-test, openobserve2-5 and their orphan worktrees. Worktree list now only: fatgame-feat-whitelist-ip, fatgame-jfg-4512, fatgame-openobserve.
