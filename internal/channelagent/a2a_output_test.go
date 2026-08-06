package channelagent

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The agent channel is output-only visibility. An agent with no ChannelID
// (or one entirely absent from agents.json) must degrade to silence: no send
// attempted, no error, no latency added to the caller.
func TestAgentOutputSinkSendLineDegradesToSilenceWithNoChannel(t *testing.T) {
	root := t.TempDir() // no agents.json at all

	var called int32
	send := func(_ context.Context, _, _ string) error {
		atomic.AddInt32(&called, 1)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := newAgentOutputSink(ctx, root, send)

	sink.Bind("ghost").SendLine("ctx1", "should never be sent")
	// Give the sender goroutine a window to (wrongly) act, since SendLine only
	// enqueues — the assertion is about what the goroutine does with it.
	time.Sleep(50 * time.Millisecond)

	cancel()
	sink.Wait()
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("send must not be called for an agent with no channel")
	}
}

// Every line delivered must carry the SandboxOutputPrefix contextId label —
// this is what makes one shared agent channel readable across up to 8
// interleaved sandboxes.
func TestAgentOutputSinkSendLinePrefixesWithContext(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	if err := agents.Add(Agent{Name: "pm", ChannelID: "chan-pm", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	type sent struct{ channelID, text string }
	got := make(chan sent, 1)
	send := func(_ context.Context, channelID, text string) error {
		got <- sent{channelID, text}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := newAgentOutputSink(ctx, root, send)

	sink.Bind("pm").SendLine("ctx7", "hello from the sandbox")

	select {
	case msg := <-got:
		if msg.channelID != "chan-pm" {
			t.Fatalf("channelID = %q, want chan-pm", msg.channelID)
		}
		if !strings.Contains(msg.text, "ctx7") {
			t.Fatalf("text = %q, missing context label", msg.text)
		}
		if !strings.Contains(msg.text, "hello from the sandbox") {
			t.Fatalf("text = %q, missing the line itself", msg.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send was never called")
	}
	cancel()
	sink.Wait()
}

// The whole point of decoupling SendLine from Discord I/O: a slow (or hung)
// send must never make SendLine itself block, because SendLine is called
// synchronously from a sandbox driver's loop. A direct DiscordSender.Send
// there would stall that sandbox's drive cadence for up to discordSendBudget
// (12s) on a throttled/429'd/hung channel — this proves the decoupling holds
// even when the consumer never keeps up. Bind is called once, up front, the
// same way a driver's loop() calls it once per sandbox — the loop below only
// exercises the repeated SendLine calls, which do no I/O of their own.
func TestAgentOutputSinkSendLineNeverBlocksOnSlowSend(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	if err := agents.Add(Agent{Name: "pm", ChannelID: "chan-pm", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	release := make(chan struct{})
	var calls int32
	slowSend := func(ctx context.Context, _, _ string) error {
		atomic.AddInt32(&calls, 1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := newAgentOutputSink(ctx, root, slowSend)
	channel := sink.Bind("pm")

	start := time.Now()
	// Enough sends to fill the queue well past capacity. If SendLine ever
	// blocked waiting for the (permanently stuck, until we close release)
	// consumer, this loop would hang for the full test timeout instead of
	// finishing near-instantly.
	const n = agentOutputQueueSize + 50
	for i := 0; i < n; i++ {
		channel.SendLine("ctx1", "line")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("SendLine blocked the caller: %d calls took %v", n, elapsed)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("the slow send was never even attempted once")
	}

	close(release)
	cancel()
	sink.Wait()
}

// The regression this whole fix targets: SendLine must do NO agents.json
// read of its own — only Bind does, once. Proven by deleting agents.json
// entirely right after Bind and confirming SendLine keeps delivering: if
// SendLine re-resolved the channel per call (the bug), the agent would look
// "unknown" the moment the file vanished and every line would silently drop.
func TestAgentChannelSendLineDoesNoDiskIOAfterBind(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	if err := agents.Add(Agent{Name: "pm", ChannelID: "chan-pm", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	got := make(chan string, 1)
	send := func(_ context.Context, _, text string) error {
		got <- text
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := newAgentOutputSink(ctx, root, send)

	channel := sink.Bind("pm") // the ONLY agents.json read this test expects

	if err := os.Remove(AgentsPath(root)); err != nil {
		t.Fatalf("remove agents.json: %v", err)
	}

	channel.SendLine("ctx1", "still delivered")

	select {
	case text := <-got:
		if !strings.Contains(text, "still delivered") {
			t.Fatalf("text = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendLine stopped delivering after agents.json vanished — it must be re-reading the agent per call")
	}
	cancel()
	sink.Wait()
}
