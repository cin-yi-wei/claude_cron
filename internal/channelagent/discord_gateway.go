package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// Discord Gateway opcodes (subset we use).
const (
	gwDispatch     = 0  // server → client: an event (t names it)
	gwHeartbeat    = 1  // client → server: keepalive
	gwIdentify     = 2  // client → server: auth
	gwReconnect    = 7  // server → client: please reconnect
	gwInvalid      = 9  // server → client: invalid session
	gwHello        = 10 // server → client: heartbeat_interval
	gwHeartbeatACK = 11

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
func (w coderWSConn) Close() { _ = w.c.Close(websocket.StatusNormalClosure, "") }

// DiscordGatewayIngester is the active/push ingester for Discord: it holds a
// websocket to the Gateway, receives MESSAGE_CREATE events for its channel, and
// writes them to the inbox. On disconnect Run returns; the PushManager restarts
// it on the next supervisor cycle.
type DiscordGatewayIngester struct {
	Root       string
	Token      string
	ChannelID  string
	GatewayURL string // optional override (tests / self-host)

	// dial is injectable for tests; nil uses the real coder/websocket dialer.
	dial func(ctx context.Context, url string) (gwConn, error)
}

func (g DiscordGatewayIngester) Run(ctx context.Context) error {
	if g.Token == "" {
		return fmt.Errorf("discord gateway: token required")
	}
	url := g.GatewayURL
	if url == "" {
		url = defaultGatewayURL
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
	conn, err := dial(ctx, url)
	if err != nil {
		return fmt.Errorf("discord gateway dial: %w", err)
	}
	defer conn.Close()
	return g.runLoop(ctx, conn)
}

type gwEnvelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int            `json:"s"`
	T  string          `json:"t"`
}

// runLoop drives the gateway protocol over conn: HELLO → IDENTIFY → heartbeat
// loop + dispatch handling. Split out from Run so tests can feed a fake conn.
func (g DiscordGatewayIngester) runLoop(ctx context.Context, conn gwConn) error {
	// First frame must be HELLO with the heartbeat interval.
	first, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	var hello gwEnvelope
	if err := json.Unmarshal(first, &hello); err != nil {
		return err
	}
	if hello.Op != gwHello {
		return fmt.Errorf("discord gateway: expected HELLO op %d, got %d", gwHello, hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &helloData); err != nil {
		return err
	}

	// IDENTIFY.
	identify := map[string]any{
		"op": gwIdentify,
		"d": map[string]any{
			"token":   g.Token,
			"intents": gwIntents,
			"properties": map[string]string{
				"os": "linux", "browser": "claude_cron", "device": "claude_cron",
			},
		},
	}
	if err := writeJSON(ctx, conn, identify); err != nil {
		return err
	}

	// Heartbeat loop in the background. lastSeq is shared; reads are simple ints
	// so a coarse approach (send latest seen) is fine for keepalive.
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = 30 * time.Second
	}
	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	var lastSeq *int
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_ = writeJSON(hbCtx, conn, map[string]any{"op": gwHeartbeat, "d": lastSeq})
			}
		}
	}()

	// Dispatch loop.
	for {
		data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var ev gwEnvelope
		if err := json.Unmarshal(data, &ev); err != nil {
			return err
		}
		if ev.S != nil {
			lastSeq = ev.S
		}
		switch ev.Op {
		case gwHeartbeatACK:
			// ok
		case gwReconnect, gwInvalid:
			return fmt.Errorf("discord gateway: server asked to reconnect (op %d)", ev.Op)
		case gwDispatch:
			if ev.T == "MESSAGE_CREATE" {
				if msg, ok := gatewayMessageToSource(ev.D, g.ChannelID); ok {
					if _, err := IngestMessages(ctx, g.Root, []SourceMessage{msg}); err != nil {
						return err
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
}

// gatewayMessageToSource maps a MESSAGE_CREATE payload to a SourceMessage,
// keeping only non-bot messages for channelID.
func gatewayMessageToSource(raw json.RawMessage, channelID string) (SourceMessage, bool) {
	var m gatewayMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return SourceMessage{}, false
	}
	if m.ChannelID != channelID || m.Author.Bot {
		return SourceMessage{}, false
	}
	created := m.Timestamp
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	return SourceMessage{
		Platform:  "discord",
		ChannelID: channelID,
		MessageID: m.ID,
		AuthorID:  m.Author.ID,
		CreatedAt: created,
		Content:   m.Content,
	}, true
}
