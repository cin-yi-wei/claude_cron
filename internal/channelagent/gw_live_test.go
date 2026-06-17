package channelagent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestLiveGatewayProbe connects to the real Discord Gateway with DISCORD_BOT_TOKEN
// and reports whether IDENTIFY succeeds (READY) or is rejected (e.g. disallowed
// MESSAGE_CONTENT intent → close 4014). Skipped unless CC_LIVE_GW=1.
//
//	CC_LIVE_GW=1 go test ./internal/channelagent/ -run TestLiveGatewayProbe -v
func TestLiveGatewayProbe(t *testing.T) {
	if os.Getenv("CC_LIVE_GW") != "1" {
		t.Skip("set CC_LIVE_GW=1 to run the live Discord Gateway probe")
	}
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		t.Fatal("DISCORD_BOT_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, defaultGatewayURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	read := func() (gwEnvelope, error) {
		_, data, err := c.Read(ctx)
		if err != nil {
			return gwEnvelope{}, err
		}
		var ev gwEnvelope
		_ = json.Unmarshal(data, &ev)
		return ev, nil
	}

	hello, err := read()
	if err != nil {
		t.Fatalf("read HELLO: %v", err)
	}
	t.Logf("HELLO op=%d", hello.Op)

	identify := map[string]any{"op": gwIdentify, "d": map[string]any{
		"token": token, "intents": gwIntents,
		"properties": map[string]string{"os": "linux", "browser": "claude_cron", "device": "claude_cron"},
	}}
	b, _ := json.Marshal(identify)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write IDENTIFY: %v", err)
	}

	for {
		ev, err := read()
		if err != nil {
			// A close here usually means a rejected IDENTIFY. 4014 = disallowed
			// (privileged) intent: enable MESSAGE CONTENT in the bot portal.
			t.Fatalf("after IDENTIFY, connection error (likely disallowed intent / bad token): %v", err)
		}
		t.Logf("frame op=%d t=%q", ev.Op, ev.T)
		if ev.Op == gwDispatch && ev.T == "READY" {
			t.Logf("READY — IDENTIFY accepted, intents OK ✅")
			return
		}
	}
}
