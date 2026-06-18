package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	case "permission-gate":
		fs := flag.NewFlagSet("permission-gate", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root (registry)")
		timeoutStr := fs.String("timeout", "300s", "how long to wait for the user's y/n")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		absRoot := *root
		if env := os.Getenv("CC_REGISTRY_ROOT"); env != "" {
			absRoot = env // set by the session launcher; the hook has no flags
		}
		if a, err := filepath.Abs(absRoot); err == nil {
			absRoot = a
		}
		to, err := time.ParseDuration(*timeoutStr)
		if err != nil {
			to = 5 * time.Minute
		}
		// Output is the hook decision JSON on stdout; always exit 0 so Claude reads it.
		_ = agent.RunPermissionGate(context.Background(), absRoot, os.Stdin, stdout, to)
		return 0
	case "admin":
		fs := flag.NewFlagSet("admin", flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".channel-agent", "runtime root")
		listen := fs.String("listen", "", "admin API listen address (default from config admin.listen or 127.0.0.1:8787)")
		readonly := fs.Bool("readonly", false, "disable write endpoints (bind/unbind/restart)")
		tokenFlag := fs.String("token", "", "bearer token (overrides config admin.token; required for non-loopback)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		absRoot := *root
		if a, err := filepath.Abs(absRoot); err == nil {
			absRoot = a
		}
		loadDotEnv(absRoot)
		cfg, err := agent.LoadConfig(absRoot)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		addr := *listen
		if addr == "" {
			addr = cfg.Admin.Listen
		}
		if addr == "" {
			addr = "127.0.0.1:8787"
		}
		var deps *agent.ControlDeps
		if !*readonly {
			d := agent.BuildControlDeps(absRoot, cfg)
			deps = &d
		}
		token := cfg.Admin.Token
		if *tokenFlag != "" {
			token = *tokenFlag
		}
		fmt.Fprintf(stdout, "admin API listening on %s (writes=%t)\n", addr, !*readonly)
		if err := agent.RunAdminServer(context.Background(), absRoot, addr, token, deps, cfg.Discord.GuildID); err != nil && err != context.Canceled {
			fmt.Fprintln(stderr, err)
			return 1
		}
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
		// One PushManager lives across the whole serve loop so push ingesters
		// (webhook servers / gateway sockets) persist between per-cycle ticks.
		supCtx := context.Background()
		pushMgr := agent.NewPushManager(supCtx)
		defer pushMgr.StopAll()
		// Host the admin API in-process when configured, so serve + admin are ONE
		// service/process (one thing to supervise). The admin UI/API stays up with
		// serve and can restart bindings/sessions if a tmux session is missing.
		if cfg.Admin.Listen != "" {
			absRoot := *root
			if a, err := filepath.Abs(*root); err == nil {
				absRoot = a
			}
			adminDeps := agent.BuildControlDeps(absRoot, cfg)
			go func() {
				fmt.Fprintf(stdout, "admin API in-process on %s\n", cfg.Admin.Listen)
				if err := agent.RunAdminServer(supCtx, absRoot, cfg.Admin.Listen, cfg.Admin.Token, &adminDeps, cfg.Discord.GuildID); err != nil && err != context.Canceled {
					fmt.Fprintf(stderr, "admin server error: %v\n", err)
				}
			}()
		}
		for {
			if err := agent.RunSupervisorOnce(supCtx, *root, cfg, timeout, stdout, pushMgr); err != nil {
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
	case "notify":
		return runNotifyCommand(args[1:], stdout, stderr)
	case "bind", "unbind", "pause", "resume", "list":
		return runManageCommand(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func runManageCommand(name string, rest []string, stdout, stderr io.Writer) int {
	root := ".channel-agent"
	deleteChannel := false
	var pos []string
	opts := map[string]string{}
	for i := 0; i < len(rest); i++ {
		switch {
		case rest[i] == "--root":
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--root requires a value")
				return 2
			}
			root = rest[i+1]
			i++
		case rest[i] == "--delete-channel":
			deleteChannel = true
		case strings.HasPrefix(rest[i], "--"):
			// --key=value options (e.g. --platform=tg). Bare flags fall through.
			kv := strings.TrimPrefix(rest[i], "--")
			if k, v, ok := strings.Cut(kv, "="); ok {
				opts[k] = v
			}
		default:
			pos = append(pos, rest[i])
		}
	}

	if absRoot, err := filepath.Abs(root); err == nil {
		root = absRoot
	}
	loadDotEnv(root)

	cfg, cfgErr := agent.LoadConfig(root)
	if cfgErr != nil && name != "list" {
		fmt.Fprintln(stderr, cfgErr)
		return 1
	}
	reg, err := agent.LoadRegistry(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	deps := agent.BuildControlDeps(root, cfg)

	cmd := agent.Command{Name: name, Args: pos, Flags: map[string]bool{}, Opts: opts}
	if deleteChannel {
		cmd.Flags["delete-channel"] = true
	}

	// --plane selects which control plane this CLI invocation acts as (default
	// discord). A plane's control assistant passes --plane so its bindings are
	// tagged + filtered correctly.
	planeName := opts["plane"]
	if planeName == "" {
		planeName = agent.PlatformDiscord
	}
	plane := agent.ControlPlane{Name: planeName, Platform: planeName}

	reply, changed, herr := agent.HandleCommand(context.Background(), deps, &reg, cmd, plane)
	if changed {
		if serr := agent.SaveRegistry(root, reg); serr != nil {
			fmt.Fprintln(stderr, serr)
			return 1
		}
	}
	if reply != "" {
		fmt.Fprintln(stdout, reply)
	}
	if herr != nil {
		fmt.Fprintln(stderr, herr)
		return 1
	}
	// Validation rejections (bad name, missing dir, dup) come back as a reply
	// with changed=false and no error. For mutating commands treat "not changed"
	// as failure so callers/scripts see it; `list` is read-only and always 0.
	if !changed && name != "list" {
		return 1
	}
	return 0
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
		if cfg.Discord.GuildID == "" {
			return fmt.Errorf("discord guild_id is required")
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

func runNotifyCommand(rest []string, stdout, stderr io.Writer) int {
	root := ".channel-agent"
	var pos []string
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--root" {
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--root requires a value")
				return 2
			}
			root = rest[i+1]
			i++
			continue
		}
		pos = append(pos, rest[i])
	}
	if len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: claude-cron notify <channel-id> <text...> [--root <root>]")
		return 2
	}
	channelID := pos[0]
	text := strings.Join(pos[1:], " ")

	if absRoot, err := filepath.Abs(root); err == nil {
		root = absRoot
	}
	// Be forgiving about the --root the caller passed: notify is often invoked
	// with the control subdir (.channel-agent/control) but config.json lives at
	// .channel-agent. Walk up to the nearest ancestor that actually has a
	// config.json so both roots work.
	root = resolveConfigRoot(root)
	loadDotEnv(root)
	cfg, err := agent.LoadConfig(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sender := agent.DiscordSender{
		BaseURL:   cfg.Discord.BaseURL,
		Token:     os.Getenv(cfg.Discord.TokenEnv),
		ChannelID: channelID,
	}
	if err := sender.Send(context.Background(), agent.OutputJob{Send: true, Text: text}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
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

// resolveConfigRoot returns the nearest ancestor of start (including start
// itself) that contains a config.json. If none is found within a bounded walk,
// start is returned unchanged so the caller surfaces the original error.
func resolveConfigRoot(start string) string {
	dir := start
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(agent.ConfigPath(dir)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}

// loadDotEnv walks up from start (bounded) looking for a .env file and loads
// its KEY=VALUE pairs into the process environment. Existing environment
// variables are never overwritten, so an explicitly-exported token still wins.
// It is best-effort: a missing or unreadable .env is silently ignored.
//
// This lets ad-hoc invocations such as `claude-cron notify` (often run from a
// detached background shell that did not inherit DISCORD_BOT_TOKEN) find the
// token the same way the long-running daemon does.
func loadDotEnv(start string) {
	dir := start
	for i := 0; i < 8; i++ {
		if applyDotEnvFile(filepath.Join(dir, ".env")) {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
}

// applyDotEnvFile loads one .env file. It returns true when the file existed
// and was read (regardless of how many keys it held), false when absent.
func applyDotEnvFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return true
}
