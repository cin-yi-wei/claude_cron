package channelagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// RunSupervisorOnce runs one supervisor cycle: process the control channel,
// then run the per-binding pipeline for every registered binding.
func RunSupervisorOnce(ctx context.Context, root string, cfg Config, timeout time.Duration, stdout io.Writer) error {
	token := os.Getenv(cfg.Discord.TokenEnv)
	admin := DiscordAdmin{BaseURL: cfg.Discord.BaseURL, Token: token}

	reg, err := LoadRegistry(root)
	if err != nil {
		return err
	}

	deps := ControlDeps{
		Root:           root,
		GuildID:        cfg.Discord.GuildID,
		CreateChannel:  admin.CreateChannel,
		DeleteChannel:  admin.DeleteChannel,
		EnsureWorktree: EnsureWorktree,
		RemoveWorktree: RemoveWorktree,
		StartSession:   StartTmuxClaude,
		StopSession:    StopTmuxSession,
		InitRoot:       Init,
	}

	controlSource := DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID, Limit: 50}
	controlSender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: cfg.Discord.ChannelID}
	if err := RunControlOnce(ctx, root, deps, &reg, controlSource, controlSender); err != nil {
		fmt.Fprintf(stdout, "control error: %v\n", err)
	}

	// reg may have changed; reload the persisted set.
	reg, err = LoadRegistry(root)
	if err != nil {
		return err
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
		res, err := RunServeOnce(ctx, b.Root, source, injector, sender, timeout)
		if err != nil {
			fmt.Fprintf(stdout, "binding %s error: %v\n", b.Name, err)
			continue
		}
		fmt.Fprintf(stdout, "binding=%s created=%d processed=%t sent=%d\n", b.Name, res.Created, res.Processed, res.Sent)
	}
	return nil
}
