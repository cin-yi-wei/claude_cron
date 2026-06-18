package channelagent

import (
	"context"
	"sync"
)

// ControlGatewaySource is the control channel's "Gateway + poll backstop"
// MessageSource. A background DiscordGatewayIngester pushes MESSAGE_CREATE into
// a buffer; Fetch() returns the buffered messages PLUS a normal poll. The poll
// is always run so the control channel — the lifeline — can never be worse than
// plain polling: if the Gateway misses or drops, poll still catches it, and the
// downstream control_seen dedup collapses any overlap.
type ControlGatewaySource struct {
	Poll DiscordSource // backstop, always run

	mu  sync.Mutex
	buf []SourceMessage
}

// push appends a Gateway-captured message to the buffer (the ingester Sink).
func (s *ControlGatewaySource) push(m SourceMessage) error {
	s.mu.Lock()
	s.buf = append(s.buf, m)
	s.mu.Unlock()
	return nil
}

// drain returns and clears the buffered Gateway messages.
func (s *ControlGatewaySource) drain() []SourceMessage {
	s.mu.Lock()
	out := s.buf
	s.buf = nil
	s.mu.Unlock()
	return out
}

func (s *ControlGatewaySource) Fetch(ctx context.Context) ([]SourceMessage, error) {
	buffered := s.drain()
	polled, err := s.Poll.Fetch(ctx)
	if err != nil {
		// Poll failed, but Gateway-buffered messages are still good — return them
		// rather than dropping everything on a transient REST hiccup.
		if len(buffered) > 0 {
			return buffered, nil
		}
		return nil, err
	}
	return append(buffered, polled...), nil
}

// gatewayIngester builds the background ingester that feeds this source's buffer.
func (s *ControlGatewaySource) gatewayIngester(token, channelID, baseURL string) DiscordGatewayIngester {
	return DiscordGatewayIngester{Token: token, ChannelID: channelID, Sink: s.push}
}

// BufferPollSource is the control source used when the shared Discord Gateway
// demux feeds the control channel (phase C): the demux writes the control
// channel's messages to a file buffer, and Fetch returns that buffer PLUS a poll
// backstop. Same lifeline guarantee as ControlGatewaySource — if the demux misses
// or its connection drops, the always-run poll still delivers; control_seen dedups
// any overlap — but with no separate per-control Gateway connection.
type BufferPollSource struct {
	BufferPath string
	Poll       MessageSource // backstop, always run (nil = buffer only)
}

func (s BufferPollSource) Fetch(ctx context.Context) ([]SourceMessage, error) {
	buffered, _ := TelegramBufferSource{Path: s.BufferPath}.Fetch(ctx) // read + clear
	if s.Poll == nil {
		return buffered, nil
	}
	polled, err := s.Poll.Fetch(ctx)
	if err != nil {
		if len(buffered) > 0 {
			return buffered, nil // poll hiccup, buffered still good
		}
		return nil, err
	}
	return append(buffered, polled...), nil
}
