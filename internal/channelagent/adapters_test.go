package channelagent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTmuxInjectorAutoStartsMissingSession(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 0
	defer func() { injectSubmitDelay = oldDelay }()

	var calls [][]string
	runExternalCommand = func(_ context.Context, name string, args ...string) error {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		if name == "tmux" && len(args) >= 1 && args[0] == "has-session" {
			return errors.New("missing")
		}
		return nil
	}

	err := TmuxInjector{Session: "claude-cron", Root: ".channel-agent", AutoStart: true}.Inject(context.Background(), InputJob{JobID: "j1", RequestID: "r1", InputHash: "h1"}, ".channel-agent/outbox/pending/j1.json")
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}

	wantFirst := []string{"tmux", "has-session", "-t", "claude-cron"}
	wantSecond := []string{"tmux", "new-session", "-d", "-s", "claude-cron", "claude"}
	if len(calls) != 4 {
		t.Fatalf("calls = %#v, want 4 calls", calls)
	}
	if !reflect.DeepEqual(calls[0], wantFirst) || !reflect.DeepEqual(calls[1], wantSecond) {
		t.Fatalf("calls = %#v", calls)
	}
	// Third call sends the prompt literally as a single line; fourth submits with Enter.
	if calls[2][1] != "send-keys" || calls[2][len(calls[2])-2] != "-l" {
		t.Fatalf("third call = %#v, want literal send-keys", calls[2])
	}
	if strings.Contains(calls[2][len(calls[2])-1], "\n") {
		t.Fatalf("prompt must be single line, got %q", calls[2][len(calls[2])-1])
	}
	wantEnter := []string{"tmux", "send-keys", "-t", "claude-cron", "Enter"}
	if !reflect.DeepEqual(calls[3], wantEnter) {
		t.Fatalf("fourth call = %#v, want Enter submit", calls[3])
	}
}
