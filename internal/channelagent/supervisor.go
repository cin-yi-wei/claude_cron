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
func runControlAssistant(ctx context.Context, root, tokenEnv, tokenValue string, injector Injector, sender Sender, timeout time.Duration) error {
	cb := ControlBinding(root)
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
		controlSource = cs
	}
	controlSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
	if err := RunControlOnce(ctx, root, ControlBinding(root).Root, deps, &reg, controlSource, controlSender); err != nil {
		fmt.Fprintf(stdout, "control error: %v\n", err)
	}

	// reg may have changed; reload the persisted set.
	reg, err = LoadRegistry(root)
	if err != nil {
		return err
	}

	// Control assistant: start its session and drive any queued free-text jobs.
	cb := ControlBinding(root)
	if err := StartControlSession(ctx, cb.TmuxSession, cb.Worktree, cfg.Discord.TokenEnv, token, controlSystemPrompt(root, cb.Worktree)); err != nil {
		fmt.Fprintf(stdout, "control session error: %v\n", err)
	} else {
		controlInjector := TmuxInjector{Session: cb.TmuxSession, Root: cb.Root, AutoStart: false}
		controlChatSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
		if err := runControlAssistant(ctx, root, cfg.Discord.TokenEnv, token, controlInjector, controlChatSender, timeout); err != nil {
			fmt.Fprintf(stdout, "control assistant error: %v\n", err)
		}
	}

	activePush := map[string]bool{}
	for _, b := range reg.Bindings {
		if err := EnsureWorktree(ctx, b.ProjectDir, b.Branch, b.Worktree); err != nil {
			fmt.Fprintf(stdout, "binding %s worktree error: %v\n", b.Name, err)
			continue
		}
		if err := StartTmuxClaude(ctx, b.TmuxSession, b.Worktree); err != nil {
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
	return nil
}
