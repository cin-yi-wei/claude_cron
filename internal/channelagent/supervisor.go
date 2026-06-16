package channelagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
func RunSupervisorOnce(ctx context.Context, root string, cfg Config, timeout time.Duration, stdout io.Writer) error {
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

	controlSource := DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID, Limit: 50}
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

	for _, b := range reg.Bindings {
		if err := EnsureWorktree(ctx, b.ProjectDir, b.Branch, b.Worktree); err != nil {
			fmt.Fprintf(stdout, "binding %s worktree error: %v\n", b.Name, err)
			continue
		}
		if err := StartTmuxClaude(ctx, b.TmuxSession, b.Worktree); err != nil {
			fmt.Fprintf(stdout, "binding %s session error: %v\n", b.Name, err)
			continue
		}
		source := DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: b.ChannelID, Limit: 50}
		sender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: b.ChannelID}
		injector := TmuxInjector{Session: b.TmuxSession, Root: b.Root, AutoStart: true}
		res, err := RunServeOnce(ctx, b.Root, PollIngester{Source: source}, injector, sender, timeout)
		if err != nil {
			fmt.Fprintf(stdout, "binding %s error: %v\n", b.Name, err)
			continue
		}
		fmt.Fprintf(stdout, "binding=%s created=%d processed=%t sent=%d\n", b.Name, res.Created, res.Processed, res.Sent)
	}
	return nil
}
