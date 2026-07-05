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
	go func() { _, e := g.runLoop(ctx, conn, nil); done <- e }()

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
	go func() { _, e := g.runLoop(ctx, conn, nil); done <- e }()
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

func TestGatewayExtractCapturesAttachments(t *testing.T) {
	raw := []byte(`{"id":"42","channel_id":"c1","content":"look","timestamp":"2026-06-21T00:00:00Z","author":{"id":"u1","bot":false},"attachments":[{"id":"a1","url":"https://cdn/x.png","content_type":"image/png"}]}`)
	msg, ok := gatewayExtract(raw)
	if !ok {
		t.Fatal("gatewayExtract ok=false")
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want 1", msg.Attachments)
	}
	a := msg.Attachments[0]
	if a.URL != "https://cdn/x.png" || a.Type != "image/png" || a.ID != "a1" {
		t.Fatalf("attachment = %#v", a)
	}
}

// helper: run runLoop with prev, return its (session,err) after ctx cancel.
type gwResult struct {
	sess *gwSession
	err  error
}

func TestGatewayResumesWithPrevSession(t *testing.T) {
	hello := `{"op":10,"d":{"heartbeat_interval":45000}}`
	conn := &fakeGwConn{frames: [][]byte{[]byte(hello)}}
	g := DiscordGatewayIngester{Token: "tok", Route: func(context.Context, SourceMessage) error { return nil }}
	seq := 7
	prev := &gwSession{id: "sess-123", resumeURL: "wss://resume.example/?v=10", lastSeq: &seq}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan gwResult, 1)
	go func() { s, e := g.runLoop(ctx, conn, prev); res <- gwResult{s, e} }()
	waitFor(t, func() bool { conn.mu.Lock(); defer conn.mu.Unlock(); return len(conn.written) >= 1 })
	cancel()
	<-res
	conn.mu.Lock()
	defer conn.mu.Unlock()
	var env gwEnvelope
	if err := json.Unmarshal(conn.written[0], &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Op != gwResume {
		t.Fatalf("first write op = %d, want %d (RESUME)", env.Op, gwResume)
	}
	var d struct {
		SessionID string `json:"session_id"`
		Seq       *int   `json:"seq"`
	}
	_ = json.Unmarshal(env.D, &d)
	if d.SessionID != "sess-123" || d.Seq == nil || *d.Seq != 7 {
		t.Fatalf("resume payload wrong: session=%q seq=%v", d.SessionID, d.Seq)
	}
}

func TestGatewayReadyCapturesSession(t *testing.T) {
	hello := `{"op":10,"d":{"heartbeat_interval":45000}}`
	ready := `{"op":0,"t":"READY","s":1,"d":{"session_id":"S9","resume_gateway_url":"wss://gw2/"}}`
	conn := &fakeGwConn{frames: [][]byte{[]byte(hello), []byte(ready)}}
	g := DiscordGatewayIngester{Token: "tok", Route: func(context.Context, SourceMessage) error { return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan gwResult, 1)
	go func() { s, e := g.runLoop(ctx, conn, nil); res <- gwResult{s, e} }()
	// give it a moment to process READY, then end
	waitFor(t, func() bool { conn.mu.Lock(); defer conn.mu.Unlock(); return conn.idx >= 2 })
	cancel()
	r := <-res
	if r.sess == nil || r.sess.id != "S9" || r.sess.resumeURL != "wss://gw2/?v=10&encoding=json" {
		t.Fatalf("READY not captured: %+v", r.sess)
	}
	// first write must be IDENTIFY (no prev session)
	conn.mu.Lock()
	defer conn.mu.Unlock()
	var env gwEnvelope
	_ = json.Unmarshal(conn.written[0], &env)
	if env.Op != gwIdentify {
		t.Fatalf("first write op=%d want IDENTIFY", env.Op)
	}
}

func TestGatewayInvalidSessionDropsState(t *testing.T) {
	hello := `{"op":10,"d":{"heartbeat_interval":45000}}`
	invalid := `{"op":9,"d":false}`
	conn := &fakeGwConn{frames: [][]byte{[]byte(hello), []byte(invalid)}}
	g := DiscordGatewayIngester{Token: "tok", Route: func(context.Context, SourceMessage) error { return nil }}
	prev := &gwSession{id: "old", resumeURL: "wss://old/"}
	s, err := g.runLoop(context.Background(), conn, prev)
	if err == nil {
		t.Fatal("expected error on op9")
	}
	if s != nil {
		t.Fatalf("invalid session must drop state, got %+v", s)
	}
}
