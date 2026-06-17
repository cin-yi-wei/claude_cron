package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveGatewayCapture runs the real DiscordGatewayIngester against a channel
// (CC_GW_CHANNEL) for up to ~70s and reports the first non-bot MESSAGE_CREATE it
// writes to a temp inbox. Type a message in that channel while it runs.
// Gated by CC_LIVE_GW=1. Needs DISCORD_BOT_TOKEN.
func TestLiveGatewayCapture(t *testing.T) {
	if os.Getenv("CC_LIVE_GW") != "1" {
		t.Skip("set CC_LIVE_GW=1 to run the live capture")
	}
	token := os.Getenv("DISCORD_BOT_TOKEN")
	channel := os.Getenv("CC_GW_CHANNEL")
	if token == "" || channel == "" {
		t.Fatal("need DISCORD_BOT_TOKEN and CC_GW_CHANNEL")
	}
	root := filepath.Join(os.Getenv("CC_GW_OUT"), "gwcap")
	if os.Getenv("CC_GW_OUT") == "" {
		root = filepath.Join(t.TempDir(), "gwcap")
	}

	window := 300 * time.Second
	if v := os.Getenv("CC_GW_WINDOW_S"); v != "" {
		if n, err := time.ParseDuration(v + "s"); err == nil {
			window = n
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), window+2*time.Second)
	defer cancel()
	ing := DiscordGatewayIngester{Root: root, Token: token, ChannelID: channel}
	go func() { _ = ing.Run(ctx) }()

	pending := filepath.Join(root, "inbox", "pending")
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if names := listJSONNames(pending, 5); len(names) > 0 {
			t.Logf("CAPTURED %d job(s) in inbox: %v", len(names), names)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("no message captured in 70s (did anyone type in the channel?)")
}
