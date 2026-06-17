#!/usr/bin/env bash
# Boot launcher for claude_cron: (re)creates the serve + admin tmux sessions.
# Idempotent — safe to run repeatedly; skips a session that already exists.
# Invoked by the systemd --user unit on boot so the sessions survive reboot.
set -euo pipefail

REPO=/home/conray/project/claude_cron
cd "$REPO"
set -a
# shellcheck disable=SC1091
[ -f "$REPO/.env" ] && . "$REPO/.env"
set +a

start() { # name, command
  if ! tmux has-session -t "$1" 2>/dev/null; then
    tmux new-session -d -s "$1" "$2"
  fi
}

start cron-serve "exec claude-cron serve --root .channel-agent"
start admin-serve "exec claude-cron admin --root .channel-agent"
