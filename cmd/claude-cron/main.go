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
		fmt.Fprintln(stderr, "usage: channel-agent <init|watcher|claude-worker|sender>")
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
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		created, err := agent.RunWatcher(*root, *source)
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
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *adapter != "stdout" {
			fmt.Fprintf(stderr, "unsupported adapter %q\n", *adapter)
			return 2
		}
		sent, err := agent.RunSenderOnce(context.Background(), *root, agent.StdoutSender{Writer: stdout})
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
