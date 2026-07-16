package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Discord Gateway opcodes (subset we use).
const (
	gwDispatch     = 0  // server → client: an event (t names it)
	gwHeartbeat    = 1  // client → server: keepalive
	gwIdentify     = 2  // client → server: auth
	gwResume       = 6  // client → server: resume a dropped session
	gwReconnect    = 7  // server → client: please reconnect
	gwInvalid      = 9  // server → client: invalid session
	gwHello        = 10 // server → client: heartbeat_interval
	gwHeartbeatACK = 11

	// readyTimeout bounds how long after IDENTIFY/RESUME we wait for the READY (or
	// RESUMED) dispatch. A healthy session always sends it within a couple seconds;
	// a connection that heartbeats but never delivers READY is a zombie — close it
	// so Run reconnects. This is the safety net the missed-heartbeat-ACK check
	// can't provide (the zombie DOES get ACKs, it just never sends events).
	readyTimeout = 20 * time.Second

	// Intents: GUILD_MESSAGES (1<<9) | MESSAGE_CONTENT (1<<15). MESSAGE_CONTENT
	// is privileged and must be enabled in the bot's settings.
	gwIntents = (1 << 9) | (1 << 15)

	defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
)

// gwConn is the minimal websocket surface the gateway loop needs, so the loop
// is unit-testable with a fake connection (no live Discord required).
type gwConn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close()
}

type coderWSConn struct{ c *websocket.Conn }

func (w coderWSConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	return data, err
}
func (w coderWSConn) Write(ctx context.Context, data []byte) error {
	return w.c.Write(ctx, websocket.MessageText, data)
}
// Close drops the connection WITHOUT a normal-closure (1000) handshake. A 1000
// close tells Discord to INVALIDATE the session, which makes RESUME impossible
// (op-9 on the next connect) — verified live. CloseNow just tears down the
// socket, so Discord keeps the session resumable for ~a few minutes.
func (w coderWSConn) Close() { _ = w.c.CloseNow() }

// DiscordGatewayIngester is the active/push ingester for Discord: it holds a
// websocket to the Gateway, receives MESSAGE_CREATE events for its channel, and
// writes them to the inbox. On disconnect Run returns; the PushManager restarts
// it on the next supervisor cycle.
type DiscordGatewayIngester struct {
	Root       string
	Token      string
	ChannelID  string
	GatewayURL string // optional override (tests / self-host)

	// Sink, when set, receives each captured MESSAGE_CREATE instead of the
	// default (writing to Root's inbox via IngestMessages). Used by the control
	// channel to feed a buffer rather than a binding inbox.
	Sink func(SourceMessage) error

	// Route, when set, switches this ingester to DEMUX mode: one Gateway
	// connection for the whole bot, routing every MESSAGE_CREATE by its own
	// channel id (ChannelID/Sink are ignored). The closure resolves + delivers
	// (e.g. via inboundRoutes); unrouted channels return nil (dropped). This is
	// the single-connection counterpart to per-binding ingesters.
	Route func(ctx context.Context, msg SourceMessage) error

	// Interact, when set, receives each INTERACTION_CREATE dispatch (Discord
	// message-component clicks, e.g. the permission gate's 允許/拒絕 按鈕). The
	// closure resolves the binding by channel id, writes the decision and ACKs
	// the interaction. A returned error should NOT tear down the connection
	// (unlike Route) — the closure logs and returns nil so a failed ACK can't
	// kill the shared demux; the decision write is what matters and happens first.
	Interact func(ctx context.Context, it gatewayInteraction) error

	// dial is injectable for tests; nil uses the real coder/websocket dialer.
	dial func(ctx context.Context, url string) (gwConn, error)
}

func (g DiscordGatewayIngester) deliver(ctx context.Context, msg SourceMessage) error {
	if g.Sink != nil {
		return g.Sink(msg)
	}
	_, err := IngestMessages(ctx, g.Root, []SourceMessage{msg})
	return err
}

// gwSession is the resumable state carried across reconnects. When present +
// valid, a reconnect RESUMEs (op 6) instead of a fresh IDENTIFY — this is what
// stops the identify-churn that (over a day of op-7 reconnects) degrades a
// session into a heartbeat-only zombie.
type gwSession struct {
	id        string // session_id from READY
	resumeURL string // resume_gateway_url from READY
	lastSeq   *int   // last dispatch seq (for RESUME + heartbeat)
}

// Run holds a Gateway connection and reconnects internally forever (until ctx is
// cancelled), RESUMEing when possible. Previously Run returned on any disconnect
// and the PushManager restarted it with a fresh IDENTIFY each time; that churn is
// exactly what degraded the shared demux into a silent zombie. Now reconnects are
// in-process and prefer RESUME, and a zombie (no READY within readyTimeout) is
// force-closed and retried.
func (g DiscordGatewayIngester) Run(ctx context.Context) error {
	if g.Token == "" {
		return fmt.Errorf("discord gateway: token required")
	}
	base := g.GatewayURL
	if base == "" {
		base = defaultGatewayURL
	}
	dial := g.dial
	if dial == nil {
		dial = func(ctx context.Context, url string) (gwConn, error) {
			c, _, err := websocket.Dial(ctx, url, nil)
			if err != nil {
				return nil, err
			}
			return coderWSConn{c: c}, nil
		}
	}

	var sess *gwSession
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		url := base
		if sess != nil && sess.resumeURL != "" {
			url = sess.resumeURL
		}
		conn, err := dial(ctx, url)
		if err != nil {
			// Dial failed: drop resume state, back off, retry fresh.
			sess = nil
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		next, err := g.runLoop(ctx, conn, sess)
		conn.Close()
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// next carries updated resume state, or nil to force a fresh IDENTIFY
		// (invalid session). err is the disconnect reason (logged by nobody now —
		// Run only returns on ctx cancel; reconnect is silent + automatic).
		sess = next
		_ = err
		if !sleepCtx(ctx, 750*time.Millisecond) {
			return ctx.Err()
		}
	}
}

// withGatewayQuery ensures a gateway URL carries ?v=10&encoding=json. The
// resume_gateway_url from READY is bare; RESUME needs the version/encoding query
// (same as defaultGatewayURL) or Discord invalidates the session (op 9).
func withGatewayQuery(u string) string {
	if strings.Contains(u, "?") {
		return u
	}
	return strings.TrimRight(u, "/") + "/?v=10&encoding=json"
}

type gwEnvelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int            `json:"s"`
	T  string          `json:"t"`
}

// runLoop drives one connection: HELLO → (RESUME if prev, else IDENTIFY) →
// heartbeat + dispatch. Returns the resume state to carry to the next reconnect
// (nil = force fresh IDENTIFY next time) and the disconnect error. Split out from
// Run so tests can feed a fake conn.
func (g DiscordGatewayIngester) runLoop(ctx context.Context, conn gwConn, prev *gwSession) (*gwSession, error) {
	// First frame must be HELLO with the heartbeat interval.
	first, err := conn.Read(ctx)
	if err != nil {
		return prev, err
	}
	var hello gwEnvelope
	if err := json.Unmarshal(first, &hello); err != nil {
		return prev, err
	}
	if hello.Op != gwHello {
		return prev, fmt.Errorf("discord gateway: expected HELLO op %d, got %d", gwHello, hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &helloData); err != nil {
		return prev, err
	}

	// session carries state forward; seed from prev when resuming.
	sess := &gwSession{}
	if prev != nil {
		*sess = *prev
	}
	resuming := prev != nil && prev.id != "" && prev.resumeURL != ""

	if resuming {
		if err := writeJSON(ctx, conn, map[string]any{
			"op": gwResume,
			"d": map[string]any{
				"token":      g.Token,
				"session_id": prev.id,
				"seq":        prev.lastSeq,
			},
		}); err != nil {
			return sess, err
		}
	} else {
		if err := writeJSON(ctx, conn, map[string]any{
			"op": gwIdentify,
			"d": map[string]any{
				"token":   g.Token,
				"intents": gwIntents,
				"properties": map[string]string{
					"os": "linux", "browser": "claude_cron", "device": "claude_cron",
				},
			},
		}); err != nil {
			return sess, err
		}
	}

	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = 30 * time.Second
	}
	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	var lastSeq *int = sess.lastSeq
	var missedAcks int32
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if atomic.AddInt32(&missedAcks, 1) > 1 {
					conn.Close() // no ACK since last beat → treat as dead
					return
				}
				_ = writeJSON(hbCtx, conn, map[string]any{"op": gwHeartbeat, "d": lastSeq})
			}
		}
	}()

	// Liveness: a healthy session sends READY (fresh) or RESUMED (resume) within a
	// couple seconds. If neither arrives within readyTimeout, the connection is a
	// zombie (heartbeats but no events) — close it so Read errors and we reconnect.
	var gotReady int32
	go func() {
		select {
		case <-hbCtx.Done():
			return
		case <-time.After(readyTimeout):
			if atomic.LoadInt32(&gotReady) == 0 {
				conn.Close()
			}
		}
	}()

	for {
		data, err := conn.Read(ctx)
		if err != nil {
			return sess, err
		}
		var ev gwEnvelope
		if err := json.Unmarshal(data, &ev); err != nil {
			return sess, err
		}
		if ev.S != nil {
			lastSeq = ev.S
			sess.lastSeq = ev.S
		}
		switch ev.Op {
		case gwHeartbeatACK:
			atomic.StoreInt32(&missedAcks, 0)
		case gwReconnect:
			// Resumable disconnect: keep session state so Run RESUMEs next.
			return sess, fmt.Errorf("discord gateway: server asked to reconnect (op %d)", ev.Op)
		case gwInvalid:
			// Session invalid → drop state so Run does a fresh IDENTIFY next.
			return nil, fmt.Errorf("discord gateway: invalid session (op %d)", ev.Op)
		case gwDispatch:
			switch ev.T {
			case "READY":
				var rd struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				if json.Unmarshal(ev.D, &rd) == nil {
					sess.id = rd.SessionID
					if rd.ResumeGatewayURL != "" {
						// resume_gateway_url comes bare (no query); RESUME needs the
						// same ?v=10&encoding=json or Discord rejects the session.
						sess.resumeURL = withGatewayQuery(rd.ResumeGatewayURL)
					}
				}
				atomic.StoreInt32(&gotReady, 1)
			case "RESUMED":
				atomic.StoreInt32(&gotReady, 1)
			case "MESSAGE_CREATE":
				if g.Route != nil {
					if msg, ok := gatewayExtract(ev.D); ok {
						if err := g.Route(ctx, msg); err != nil {
							return sess, err
						}
					}
				} else if msg, ok := gatewayMessageToSource(ev.D, g.ChannelID); ok {
					if err := g.deliver(ctx, msg); err != nil {
						return sess, err
					}
				}
			case "INTERACTION_CREATE":
				// 元件互動（按鈕）：交給 Interact 處理。它自己吞錯不回傳，避免
				// 一次 ACK 失敗就扯斷整條共用 demux 連線。
				if g.Interact != nil {
					if it, ok := gatewayInteractionExtract(ev.D); ok {
						if err := g.Interact(ctx, it); err != nil {
							return sess, err
						}
					}
				}
			}
		}
	}
}

func writeJSON(ctx context.Context, conn gwConn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, b)
}

type gatewayMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
	Attachments []struct {
		ID          string `json:"id"`
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	} `json:"attachments"`
}

// gatewayInteraction is the subset of an INTERACTION_CREATE payload we use for
// message-component (button) interactions. Token is the interaction token used
// to ACK; Message.Content is the original prompt text (so the ACK can keep it).
type gatewayInteraction struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ChannelID string `json:"channel_id"`
	Data      struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	Message struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	} `json:"message"`
}

// gatewayInteractionExtract parses an INTERACTION_CREATE payload. ok=false when
// it can't be parsed or carries no custom_id (nothing actionable).
func gatewayInteractionExtract(raw json.RawMessage) (gatewayInteraction, bool) {
	var it gatewayInteraction
	if err := json.Unmarshal(raw, &it); err != nil {
		return gatewayInteraction{}, false
	}
	if it.Data.CustomID == "" {
		return gatewayInteraction{}, false
	}
	return it, true
}

// gatewayExtract maps a MESSAGE_CREATE payload to a SourceMessage tagged with its
// own channel id (no channel filter), dropping bot messages. Used by demux mode,
// which routes by channel id rather than pre-filtering to one channel.
func gatewayExtract(raw json.RawMessage) (SourceMessage, bool) {
	var m gatewayMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return SourceMessage{}, false
	}
	if m.Author.Bot {
		return SourceMessage{}, false
	}
	created := m.Timestamp
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	src := SourceMessage{
		Platform:  "discord",
		ChannelID: m.ChannelID,
		MessageID: m.ID,
		AuthorID:  m.Author.ID,
		CreatedAt: created,
		Content:   m.Content,
	}
	for _, a := range m.Attachments {
		src.Attachments = append(src.Attachments, Attachment{ID: a.ID, URL: a.URL, Type: a.ContentType})
	}
	return src, true
}

// gatewayMessageToSource maps a MESSAGE_CREATE payload to a SourceMessage,
// keeping only non-bot messages for channelID.
func gatewayMessageToSource(raw json.RawMessage, channelID string) (SourceMessage, bool) {
	msg, ok := gatewayExtract(raw)
	if !ok || msg.ChannelID != channelID {
		return SourceMessage{}, false
	}
	return msg, true
}
