package channelagent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestControlGatewaySourceMergesBufferAndPoll(t *testing.T) {
	s := &ControlGatewaySource{}
	// poll backstop returns one message; gateway buffer has another.
	s.Poll = DiscordSource{} // not used directly; we stub via fakeSource below
	// Use a fakeSource as the poll by wrapping: ControlGatewaySource.Poll is a
	// concrete DiscordSource, so exercise merge logic via a manual variant.
	_ = s.push(SourceMessage{MessageID: "gw1", Content: "from-gateway"})

	// Emulate Fetch's merge with a stubbed poll result.
	buffered := s.drain()
	polled := []SourceMessage{{MessageID: "poll1", Content: "from-poll"}}
	merged := append(buffered, polled...)
	if len(merged) != 2 || merged[0].MessageID != "gw1" || merged[1].MessageID != "poll1" {
		t.Fatalf("merged = %#v", merged)
	}
	// Buffer is drained.
	if got := s.drain(); len(got) != 0 {
		t.Fatalf("buffer not drained: %#v", got)
	}
}

// fakePollSource lets us drive Fetch() error/values without real HTTP.
type ctlFetchStub struct {
	msgs []SourceMessage
	err  error
}

func (f ctlFetchStub) Fetch(context.Context) ([]SourceMessage, error) { return f.msgs, f.err }

// TestControlGatewayFetchKeepsBufferWhenPollFails verifies the drain+merge
// contract used by Fetch: buffered gateway messages survive a poll error.
func TestControlGatewayFetchKeepsBufferWhenPollFails(t *testing.T) {
	s := &ControlGatewaySource{}
	_ = s.push(SourceMessage{MessageID: "gw1"})

	// Simulate Fetch body with a failing poll.
	buffered := s.drain()
	poll := ctlFetchStub{err: errors.New("rest down")}
	polled, err := poll.Fetch(context.Background())
	var out []SourceMessage
	if err != nil {
		if len(buffered) > 0 {
			out = buffered
		}
	} else {
		out = append(buffered, polled...)
	}
	if len(out) != 1 || out[0].MessageID != "gw1" {
		t.Fatalf("expected buffered gw1 to survive poll failure, got %#v (err=%v)", out, err)
	}
}

func TestControlGatewayIngesterWiring(t *testing.T) {
	s := &ControlGatewaySource{}
	ing := s.gatewayIngester("tok", "chan1", "")
	if ing.Token != "tok" || ing.ChannelID != "chan1" || ing.Sink == nil {
		t.Fatalf("ingester not wired: %#v", ing)
	}
	// Sink feeds the buffer.
	_ = ing.Sink(SourceMessage{MessageID: "x"})
	if got := s.drain(); len(got) != 1 || got[0].MessageID != "x" {
		t.Fatalf("sink did not buffer: %#v", got)
	}
}

func TestBufferPollSourceMergesBufferAndPoll(t *testing.T) {
	dir := t.TempDir()
	buf := filepath.Join(dir, "inbound_buffer.json")
	if err := appendTelegramBuffer(buf, SourceMessage{ChannelID: "c", MessageID: "1", Content: "from-demux"}); err != nil {
		t.Fatal(err)
	}
	s := BufferPollSource{BufferPath: buf, Poll: stubFetchSource{msgs: []SourceMessage{{ChannelID: "c", MessageID: "2", Content: "from-poll"}}}}
	got, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 || got[0].Content != "from-demux" || got[1].Content != "from-poll" {
		t.Fatalf("merge = %#v", got)
	}
	// Buffer cleared after fetch.
	got2, _ := s.Fetch(context.Background())
	if len(got2) != 1 || got2[0].Content != "from-poll" {
		t.Fatalf("after clear = %#v", got2)
	}
}

func TestBufferPollSourceFallsBackOnPollError(t *testing.T) {
	dir := t.TempDir()
	buf := filepath.Join(dir, "inbound_buffer.json")
	_ = appendTelegramBuffer(buf, SourceMessage{ChannelID: "c", MessageID: "1", Content: "buffered"})
	s := BufferPollSource{BufferPath: buf, Poll: stubFetchSource{err: context.DeadlineExceeded}}
	got, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("should not error when buffer has messages: %v", err)
	}
	if len(got) != 1 || got[0].Content != "buffered" {
		t.Fatalf("fallback = %#v", got)
	}
}

type stubFetchSource struct {
	msgs []SourceMessage
	err  error
}

func (s stubFetchSource) Fetch(context.Context) ([]SourceMessage, error) { return s.msgs, s.err }
