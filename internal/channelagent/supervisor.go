package channelagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runControlAssistant ensures the control assistant workspace exists and drives
// one worker+sender cycle for any queued free-text control jobs. injector and
// sender are parameterized for testing; in production the supervisor passes a
// TmuxInjector bound to cc-control and a DiscordSender for the control channel.
func runControlAssistant(ctx context.Context, cb Binding, injector Injector, sender Sender, timeout time.Duration) error {
	if err := os.MkdirAll(cb.Worktree, 0o755); err != nil {
		return err
	}
	if err := Init(cb.Root); err != nil {
		return err
	}
	if _, err := RunWorkerOnce(ctx, cb.Root, injector, timeout); err != nil {
		return err
	}
	if _, err := RunSenderOnce(ctx, cb.Root, sender); err != nil {
		return err
	}
	return nil
}

// RunSupervisorOnce runs one supervisor cycle: process the control channel,
// then run the per-binding pipeline for every registered binding.
func RunSupervisorOnce(ctx context.Context, root string, cfg Config, timeout time.Duration, stdout io.Writer, push *PushManager) error {
	// Resolve root to an absolute path so derived binding worktree/root paths are
	// absolute. git resolves a relative worktree path against the project repo
	// (`git -C <repo>`), not this process's cwd, so a relative root would place
	// worktrees inside the wrong directory.
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	token := os.Getenv(cfg.Discord.TokenEnv)

	reg, err := LoadRegistry(root)
	if err != nil {
		return err
	}

	// Unified control model: seed the configured (hardcoded) control planes as
	// registry control bindings so they appear in the list and are manageable.
	// Each reuses the EXISTING control-<x> root/session/workspace + plane name, so
	// the registry control loop (below) runs it IDENTICALLY to the legacy
	// hardcoded path (which is guarded off once seeded). Discord is the bootstrap
	// lifeline → forced as the protected default.
	if seedControlPlanes(root, cfg, &reg) {
		if err := SaveRegistry(root, reg); err != nil {
			return err
		}
	}

	deps := BuildControlDeps(root, cfg)

	// activePush tracks every push ingester/webhook that should stay alive this
	// cycle, so Reconcile (at the end) doesn't cancel them. The control gateway and
	// the telegram demux webhook use reserved keys and must be included, or they'd
	// be killed+restarted every cycle.
	activePush := map[string]bool{}

	// Shared Telegram reader: one offset-advancing getUpdates for the bot token,
	// routing each message to its chat's destination — a poll binding's inbox or a
	// control plane's buffer. Runs before control + binding processing so their
	// inboxes/buffers are filled. Replaces per-consumer getUpdates (which 409'd and
	// re-fetched the 24h backlog). Push (webhook) bindings are fed separately.
	tgToken := os.Getenv(cfg.Telegram.TokenEnv)
	if tgToken != "" && cfg.TelegramTransport() != TransportWebhook {
		// Poll mode: single getUpdates reader, route by chat id. (Webhook mode feeds
		// the same routes via the demux handler below instead — they're exclusive.)
		routes := telegramRoutes(root, cfg, reg)
		if len(routes) > 0 {
			reader := TelegramReader{BaseURL: cfg.Telegram.BaseURL, Token: tgToken, OffsetPath: pathIn(root, "state", "tg_offset.json")}
			if err := reader.Drain(ctx, routes); err != nil {
				fmt.Fprintf(stdout, "telegram reader error: %v\n", err)
			}
		}
	}
	if tgToken != "" && cfg.TelegramTransport() == TransportWebhook && push != nil {
		// Webhook mode: one shared demux endpoint for the whole bot, routing by chat
		// id (reloads the registry per request). setWebhook runs once on first mount.
		h := TelegramDemuxHandler{Root: root, Cfg: cfg, Secret: cfg.Push.Secret}
		push.EnsureWebhook("__tg_demux__", cfg.Push.Listen, "/tg", h, func() error {
			if cfg.Push.PublicURL == "" {
				return nil
			}
			url := strings.TrimRight(cfg.Push.PublicURL, "/") + "/tg"
			if err := SetWebhook(ctx, cfg.Telegram.BaseURL, tgToken, url, cfg.Push.Secret, nil); err != nil {
				fmt.Fprintf(stdout, "telegram setWebhook error: %v\n", err)
				return err
			}
			return nil
		})
		activePush["__tg_demux__"] = true
	}

	// Single shared Discord Gateway demux (opt-in): one websocket for the whole
	// bot, routing each MESSAGE_CREATE by channel id — to the matching WORKER
	// binding's inbox, or (phase C) to a control plane's buffer. Resolved fresh per
	// message so new/removed/paused bindings are honoured. Replaces per-binding
	// poll/Gateway for workers AND the separate per-control Gateway.
	if cfg.DiscordTransport() == TransportGateway && token != "" && push != nil {
		dcRoute := func(ctx context.Context, msg SourceMessage) error {
			reg2, err := LoadRegistry(root)
			if err != nil {
				return nil // transient; next msg retries routing
			}
			// Worker bindings → their inbox. Control bindings are routed below.
			for _, b := range reg2.Bindings {
				if !b.Control && b.PlatformOf() == PlatformDiscord && !b.Paused && b.ChannelID == msg.ChannelID {
					_, e := IngestMessages(ctx, b.Root, []SourceMessage{msg})
					return e
				}
			}
			// Control bindings (registry-driven) → their control buffer, drained by
			// the control loop's BufferPollSource. Covers the seeded discord plane +
			// any dc control created via bind.
			for _, b := range reg2.Bindings {
				if b.Control && b.PlatformOf() == PlatformDiscord && !b.Paused && b.ChannelID == msg.ChannelID {
					return appendTelegramBuffer(pathIn(b.Root, "state", controlBufferName(PlatformDiscord)), msg)
				}
			}
			return nil // unknown channel → dropped
		}
		push.Ensure("__dc_demux__", DiscordGatewayIngester{Token: token, Route: dcRoute}, func(e error) {
			if e != nil {
				fmt.Fprintf(stdout, "discord gateway demux exited (restarts next cycle): %v\n", e)
			}
		})
		activePush["__dc_demux__"] = true
	}

	// Control planes (discord/telegram/web) are no longer hardcoded here: they are
	// registry control bindings (seeded above from config on first boot) and run
	// by the unified control-binding loop further down, exactly like web controls.

	for _, b := range reg.Bindings {
		// Control bindings are driven by the control-binding loop below, not the
		// worker pipeline (they have no project worktree).
		if b.Control {
			continue
		}
		// Paused (hot-stopped) bindings: don't recreate the session or ingest.
		// The session was killed on /pause; any stray copy is reaped below
		// (excluded from the valid set). Messages stay in the channel until
		// /resume (poll bindings backfill via the unadvanced cursor).
		if b.Paused {
			continue
		}
		// Auto-sleep/wake: a slept binding stays down until input arrives, then
		// wakes (clears the flag + falls through to recreate the session). An idle
		// binding with no queued input is slept (session killed to free RAM).
		if b.Sleeping {
			if countJSON(pathIn(b.Root, "inbox", "pending")) == 0 {
				continue // stay asleep
			}
			reg.SetSleeping(b.Name, false)
			_ = SaveRegistry(root, reg)
			fmt.Fprintf(stdout, "binding %s waking (input arrived)\n", b.Name)
		} else if shouldSleep(b.Root, b.Worktree, cfg.IdleSleepTimeout()) {
			_ = StopTmuxSession(ctx, b.TmuxSession)
			reg.SetSleeping(b.Name, true)
			_ = SaveRegistry(root, reg)
			fmt.Fprintf(stdout, "binding %s sleeping (idle)\n", b.Name)
			continue
		}
		if err := EnsureWorktree(ctx, b.ProjectDir, b.Branch, b.Worktree); err != nil {
			fmt.Fprintf(stdout, "binding %s worktree error: %v\n", b.Name, err)
			continue
		}
		if err := StartTmuxClaude(ctx, b.TmuxSession, b.Worktree, root); err != nil {
			fmt.Fprintf(stdout, "binding %s session error: %v\n", b.Name, err)
			continue
		}
		// Stall watchdog: if the session has queued work but its transcript has
		// gone silent past the threshold, it's stuck — kill it so the next cycle
		// recreates it (--resume retries). Repeated stalls drop the poison job.
		switch stallAction(b.Root, b.Worktree, cfg.StallTimeout(), 3) {
		case "kill":
			_ = StopTmuxSession(ctx, b.TmuxSession)
			fmt.Fprintf(stdout, "binding %s stalled — restarting session\n", b.Name)
			continue
		case "giveup":
			_ = StopTmuxSession(ctx, b.TmuxSession)
			failStuckJobs(b.Root)
			fmt.Fprintf(stdout, "binding %s stalled repeatedly — dropped stuck job + restarting\n", b.Name)
			continue
		}
		tokens := bindingTokens{discord: token, telegram: os.Getenv(cfg.Telegram.TokenEnv)}
		sender, err := SelectSender(b, cfg, tokens)
		if err != nil {
			fmt.Fprintf(stdout, "binding %s sender error: %v\n", b.Name, err)
			continue
		}
		// Tee non-web replies to the ChatHub so any active binding is observable
		// from a browser chat window. Web bindings already publish via WebSender.
		if b.PlatformOf() != PlatformWeb {
			sender = TeeSender{Inner: sender, Hub: DefaultChatHub, Key: b.Name}
		}

		// Pick the per-cycle ingester. Poll bindings ingest each cycle; push
		// bindings ingest out-of-band via a persistent ingester (started once,
		// kept alive by the PushManager) and only drain here.
		var ingester Ingester
		if b.PlatformOf() == PlatformWeb {
			// Web bindings are fed out-of-band: the admin POST /api/chat/<name>/send
			// endpoint writes browser messages straight into the inbox. Just drain.
			ingester = noopIngester{}
		} else if cfg.DiscordTransport() == TransportGateway && b.PlatformOf() == PlatformDiscord {
			// Fed by the single shared Discord Gateway demux (started below); just
			// drain the inbox here. No per-binding poll or Gateway connection.
			ingester = noopIngester{}
		} else if b.ModeOf() == ModePush {
			if push == nil {
				fmt.Fprintf(stdout, "binding %s: push mode but no push manager\n", b.Name)
				continue
			}
			name := b.Name
			switch b.PlatformOf() {
			case PlatformTelegram:
				// Telegram webhooks share one HTTP server (one port), keyed by
				// path, so multiple tg-push bindings don't collide.
				handler := TelegramWebhookHandler{Root: b.Root, ChatID: b.ChannelID, Secret: cfg.Push.Secret}
				path := "/tg/" + b.ChannelID
				push.EnsureWebhook(name, cfg.Push.Listen, path, handler, func() error {
					if cfg.Push.PublicURL == "" {
						return nil
					}
					url := strings.TrimRight(cfg.Push.PublicURL, "/") + path
					if err := SetWebhook(ctx, cfg.Telegram.BaseURL, tokens.telegram, url, cfg.Push.Secret, nil); err != nil {
						fmt.Fprintf(stdout, "binding %s setWebhook error: %v\n", name, err)
						return err
					}
					return nil
				})
			default:
				pushIng, err := SelectPushIngester(b, cfg, tokens)
				if err != nil {
					fmt.Fprintf(stdout, "binding %s push ingester error: %v\n", b.Name, err)
					continue
				}
				push.Ensure(name, pushIng, func(e error) {
					if e != nil {
						fmt.Fprintf(stdout, "binding %s push ingester exited: %v\n", name, e)
					}
				})
			}
			activePush[name] = true
			ingester = noopIngester{}
		} else if b.PlatformOf() == PlatformTelegram {
			// tg-poll bindings are fed out-of-band by the shared TelegramReader
			// (which filled this binding's inbox above); just drain here.
			ingester = noopIngester{}
		} else {
			ingester, err = SelectIngester(b, cfg, tokens)
			if err != nil {
				fmt.Fprintf(stdout, "binding %s ingester error: %v\n", b.Name, err)
				continue
			}
		}

		injector := TmuxInjector{Session: b.TmuxSession, Root: b.Root, AutoStart: true}
		res, err := RunServeOnce(ctx, b.Root, ingester, injector, sender, timeout)
		if err != nil {
			fmt.Fprintf(stdout, "binding %s error: %v\n", b.Name, err)
			continue
		}
		fmt.Fprintf(stdout, "binding=%s created=%d processed=%t sent=%d\n", b.Name, res.Created, res.Processed, res.Sent)
	}
	// Registry-defined control bindings (unified control model). Each runs the
	// control pipeline against its own root/session, fed by its platform's source
	// and replying via the hub (web) or the platform sender teed to the hub
	// (dc/tg). Additive: a no-op when there are none, so it cannot disturb the
	// hardcoded dc/tg control planes above.
	for _, b := range reg.Bindings {
		if !b.Control || b.Paused {
			continue
		}
		var tokenEnv, tokenVal string
		var src MessageSource
		var snd Sender
		switch b.PlatformOf() {
		case PlatformWeb:
			// Browser is the transport: sendChat appends to this buffer; replies
			// stream back via the hub.
			src = TelegramBufferSource{Path: pathIn(b.Root, "state", "inbound_buffer.json")}
			snd = WebSender{Hub: DefaultChatHub, Key: b.Name}
		case PlatformTelegram:
			tokenEnv, tokenVal = cfg.Telegram.TokenEnv, tgToken
			src = TelegramBufferSource{Path: pathIn(b.Root, "state", "tg_buffer.json")}
			snd = TeeSender{Inner: TelegramSender{BaseURL: cfg.Telegram.BaseURL, Token: tgToken, ChatID: b.ChannelID}, Hub: DefaultChatHub, Key: b.Name}
		case PlatformDiscord:
			tokenEnv, tokenVal = cfg.Discord.TokenEnv, token
			src = BufferPollSource{BufferPath: pathIn(b.Root, "state", "inbound_buffer.json"), Poll: DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: b.ChannelID, Limit: 50}}
			snd = TeeSender{Inner: DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: b.ChannelID}, Hub: DefaultChatHub, Key: b.Name}
		default:
			continue
		}
		cplane := ControlPlane{Name: b.Name, Platform: b.PlatformOf(), ChannelID: b.ChannelID}
		if err := RunControlOnce(ctx, root, b.Root, deps, &reg, src, snd, cplane); err != nil {
			fmt.Fprintf(stdout, "control-binding[%s] error: %v\n", b.Name, err)
		}
		if reg, err = LoadRegistry(root); err != nil {
			return err
		}
		if err := StartControlSession(ctx, b.TmuxSession, b.Worktree, root, tokenEnv, tokenVal, controlSystemPrompt(root, b.Worktree, b.Name)); err != nil {
			fmt.Fprintf(stdout, "control-binding[%s] session error: %v\n", b.Name, err)
			continue
		}
		inj := TmuxInjector{Session: b.TmuxSession, Root: b.Root, AutoStart: false}
		if err := runControlAssistant(ctx, b, inj, snd, timeout); err != nil {
			fmt.Fprintf(stdout, "control-binding[%s] assistant error: %v\n", b.Name, err)
		}
	}

	// Stop push ingesters whose binding was removed or flipped to poll.
	if push != nil {
		push.Reconcile(activePush)
	}
	// Reap orphan cc-* tmux sessions (e.g. an unbind that raced with this cycle's
	// StartTmuxClaude). Valid = control session + one per current binding.
	valid := map[string]bool{ControlBinding(root).TmuxSession: true}
	// Keep every configured control plane's reserved session alive.
	for _, plane := range cfg.ControlPlanes() {
		valid[ControlBindingFor(root, plane.Name).TmuxSession] = true
	}
	for _, b := range reg.Bindings {
		// Paused/sleeping bindings intentionally have no session; leaving them out
		// of the valid set lets the reaper kill any session that lingers.
		if b.Paused || b.Sleeping {
			continue
		}
		valid[b.TmuxSession] = true
	}
	if orphans := reapOrphanSessions(ctx, valid); len(orphans) > 0 {
		fmt.Fprintf(stdout, "reaped orphan sessions: %v\n", orphans)
	}
	return nil
}

// hasControlNamed reports whether the registry has a control binding by name.
func hasControlNamed(reg *Registry, name string) bool {
	b, ok := reg.Get(name)
	return ok && b.Control
}

// seedControlPlanes migrates the configured (hardcoded) control planes into the
// registry as control bindings, reusing their EXISTING control-<x> identities
// (session/root/workspace) and plane name so the registry control loop runs them
// identically to the legacy path. Idempotent: only adds missing planes. Discord
// is the bootstrap lifeline → forced as the protected default. Returns whether
// the registry changed.
func seedControlPlanes(root string, cfg Config, reg *Registry) bool {
	changed := false
	add := func(planeName, channel string) {
		if channel == "" {
			return
		}
		if hasControlNamed(reg, planeName) {
			return
		}
		cb := ControlBindingFor(root, planeName)
		reg.Bindings = append(reg.Bindings, Binding{
			Name: planeName, Platform: planeName, Control: true, Plane: planeName,
			ChannelID: channel, TmuxSession: cb.TmuxSession, Root: cb.Root, Worktree: cb.Worktree,
		})
		changed = true
	}
	add(PlatformDiscord, cfg.Discord.ChannelID)
	add(PlatformTelegram, cfg.Control.TelegramChatID)
	// Discord is the lifeline → the protected default (only flip when needed so
	// this doesn't mark the registry dirty every cycle).
	if b, ok := reg.Get(PlatformDiscord); ok && b.Control && !b.Default {
		reg.SetDefaultControl(PlatformDiscord)
		changed = true
	}
	return changed
}
