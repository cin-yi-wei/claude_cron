package channelagent

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// waitSessionReady must NOT return while the pane is still changing (a large
// --resume replaying), even though the prompt line is already present; it returns
// only once the pane holds still for readyStableWindow.
func TestWaitSessionReadyWaitsForStable(t *testing.T) {
	oldOut := runExternalCommandOutput
	oldBD, oldPS, oldSW := sessionBootDelay, readyProbeSettle, readyStableWindow
	defer func() {
		runExternalCommandOutput = oldOut
		sessionBootDelay, readyProbeSettle, readyStableWindow = oldBD, oldPS, oldSW
	}()
	sessionBootDelay, readyProbeSettle, readyStableWindow = 5*time.Second, 10*time.Millisecond, 50*time.Millisecond

	n := 0
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		n++
		if n < 6 {
			// prompt present (ready) but content changing every capture = replaying
			return fmt.Sprintf("replaying line %d\n❯ \n? for shortcuts", n), nil
		}
		return "❯ \n? for shortcuts", nil // settled
	}

	done := make(chan struct{})
	go func() { waitSessionReady(context.Background(), "s"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("waitSessionReady hung (never saw stable)")
	}
	if n < 6 {
		t.Fatalf("returned before pane settled (n=%d) — injected into a replaying resume", n)
	}
}
