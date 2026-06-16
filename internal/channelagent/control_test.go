package channelagent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cmd, ok := ParseCommand("/bind proj-a /home/u/a ticket-1")
	if !ok {
		t.Fatal("expected a command")
	}
	if cmd.Name != "bind" {
		t.Fatalf("Name = %q", cmd.Name)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"proj-a", "/home/u/a", "ticket-1"}) {
		t.Fatalf("Args = %#v", cmd.Args)
	}

	cmd2, ok := ParseCommand("/unbind proj-a --delete-channel")
	if !ok || cmd2.Name != "unbind" || !cmd2.Flags["delete-channel"] {
		t.Fatalf("unbind parse wrong: %#v ok=%v", cmd2, ok)
	}
	if !reflect.DeepEqual(cmd2.Args, []string{"proj-a"}) {
		t.Fatalf("unbind Args = %#v", cmd2.Args)
	}

	if _, ok := ParseCommand("hello world"); ok {
		t.Fatal("non-slash text should not parse as command")
	}
}

func newTestDeps(root string, created *[]string) ControlDeps {
	return ControlDeps{
		Root:    root,
		GuildID: "guild1",
		CreateChannel: func(_ context.Context, guildID, name string) (string, error) {
			*created = append(*created, "create:"+name)
			return "chan-" + name, nil
		},
		DeleteChannel: func(_ context.Context, channelID string) error {
			*created = append(*created, "delete:"+channelID)
			return nil
		},
		EnsureWorktree: func(_ context.Context, projectDir, branch, wt string) error {
			*created = append(*created, "worktree:"+branch)
			return nil
		},
		RemoveWorktree: func(_ context.Context, projectDir, wt string) error {
			*created = append(*created, "rmworktree:"+wt)
			return nil
		},
		StartSession: func(_ context.Context, session, cwd string) error {
			*created = append(*created, "start:"+session)
			return nil
		},
		StopSession: func(_ context.Context, session string) error {
			*created = append(*created, "stop:"+session)
			return nil
		},
		InitRoot: func(string) error { return nil },
	}
}

func TestHandleBindThenUnbind(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}

	projectDir := t.TempDir()
	cmd, _ := ParseCommand("/bind proj-a " + projectDir + " ticket-1")
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !changed {
		t.Fatal("bind should change registry")
	}
	if !strings.Contains(reply, "proj-a") {
		t.Fatalf("reply = %q", reply)
	}
	b, ok := reg.Get("proj-a")
	if !ok || b.ChannelID != "chan-proj-a" {
		t.Fatalf("binding not registered correctly: %#v ok=%v", b, ok)
	}
	wantOrder := []string{"create:proj-a", "worktree:ticket-1", "start:cc-proj-a"}
	for _, w := range wantOrder {
		if !containsStr(actions, w) {
			t.Fatalf("missing action %q in %#v", w, actions)
		}
	}

	cmd2, _ := ParseCommand("/unbind proj-a")
	_, changed2, err := HandleCommand(context.Background(), deps, &reg, cmd2)
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if !changed2 {
		t.Fatal("unbind should change registry")
	}
	if _, ok := reg.Get("proj-a"); ok {
		t.Fatal("proj-a still registered after unbind")
	}
	if containsStr(actions, "delete:chan-proj-a") {
		t.Fatal("channel should NOT be deleted without --delete-channel")
	}
	if !containsStr(actions, "stop:cc-proj-a") {
		t.Fatal("session should be stopped on unbind")
	}
}

func TestHandleBindRejectsBadName(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	projectDir := t.TempDir()
	cmd, _ := ParseCommand("/bind Bad_Name " + projectDir + " ticket-1")
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if changed {
		t.Fatal("bad name should not change registry")
	}
	if !strings.Contains(strings.ToLower(reply), "name") {
		t.Fatalf("reply should explain bad name, got %q", reply)
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestControlBindingDerivation(t *testing.T) {
	b := ControlBinding("/abs/root")
	if b.Name != "control" {
		t.Fatalf("Name = %q", b.Name)
	}
	if b.TmuxSession != "cc-control" {
		t.Fatalf("TmuxSession = %q", b.TmuxSession)
	}
	if b.Root != "/abs/root/control" {
		t.Fatalf("Root = %q", b.Root)
	}
	if b.Worktree != "/abs/root/control-workspace" {
		t.Fatalf("Worktree (workspace) = %q", b.Worktree)
	}
}

func TestControlSystemPromptMentionsCommands(t *testing.T) {
	p := controlSystemPrompt("/abs/root", "/abs/root/control-workspace")
	for _, want := range []string{"claude-cron bind", "claude-cron unbind", "claude-cron list", "/abs/root/control-workspace"} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, p)
		}
	}
}

func TestBuildControlDepsWiresConfig(t *testing.T) {
	cfg := Config{}
	cfg.Discord.GuildID = "g1"
	cfg.Discord.TokenEnv = "DISCORD_BOT_TOKEN"
	deps := BuildControlDeps("/abs/root", cfg)
	if deps.Root != "/abs/root" {
		t.Fatalf("Root = %q", deps.Root)
	}
	if deps.GuildID != "g1" {
		t.Fatalf("GuildID = %q", deps.GuildID)
	}
	if deps.CreateChannel == nil || deps.EnsureWorktree == nil || deps.StartSession == nil || deps.InitRoot == nil {
		t.Fatal("control deps function fields must be non-nil")
	}
}

func TestRunControlOnceRetriesFailedCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	calls := 0
	deps.CreateChannel = func(_ context.Context, guildID, name string) (string, error) {
		calls++
		return "", fmt.Errorf("boom")
	}
	reg := Registry{}
	src := stubSource{msgs: []SourceMessage{{Platform: "discord", ChannelID: "ctl", MessageID: "m1", AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z", Content: "/bind proj-a " + t.TempDir() + " ticket-1"}}}
	sender := &capSender{}
	_ = RunControlOnce(context.Background(), root, deps, &reg, src, sender)
	reg2, _ := LoadRegistry(root)
	_ = RunControlOnce(context.Background(), root, deps, &reg2, src, sender)
	if calls != 2 {
		t.Fatalf("expected failed command retried (calls=2), got calls=%d", calls)
	}
}
