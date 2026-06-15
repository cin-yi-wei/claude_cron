package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	agent "claude_cron/internal/channelagent"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: claude-cron <init|watcher|claude-worker|sender>")
		return 2
	}

	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := agent.Init(*root); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "watcher":
		fs := flag.NewFlagSet("watcher", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		source := fs.String("source", ".channel-agent/mock/source_messages.json", "mock source messages JSON")
		sourceAdapter := fs.String("source-adapter", "mock", "source adapter: mock, discord, telegram")
		discordTokenEnv := fs.String("discord-token-env", "DISCORD_BOT_TOKEN", "Discord bot token env var")
		discordChannelID := fs.String("discord-channel-id", "", "Discord channel ID")
		discordBaseURL := fs.String("discord-base-url", "", "Discord API base URL")
		telegramTokenEnv := fs.String("telegram-token-env", "TELEGRAM_BOT_TOKEN", "Telegram bot token env var")
		telegramChatID := fs.String("telegram-chat-id", "", "Telegram chat ID")
		telegramBaseURL := fs.String("telegram-base-url", "", "Telegram API base URL")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		messageSource, err := buildSource(*sourceAdapter, *source, platformConfig{
			discordTokenEnv:  *discordTokenEnv,
			discordChannelID: *discordChannelID,
			discordBaseURL:   *discordBaseURL,
			telegramTokenEnv: *telegramTokenEnv,
			telegramChatID:   *telegramChatID,
			telegramBaseURL:  *telegramBaseURL,
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		created, err := agent.RunWatcherWithSource(context.Background(), *root, messageSource)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "created=%d\n", created)
		return 0
	case "claude-worker":
		fs := flag.NewFlagSet("claude-worker", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		session := fs.String("tmux-session", "channel-agent", "tmux session running Claude Code")
		timeout := fs.Duration("timeout", 120*time.Second, "wait timeout")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		processed, err := agent.RunWorkerOnce(context.Background(), *root, agent.TmuxInjector{Session: *session, Root: *root}, *timeout)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "processed=%t\n", processed)
		return 0
	case "sender":
		fs := flag.NewFlagSet("sender", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		adapter := fs.String("adapter", "stdout", "sender adapter")
		discordTokenEnv := fs.String("discord-token-env", "DISCORD_BOT_TOKEN", "Discord bot token env var")
		discordChannelID := fs.String("discord-channel-id", "", "Discord channel ID")
		discordBaseURL := fs.String("discord-base-url", "", "Discord API base URL")
		telegramTokenEnv := fs.String("telegram-token-env", "TELEGRAM_BOT_TOKEN", "Telegram bot token env var")
		telegramChatID := fs.String("telegram-chat-id", "", "Telegram chat ID")
		telegramBaseURL := fs.String("telegram-base-url", "", "Telegram API base URL")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		sender, err := buildSender(*adapter, stdout, platformConfig{
			discordTokenEnv:  *discordTokenEnv,
			discordChannelID: *discordChannelID,
			discordBaseURL:   *discordBaseURL,
			telegramTokenEnv: *telegramTokenEnv,
			telegramChatID:   *telegramChatID,
			telegramBaseURL:  *telegramBaseURL,
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		sent, err := agent.RunSenderOnce(context.Background(), *root, sender)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "sent=%d\n", sent)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

type platformConfig struct {
	discordTokenEnv  string
	discordChannelID string
	discordBaseURL   string
	telegramTokenEnv string
	telegramChatID   string
	telegramBaseURL  string
}

func buildSource(adapter, mockPath string, cfg platformConfig) (agent.MessageSource, error) {
	switch adapter {
	case "mock":
		return agent.MockFileSource{Path: mockPath}, nil
	case "discord":
		return agent.DiscordSource{
			BaseURL:   cfg.discordBaseURL,
			Token:     os.Getenv(cfg.discordTokenEnv),
			ChannelID: cfg.discordChannelID,
			Limit:     50,
		}, nil
	case "telegram", "tg":
		return agent.TelegramSource{
			BaseURL: cfg.telegramBaseURL,
			Token:   os.Getenv(cfg.telegramTokenEnv),
			ChatID:  cfg.telegramChatID,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported source adapter %q", adapter)
	}
}

func buildSender(adapter string, stdout io.Writer, cfg platformConfig) (agent.Sender, error) {
	switch adapter {
	case "stdout":
		return agent.StdoutSender{Writer: stdout}, nil
	case "discord":
		return agent.DiscordSender{
			BaseURL:   cfg.discordBaseURL,
			Token:     os.Getenv(cfg.discordTokenEnv),
			ChannelID: cfg.discordChannelID,
		}, nil
	case "telegram", "tg":
		return agent.TelegramSender{
			BaseURL: cfg.telegramBaseURL,
			Token:   os.Getenv(cfg.telegramTokenEnv),
			ChatID:  cfg.telegramChatID,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported adapter %q", adapter)
	}
}
