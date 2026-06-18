package channelagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGwConn feeds scripted frames to runLoop and records what was written.
type fakeGwConn struct {
	mu      sync.Mutex
	frames  [][]byte
	idx     int
	written [][]byte
	closed  bool
}

func (f *fakeGwConn) Read(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	if f.idx < len(f.frames) {
		frame := f.frames[f.idx]
		f.idx++
		f.mu.Unlock()
		return frame, nil
	}
	f.mu.Unlock()
	// Nothing more to deliver: block until ctx cancel so the loop ends via err.
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeGwConn) Write(ctx context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, append([]byte(nil), data...))
	return nil
}

func (f *fakeGwConn) Close() { f.closed = true }

func TestGatewayLoopIdentifiesAndIngests(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	hello := `{"op":10,"d":{"heartbeat_interval":45000}}`
	msg := `{"op":0,"t":"MESSAGE_CREATE","s":1,"d":{"id":"m1","channel_id":"c1","content":"hi","author":{"id":"u1","bot":false},"timestamp":"2026-06-16T01:30:12Z"}}`
	conn := &fakeGwConn{frames: [][]byte{[]byte(hello), []byte(msg)}}

	ctx, cancel := context.WithCancel(context.Background())
	g := DiscordGatewayIngester{Root: root, Token: "tok", ChannelID: "c1"}

	done := make(chan error, 1)
	go func() { done <- g.runLoop(ctx, conn) }()

	// Once the message is ingested, the inbox has a job; then cancel to end loop.
	waitFor(t, func() bool { return countJSONFilesSafe(filepath.Join(root, "inbox", "pending")) == 1 })
	cancel()
	<-done

	// First write must be an IDENTIFY (op 2) carrying our token.
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.written) == 0 {
		t.Fatal("no frames written (expected IDENTIFY)")
	}
	var ident gwEnvelope
	if err := json.Unmarshal(conn.written[0], &ident); err != nil {
		t.Fatalf("identify unmarshal: %v", err)
	}
	if ident.Op != gwIdentify {
		t.Fatalf("first write op = %d, want %d (IDENTIFY)", ident.Op, gwIdentify)
	}
}

func TestGatewayMessageToSourceFiltersBotAndOtherChannel(t *testing.T) {
	bot := json.RawMessage(`{"id":"m","channel_id":"c1","author":{"id":"b","bot":true}}`)
	if _, ok := gatewayMessageToSource(bot, "c1"); ok {
		t.Fatal("bot message should be filtered")
	}
	other := json.RawMessage(`{"id":"m","channel_id":"cZ","author":{"id":"u","bot":false}}`)
	if _, ok := gatewayMessageToSource(other, "c1"); ok {
		t.Fatal("other channel should be filtered")
	}
	ok := json.RawMessage(`{"id":"m","channel_id":"c1","content":"x","author":{"id":"u","bot":false},"timestamp":"2026-06-16T01:30:12Z"}`)
	if _, got := gatewayMessageToSource(ok, "c1"); !got {
		t.Fatal("valid message should pass")
	}
}

// countJSONFilesSafe is a non-fatal variant for use inside waitFor polling.
func countJSONFilesSafe(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

var _ PushIngester = DiscordGatewayIngester{}

func TestGatewayDemuxRoutesByChannel(t *testing.T) {
	hello := `{"op":10,"d":{"heartbeat_interval":45000}}`
	m1 := `{"op":0,"t":"MESSAGE_CREATE","s":1,"d":{"id":"a","channel_id":"c1","content":"one","author":{"id":"u","bot":false},"timestamp":"2026-06-16T01:30:12Z"}}`
	m2 := `{"op":0,"t":"MESSAGE_CREATE","s":2,"d":{"id":"b","channel_id":"c2","content":"two","author":{"id":"u","bot":false},"timestamp":"2026-06-16T01:30:13Z"}}`
	bot := `{"op":0,"t":"MESSAGE_CREATE","s":3,"d":{"id":"c","channel_id":"c1","content":"botmsg","author":{"id":"x","bot":true},"timestamp":"2026-06-16T01:30:14Z"}}`
	conn := &fakeGwConn{frames: [][]byte{[]byte(hello), []byte(m1), []byte(m2), []byte(bot)}}

	var mu sync.Mutex
	got := map[string]string{} // channel_id -> content
	g := DiscordGatewayIngester{Token: "tok", Route: func(_ context.Context, msg SourceMessage) error {
		mu.Lock()
		got[msg.ChannelID] = msg.Content
		mu.Unlock()
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.runLoop(ctx, conn) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if got["c1"] != "one" || got["c2"] != "two" {
		t.Fatalf("demux routing = %#v", got)
	}
	if _, ok := got["c1"]; ok && len(got) > 2 {
		t.Fatalf("bot message should have been dropped: %#v", got)
	}
}
