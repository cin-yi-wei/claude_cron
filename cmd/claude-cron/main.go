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

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: claude-cron <init|watcher|claude-worker|sender|serve|doctor>")
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "claude-cron %s\n", version)
		return 0
	case "init":
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		platform, initArgs := takePlatformArg(args[1:])
		discordChannelID := fs.String("discord-channel-id", "", "Discord channel ID")
		telegramChatID := fs.String("telegram-chat-id", "", "Telegram chat ID")
		if err := fs.Parse(initArgs); err != nil {
			return 2
		}
		if err := agent.Init(*root); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if platform != "" {
			cfg, err := agent.DefaultConfig(platform)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			cfg.Discord.ChannelID = *discordChannelID
			cfg.Telegram.ChatID = *telegramChatID
			if err := agent.SaveConfig(*root, cfg); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		return 0
	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		cfg, err := agent.LoadConfig(*root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := validateConfig(cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "doctor=ok")
		return 0
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		once := fs.Bool("once", false, "run one poll/process/send cycle")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		cfg, err := agent.LoadConfig(*root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := validateConfigForServe(cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		interval, err := time.ParseDuration(cfg.PollInterval)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if interval <= 0 {
			interval = 10 * time.Second
		}
		timeout, err := time.ParseDuration(cfg.Claude.Timeout)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for {
			if err := agent.RunSupervisorOnce(context.Background(), *root, cfg, timeout, stdout); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if *once {
				return 0
			}
			time.Sleep(interval)
		}
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
		processed, err := agent.RunWorkerOnce(context.Background(), *root, agent.TmuxInjector{Session: *session, Root: *root, AutoStart: true}, *timeout)
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

func takePlatformArg(args []string) (string, []string) {
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return "", args
	}
	return args[0], args[1:]
}

func validateConfig(cfg agent.Config) error {
	switch cfg.Platform {
	case "mock":
		if cfg.Mock.SourcePath == "" {
			return fmt.Errorf("mock source_path is required")
		}
	case "discord":
		if cfg.Discord.ChannelID == "" {
			return fmt.Errorf("discord channel_id is required")
		}
		if cfg.Discord.TokenEnv == "" || os.Getenv(cfg.Discord.TokenEnv) == "" {
			return fmt.Errorf("discord token env %q is not set", cfg.Discord.TokenEnv)
		}
	case "telegram":
		if cfg.Telegram.ChatID == "" {
			return fmt.Errorf("telegram chat_id is required")
		}
		if cfg.Telegram.TokenEnv == "" || os.Getenv(cfg.Telegram.TokenEnv) == "" {
			return fmt.Errorf("telegram token env %q is not set", cfg.Telegram.TokenEnv)
		}
	default:
		return fmt.Errorf("unsupported platform %q", cfg.Platform)
	}
	if cfg.Claude.TmuxSession == "" {
		return fmt.Errorf("claude tmux_session is required")
	}
	return nil
}

func validateConfigForServe(cfg agent.Config) error {
	switch cfg.Platform {
	case "mock":
		if cfg.Mock.SourcePath == "" {
			return fmt.Errorf("mock source_path is required")
		}
	case "discord":
		if cfg.Discord.ChannelID == "" {
			return fmt.Errorf("discord channel_id is required")
		}
		// Token value is checked at runtime; the supervisor handles auth errors gracefully.
		if cfg.Discord.TokenEnv == "" {
			return fmt.Errorf("discord token_env is required")
		}
	case "telegram":
		if cfg.Telegram.ChatID == "" {
			return fmt.Errorf("telegram chat_id is required")
		}
		// Token value is checked at runtime; the supervisor handles auth errors gracefully.
		if cfg.Telegram.TokenEnv == "" {
			return fmt.Errorf("telegram token_env is required")
		}
	default:
		return fmt.Errorf("unsupported platform %q", cfg.Platform)
	}
	if cfg.Claude.Timeout == "" {
		return fmt.Errorf("claude timeout is required")
	}
	if cfg.PollInterval == "" {
		return fmt.Errorf("poll_interval is required")
	}
	return nil
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
