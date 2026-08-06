package channelagent

import (
	"context"
	"fmt"
	"os"
)

// agentOutputQueueSize bounds the buffered channel that decouples SendLine
// (called synchronously from a sandbox driver's loop) from the actual Discord
// POST. Sized to absorb a burst from up to 8 concurrent sandboxes without
// requiring the sender goroutine to keep pace instantly. When it fills,
// SendLine drops the line instead of blocking — see SendLine's doc: losing a
// visibility line is acceptable, stalling a sandbox's drive cadence is not.
const agentOutputQueueSize = 256

// agentOutputMsg is one already-prefixed line queued for delivery to a
// resolved Discord channel.
type agentOutputMsg struct {
	channelID string
	text      string
}

// AgentOutputSink decouples the sandbox driver loop from Discord I/O.
// SendLine is called synchronously from SandboxDriver's loop() and must NEVER
// block it: a direct DiscordSender.Send there would stall that sandbox's
// drive cadence for up to discordSendBudget (12s) whenever the channel is
// throttled, 429'd, or the connection hangs. A single background goroutine
// drains the queue through send, which in production (NewAgentOutputSink) is
// the exact same DiscordSender.Send → postMessage path activity streaming
// uses — same per-channel throttle, same 429 retry_after backoff. This must
// never grow into a second, bypass send path.
type AgentOutputSink struct {
	root string
	send func(ctx context.Context, channelID, text string) error

	ch   chan agentOutputMsg
	done chan struct{}
}

// NewAgentOutputSink builds a sink that delivers through the real Discord
// send path and starts its sender goroutine immediately, tied to ctx: the
// goroutine exits when ctx is done (serve shutdown), and Wait blocks until it
// has — no leaked goroutines. root is only used to resolve AgentChannelFor at
// send time (see SendLine), not cached config state.
func NewAgentOutputSink(ctx context.Context, root string, cfg Config) *AgentOutputSink {
	token := os.Getenv(cfg.Discord.TokenEnv)
	send := func(ctx context.Context, channelID, text string) error {
		sender := DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: token, ChannelID: channelID}
		return sender.Send(ctx, OutputJob{Schema: 1, Send: true, Text: text})
	}
	return newAgentOutputSink(ctx, root, send)
}

// newAgentOutputSink is the constructor tests use to substitute a fake send
// function, so the driver→sink wiring is exercised (including "never blocks
// on a slow send") without ever making a real HTTP call. Production always
// goes through NewAgentOutputSink's real Discord path.
func newAgentOutputSink(ctx context.Context, root string, send func(ctx context.Context, channelID, text string) error) *AgentOutputSink {
	s := &AgentOutputSink{
		root: root,
		send: send,
		ch:   make(chan agentOutputMsg, agentOutputQueueSize),
		done: make(chan struct{}),
	}
	go s.run(ctx)
	return s
}

func (s *AgentOutputSink) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.ch:
			if err := s.send(ctx, msg.channelID, msg.text); err != nil {
				fmt.Fprintf(os.Stderr, "a2a agent channel send failed: %v\n", err)
			}
		}
	}
}

// SendLine queues one line, labelled with SandboxOutputPrefix(contextID), for
// agentName's output channel. Never blocks the caller:
//   - an agent with no ChannelID (or unknown to agents.json) degrades to
//     silence — AgentChannelFor returns false, so this is a plain no-op: no
//     send, no error, no added latency;
//   - a full queue drops the line rather than wait for the sender goroutine —
//     losing a visibility line is acceptable, stalling a sandbox is not.
func (s *AgentOutputSink) SendLine(agentName, contextID, line string) {
	channelID, ok := AgentChannelFor(s.root, agentName)
	if !ok || channelID == "" {
		return
	}
	msg := agentOutputMsg{channelID: channelID, text: SandboxOutputPrefix(contextID) + " " + line}
	select {
	case s.ch <- msg:
	default:
		// Queue full: drop. Never block the driver loop that called us.
	}
}

// Wait blocks until the sender goroutine has exited (the ctx passed to
// New[...]AgentOutputSink is done). Mirrors SandboxDriver.StopAll's shutdown
// discipline — used on serve shutdown / in tests to prove no goroutine leaks.
func (s *AgentOutputSink) Wait() {
	<-s.done
}
