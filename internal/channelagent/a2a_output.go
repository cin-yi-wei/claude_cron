package channelagent

import (
	"context"
	"fmt"
	"os"
)

// agentOutputQueueSize bounds the buffered channel that decouples an
// AgentChannel's SendLine (called synchronously from a sandbox driver's loop)
// from the actual Discord POST. Sized to absorb a burst from up to 8
// concurrent sandboxes without requiring the sender goroutine to keep pace
// instantly. When it fills, SendLine drops the line instead of blocking —
// losing a visibility line is acceptable, stalling a sandbox's drive cadence
// is not.
const agentOutputQueueSize = 256

// agentOutputMsg is one already-prefixed line queued for delivery to a
// resolved Discord channel.
type agentOutputMsg struct {
	channelID string
	text      string
}

// AgentOutputSink decouples the sandbox driver loop from Discord I/O.
// AgentChannel.SendLine (obtained via Bind) is called synchronously from
// SandboxDriver's loop() and must NEVER block it: a direct DiscordSender.Send
// there would stall that sandbox's drive cadence for up to discordSendBudget
// (12s) whenever the channel is throttled, 429'd, or the connection hangs. A
// single background goroutine drains the queue through send, which in
// production (NewAgentOutputSink) is the exact same DiscordSender.Send →
// postMessage path activity streaming uses — same per-channel throttle, same
// 429 retry_after backoff. This must never grow into a second, bypass send
// path.
type AgentOutputSink struct {
	root string
	send func(ctx context.Context, channelID, text string) error

	ch   chan agentOutputMsg
	done chan struct{}
}

// NewAgentOutputSink builds a sink that delivers through the real Discord
// send path and starts its sender goroutine immediately, tied to ctx: the
// goroutine exits when ctx is done (serve shutdown), and Wait blocks until it
// has — no leaked goroutines. root is only used to resolve AgentChannelFor
// inside Bind, not cached config state.
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

// AgentChannel is a resolved binding to one agent's output channel: the
// agents.json lookup (AgentChannelFor) has already happened by the time this
// is returned from Bind, so every SendLine call on it does NO disk I/O — only
// a non-blocking buffered-channel send. Bind it ONCE per driver (at loop
// start) and reuse the handle for that sandbox's whole lifetime; calling Bind
// again per line would reintroduce the very per-line agents.json read this
// type exists to avoid. The zero value (and any handle for an agent with no
// ChannelID) is a valid, silent no-op: SendLine on it never sends, never
// errors, never touches disk. An agent's ChannelID changing mid-task is only
// picked up the next time the driver (re)starts and re-Binds, not mid-flight.
type AgentChannel struct {
	sink      *AgentOutputSink
	channelID string
	ok        bool
}

// Bind resolves agentName's output channel via a SINGLE agents.json read
// (AgentChannelFor) and returns a handle for repeated, I/O-free SendLine
// calls. Safe to call on a nil sink (returns the silent zero value), so
// callers that never wired an AgentOutputSink (most tests) get free
// degrade-to-silence without a nil check of their own.
func (s *AgentOutputSink) Bind(agentName string) AgentChannel {
	if s == nil {
		return AgentChannel{}
	}
	channelID, ok := AgentChannelFor(s.root, agentName)
	return AgentChannel{sink: s, channelID: channelID, ok: ok}
}

// SendLine queues line, labelled with SandboxOutputPrefix(contextID), for
// delivery on the channel this handle was Bind-ed to. Never blocks the
// caller and never touches disk (that already happened in Bind):
//   - an unresolved handle (no ChannelID / unknown agent) is a pure no-op —
//     no send, no error, no added latency;
//   - a resolved handle does one non-blocking buffered-channel send, dropping
//     the line if the queue is full rather than waiting for the sender
//     goroutine — losing a visibility line is acceptable, stalling a sandbox
//     is not.
func (c AgentChannel) SendLine(contextID, line string) {
	if !c.ok {
		return
	}
	msg := agentOutputMsg{channelID: c.channelID, text: SandboxOutputPrefix(contextID) + " " + line}
	select {
	case c.sink.ch <- msg:
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
