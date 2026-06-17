package channelagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnbindSurfacesWorktreeError(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	reg := Registry{}
	_ = reg.Add(Binding{Name: "x", ChannelID: "c", Worktree: "/tmp/x", ProjectDir: "/tmp", TmuxSession: "cc-x", Root: pathIn(root, "bindings", "x")})
	deps := ControlDeps{
		Root:           root,
		StopSession:    func(context.Context, string) error { return nil },
		RemoveWorktree: func(context.Context, string, string) error { return errors.New("boom") },
		DeleteChannel:  func(context.Context, string) error { return nil },
	}
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, Command{Name: "unbind", Args: []string{"x"}, Flags: map[string]bool{}}, ControlPlane{})
	if err != nil || !changed {
		t.Fatalf("unbind: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(reply, "worktree 清理可能不完全") {
		t.Fatalf("expected worktree warning in reply, got: %s", reply)
	}
	if _, ok := reg.Get("x"); ok {
		t.Fatal("binding should still be removed even if worktree cleanup warned")
	}
}
