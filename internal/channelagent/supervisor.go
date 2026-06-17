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
	if tgToken != "" && !cfg.Telegram.Webhook {
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
	if tgToken != "" && cfg.Telegram.Webhook && push != nil {
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

	controlPoll := DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID, Limit: 50}
	var controlSource MessageSource = controlPoll
	if cfg.Control.Mode == ModePush && push != nil {
		// Gateway-fed control with poll always-on as backstop (lifeline channel).
		cs := push.ControlSource(controlPoll)
		push.Ensure("__control_gw__", cs.gatewayIngester(token, cfg.Discord.ChannelID, cfg.Discord.BaseURL), func(e error) {
			if e != nil {
				fmt.Fprintf(stdout, "control gateway exited (poll backstop continues): %v\n", e)
			}
		})
		activePush["__control_gw__"] = true
		controlSource = cs
	}
	discordPlane := ControlPlane{Name: PlatformDiscord, Platform: PlatformDiscord, ChannelID: cfg.Discord.ChannelID}
	controlSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
	if err := RunControlOnce(ctx, root, ControlBinding(root).Root, deps, &reg, controlSource, controlSender, discordPlane); err != nil {
		fmt.Fprintf(stdout, "control error: %v\n", err)
	}

	// reg may have changed; reload the persisted set.
	reg, err = LoadRegistry(root)
	if err != nil {
		return err
	}

	// Control assistant: start its session and drive any queued free-text jobs.
	cb := ControlBinding(root)
	if err := StartControlSession(ctx, cb.TmuxSession, cb.Worktree, cfg.Discord.TokenEnv, token, controlSystemPrompt(root, cb.Worktree, PlatformDiscord)); err != nil {
		fmt.Fprintf(stdout, "control session error: %v\n", err)
	} else {
		controlInjector := TmuxInjector{Session: cb.TmuxSession, Root: cb.Root, AutoStart: false}
		controlChatSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
		if err := runControlAssistant(ctx, cb, controlInjector, controlChatSender, timeout); err != nil {
			fmt.Fprintf(stdout, "control assistant error: %v\n", err)
		}
	}

	// Extra (isolated) control planes — currently a Telegram plane when
	// configured. Each runs the same control pipeline against its own chat,
	// reserved session (cc-control-<plane>), root, and seen-state; it only
	// sees/manages bindings tagged with its own plane.
	tgControlPlanes := cfg.ControlPlanes()
	for _, plane := range tgControlPlanes {
		if plane.Name == PlatformDiscord {
			continue // handled above
		}
		if plane.Platform != PlatformTelegram || tgToken == "" {
			continue
		}
		pcb := ControlBindingFor(root, plane.Name)
		// Source = the buffer the shared reader routed this plane's messages into
		// (the reader owns the single getUpdates cursor); sender still posts直接.
		src := TelegramBufferSource{Path: pathIn(pcb.Root, "state", "tg_buffer.json")}
		snd := TelegramSender{BaseURL: cfg.Telegram.BaseURL, Token: tgToken, ChatID: plane.ChannelID}
		if err := RunControlOnce(ctx, root, pcb.Root, deps, &reg, src, snd, plane); err != nil {
			fmt.Fprintf(stdout, "control[%s] error: %v\n", plane.Name, err)
		}
		if reg, err = LoadRegistry(root); err != nil {
			return err
		}
		if err := StartControlSession(ctx, pcb.TmuxSession, pcb.Worktree, cfg.Telegram.TokenEnv, tgToken, controlSystemPrompt(root, pcb.Worktree, plane.Name)); err != nil {
			fmt.Fprintf(stdout, "control[%s] session error: %v\n", plane.Name, err)
			continue
		}
		inj := TmuxInjector{Session: pcb.TmuxSession, Root: pcb.Root, AutoStart: false}
		if err := runControlAssistant(ctx, pcb, inj, snd, timeout); err != nil {
			fmt.Fprintf(stdout, "control[%s] assistant error: %v\n", plane.Name, err)
		}
	}

	for _, b := range reg.Bindings {
		// Paused (hot-stopped) bindings: don't recreate the session or ingest.
		// The session was killed on /pause; any stray copy is reaped below
		// (excluded from the valid set). Messages stay in the channel until
		// /resume (poll bindings backfill via the unadvanced cursor).
		if b.Paused {
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
		tokens := bindingTokens{discord: token, telegram: os.Getenv(cfg.Telegram.TokenEnv)}
		sender, err := SelectSender(b, cfg, tokens)
		if err != nil {
			fmt.Fprintf(stdout, "binding %s sender error: %v\n", b.Name, err)
			continue
		}

		// Pick the per-cycle ingester. Poll bindings ingest each cycle; push
		// bindings ingest out-of-band via a persistent ingester (started once,
		// kept alive by the PushManager) and only drain here.
		var ingester Ingester
		if b.ModeOf() == ModePush {
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
		// Paused bindings intentionally have no session; leaving them out of the
		// valid set lets the reaper kill any session that lingers after /pause.
		if b.Paused {
			continue
		}
		valid[b.TmuxSession] = true
	}
	if orphans := reapOrphanSessions(ctx, valid); len(orphans) > 0 {
		fmt.Fprintf(stdout, "reaped orphan sessions: %v\n", orphans)
	}
	return nil
}
