#!/usr/bin/env bash
# DEPRECATED (2026-06-18): serve is now a systemd --user service that also hosts
# the admin API in-process — see ~/.config/systemd/user/claude-cron-serve.service
# (Type=simple, Restart=always). systemd starts it on boot + auto-restarts it, so
# the old "run serve+admin inside tmux sessions" approach is gone.
#
# This script is kept only as a manual "bring it up" helper; it now just (re)starts
# the systemd service rather than spawning tmux sessions. The cc-* Claude TUI
# sessions are created/managed by serve itself (StartTmuxClaude + auto-resume).
set -euo pipefail

systemctl --user daemon-reload
systemctl --user enable --now claude-cron-serve.service
systemctl --user status claude-cron-serve.service --no-pager | head -4
