package channelagent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestTmuxInjectorAutoStartsMissingSession(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()

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
	if len(calls) != 3 {
		t.Fatalf("calls = %#v, want 3 calls", calls)
	}
	if !reflect.DeepEqual(calls[0], wantFirst) || !reflect.DeepEqual(calls[1], wantSecond) {
		t.Fatalf("calls = %#v", calls)
	}
}
