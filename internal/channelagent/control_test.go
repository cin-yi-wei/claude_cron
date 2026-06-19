package channelagent

import (
	"context"
	"fmt"
	"os"
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

	cmd2, ok := ParseCommand("/unbind proj-a")
	if !ok || cmd2.Name != "unbind" {
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
		InitProject: func(_ context.Context, projectDir string) error {
			*created = append(*created, "initproject:"+projectDir)
			return os.MkdirAll(projectDir, 0o755)
		},
		WipCommit: func(_ context.Context, wt string) error {
			*created = append(*created, "wip:"+wt)
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
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd, ControlPlane{})
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
	_, changed2, err := HandleCommand(context.Background(), deps, &reg, cmd2, ControlPlane{})
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
		t.Fatal("unbind must never delete the Discord channel")
	}
	if !containsStr(actions, "stop:cc-proj-a") {
		t.Fatal("session should be stopped on unbind")
	}
}

func TestUnbindLeavesTombstoneAndRebindReusesChannel(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	projectDir := t.TempDir()

	cmd, _ := ParseCommand("/bind proj-a " + projectDir + " ticket-1")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, cmd, ControlPlane{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	orig, _ := reg.Get("proj-a")

	cmd2, _ := ParseCommand("/unbind proj-a")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, cmd2, ControlPlane{}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	u, ok := reg.UnboundByName("proj-a")
	if !ok || u.ChannelID != orig.ChannelID || u.Branch != "ticket-1" {
		t.Fatalf("tombstone wrong: %#v ok=%v", u, ok)
	}

	// Count channel creations so far (one).
	creates := 0
	for _, a := range actions {
		if a == "create:proj-a" {
			creates++
		}
	}

	cmd3, _ := ParseCommand("/bind proj-a " + projectDir + " ticket-1")
	reply, _, err := HandleCommand(context.Background(), deps, &reg, cmd3, ControlPlane{})
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if !strings.Contains(reply, "重新綁定") {
		t.Fatalf("rebind reply should say 重新綁定, got %q", reply)
	}
	b, _ := reg.Get("proj-a")
	if b.ChannelID != orig.ChannelID {
		t.Fatalf("rebind channel = %q, want reused %q", b.ChannelID, orig.ChannelID)
	}
	if _, ok := reg.UnboundByName("proj-a"); ok {
		t.Fatal("tombstone should be cleared after rebind")
	}
	creates2 := 0
	for _, a := range actions {
		if a == "create:proj-a" {
			creates2++
		}
	}
	if creates2 != creates {
		t.Fatalf("rebind must NOT create a new channel: creates %d -> %d", creates, creates2)
	}
}

func TestHandleBindRejectsReservedControlName(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	_ = Init(root)
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	cmd, _ := ParseCommand("/bind control " + t.TempDir() + " dev")
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd, ControlPlane{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if changed {
		t.Fatal("reserved name must not change registry")
	}
	if !strings.Contains(reply, "control") {
		t.Fatalf("reply should explain reserved name, got %q", reply)
	}
	if len(actions) != 0 {
		t.Fatalf("no provisioning should happen, got %#v", actions)
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
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, cmd, ControlPlane{})
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
	if b.TmuxSession != "cc-dc-control" {
		t.Fatalf("TmuxSession = %q", b.TmuxSession)
	}
	if b.Root != "/abs/root/control-dc" {
		t.Fatalf("Root = %q", b.Root)
	}
	if b.Worktree != "/abs/root/control-dc-workspace" {
		t.Fatalf("Worktree (workspace) = %q", b.Worktree)
	}
}

func TestControlBindingDerivationTelegram(t *testing.T) {
	b := ControlBindingFor("/abs/root", PlatformTelegram)
	if b.Name != "control-telegram" {
		t.Fatalf("Name = %q", b.Name)
	}
	if b.TmuxSession != "cc-tg-control" {
		t.Fatalf("TmuxSession = %q", b.TmuxSession)
	}
	if b.Root != "/abs/root/control-tg" {
		t.Fatalf("Root = %q", b.Root)
	}
	if b.Worktree != "/abs/root/control-tg-workspace" {
		t.Fatalf("Worktree (workspace) = %q", b.Worktree)
	}
}

func TestControlSystemPromptMentionsCommands(t *testing.T) {
	p := controlSystemPrompt("/abs/root", "/abs/root/control-workspace", PlatformDiscord)
	for _, want := range []string{"claude-cron bind", "claude-cron unbind", "claude-cron list", "/abs/root/control-workspace"} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, p)
		}
	}
	// Discord (default) plane prompt carries no --plane flag.
	if strings.Contains(p, "--plane") {
		t.Fatalf("discord prompt should not mention --plane:\n%s", p)
	}
	// A non-discord plane prompt instructs --plane on every command.
	tg := controlSystemPrompt("/abs/root", "/abs/root/control-telegram-workspace", PlatformTelegram)
	if !strings.Contains(tg, "--plane=telegram") {
		t.Fatalf("telegram prompt must mention --plane=telegram:\n%s", tg)
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
	_ = RunControlOnce(context.Background(), root, ControlBinding(root).Root, deps, &reg, src, sender, ControlPlane{})
	reg2, _ := LoadRegistry(root)
	_ = RunControlOnce(context.Background(), root, ControlBinding(root).Root, deps, &reg2, src, sender, ControlPlane{})
	if calls != 2 {
		t.Fatalf("expected failed command retried (calls=2), got calls=%d", calls)
	}
}

func TestRunControlOnceRoutesFreeTextToInbox(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	controlRoot := ControlBinding(root).Root
	if err := Init(controlRoot); err != nil {
		t.Fatalf("Init controlRoot: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	src := stubSource{msgs: []SourceMessage{{
		Platform: "discord", ChannelID: "ctl", MessageID: "m1",
		AuthorID: "u1", CreatedAt: "2026-06-16T00:00:00Z",
		Content: "幫我建一個 logs 資料夾",
	}}}
	sender := &capSender{}

	if err := RunControlOnce(context.Background(), root, controlRoot, deps, &reg, src, sender, ControlPlane{}); err != nil {
		t.Fatalf("RunControlOnce: %v", err)
	}
	pending, _ := os.ReadDir(pathIn(controlRoot, "inbox", "pending"))
	if len(pending) != 1 {
		t.Fatalf("expected 1 queued control job, got %d", len(pending))
	}

	if err := RunControlOnce(context.Background(), root, controlRoot, deps, &reg, src, sender, ControlPlane{}); err != nil {
		t.Fatalf("RunControlOnce 2: %v", err)
	}
	pending2, _ := os.ReadDir(pathIn(controlRoot, "inbox", "pending"))
	if len(pending2) != 1 {
		t.Fatalf("free-text message re-enqueued, pending=%d", len(pending2))
	}
}

func TestHandleBindAutoInitsMissingProject(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}

	// Point at a path that does not exist yet; bind should auto-provision it.
	missing := filepath.Join(t.TempDir(), "brand-new-proj")
	cmd, _ := ParseCommand("/bind newproj " + missing + " dev")
	_, changed, err := HandleCommand(context.Background(), deps, &reg, cmd, ControlPlane{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !changed {
		t.Fatal("bind on auto-init should change registry")
	}
	if !containsStr(actions, "initproject:"+missing) {
		t.Fatalf("expected initproject action, got %#v", actions)
	}
}

func TestHandleUnbindWipsBeforeRemove(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	projectDir := t.TempDir()
	bindCmd, _ := ParseCommand("/bind proj-w " + projectDir + " feat-x")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, bindCmd, ControlPlane{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	b, _ := reg.Get("proj-w")

	actions = nil
	unbindCmd, _ := ParseCommand("/unbind proj-w")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, unbindCmd, ControlPlane{}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	// gwip must run, and must come before the worktree removal.
	wipIdx, rmIdx := -1, -1
	for i, a := range actions {
		if a == "wip:"+b.Worktree {
			wipIdx = i
		}
		if a == "rmworktree:"+b.Worktree {
			rmIdx = i
		}
	}
	if wipIdx < 0 {
		t.Fatalf("expected wip action, got %#v", actions)
	}
	if rmIdx < 0 || wipIdx > rmIdx {
		t.Fatalf("wip must precede rmworktree: %#v", actions)
	}
}

func TestHandlePauseResume(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}
	projectDir := t.TempDir()
	bindCmd, _ := ParseCommand("/bind proj-p " + projectDir + " dev")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, bindCmd, ControlPlane{}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	actions = nil
	pauseCmd, _ := ParseCommand("/pause proj-p")
	reply, changed, err := HandleCommand(context.Background(), deps, &reg, pauseCmd, ControlPlane{})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !changed {
		t.Fatal("pause should change registry")
	}
	if b, _ := reg.Get("proj-p"); !b.Paused {
		t.Fatal("binding should be paused")
	}
	if !containsStr(actions, "stop:cc-proj-p") {
		t.Fatalf("pause should stop session, got %#v", actions)
	}
	if !strings.Contains(reply, "proj-p") {
		t.Fatalf("reply = %q", reply)
	}

	// Double pause is a no-op (no registry change).
	if _, changed2, _ := HandleCommand(context.Background(), deps, &reg, pauseCmd, ControlPlane{}); changed2 {
		t.Fatal("second pause should not change registry")
	}

	resumeCmd, _ := ParseCommand("/resume proj-p")
	_, changed3, err := HandleCommand(context.Background(), deps, &reg, resumeCmd, ControlPlane{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !changed3 {
		t.Fatal("resume should change registry")
	}
	if b, _ := reg.Get("proj-p"); b.Paused {
		t.Fatal("binding should be un-paused")
	}

	// Resume when not paused is a no-op.
	if _, changed4, _ := HandleCommand(context.Background(), deps, &reg, resumeCmd, ControlPlane{}); changed4 {
		t.Fatal("resume on active binding should not change registry")
	}
}

func TestControlPlanesIsolation(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var actions []string
	deps := newTestDeps(root, &actions)
	reg := Registry{}

	dc := ControlPlane{Name: PlatformDiscord, Platform: PlatformDiscord}
	tg := ControlPlane{Name: PlatformTelegram, Platform: PlatformTelegram}

	// Bind one binding from each plane.
	dcCmd, _ := ParseCommand("/bind dcproj " + t.TempDir() + " feat")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, dcCmd, dc); err != nil {
		t.Fatalf("dc bind: %v", err)
	}
	tgCmd, _ := ParseCommand("/bind tgproj " + t.TempDir() + " feat --chat-id=42")
	if _, _, err := HandleCommand(context.Background(), deps, &reg, tgCmd, tg); err != nil {
		t.Fatalf("tg bind: %v", err)
	}

	// Plane stamping + platform defaulting.
	if b, _ := reg.Get("dcproj"); b.PlaneOf() != PlatformDiscord || b.PlatformOf() != PlatformDiscord {
		t.Fatalf("dcproj plane/platform wrong: %#v", b)
	}
	if b, _ := reg.Get("tgproj"); b.PlaneOf() != PlatformTelegram || b.PlatformOf() != PlatformTelegram {
		t.Fatalf("tgproj should default to telegram plane+platform: %#v", b)
	}

	// list is plane-scoped.
	if got := handleList(&reg, dc); !strings.Contains(got, "dcproj") || strings.Contains(got, "tgproj") {
		t.Fatalf("discord list should show only dcproj: %q", got)
	}
	if got := handleList(&reg, tg); !strings.Contains(got, "tgproj") || strings.Contains(got, "dcproj") {
		t.Fatalf("telegram list should show only tgproj: %q", got)
	}

	// Cross-plane unbind is refused (not found), and leaves the binding intact.
	xCmd, _ := ParseCommand("/unbind tgproj")
	reply, changed, _ := HandleCommand(context.Background(), deps, &reg, xCmd, dc)
	if changed || !strings.Contains(reply, "找不到") {
		t.Fatalf("discord must not see tgproj: reply=%q changed=%v", reply, changed)
	}
	if _, ok := reg.Get("tgproj"); !ok {
		t.Fatal("tgproj should still exist after cross-plane unbind attempt")
	}

	// Same-plane unbind works.
	if _, changed, _ := HandleCommand(context.Background(), deps, &reg, xCmd, tg); !changed {
		t.Fatal("same-plane unbind should succeed")
	}

	// Global-unique names: a second plane can't reuse a name.
	dupCmd, _ := ParseCommand("/bind dcproj " + t.TempDir() + " feat --chat-id=9")
	if reply, changed, _ := HandleCommand(context.Background(), deps, &reg, dupCmd, tg); changed || !strings.Contains(reply, "已存在") {
		t.Fatalf("name must be globally unique: reply=%q changed=%v", reply, changed)
	}
}

func TestConfigControlPlanes(t *testing.T) {
	var c Config
	c.Discord.ChannelID = "dc1"
	if p := c.ControlPlanes(); len(p) != 1 || p[0].Name != PlatformDiscord || p[0].ChannelID != "dc1" {
		t.Fatalf("default planes = %#v", p)
	}
	c.Control.TelegramChatID = "tg1"
	p := c.ControlPlanes()
	if len(p) != 2 || p[1].Name != PlatformTelegram || p[1].Platform != PlatformTelegram || p[1].ChannelID != "tg1" {
		t.Fatalf("with tg planes = %#v", p)
	}
}

func TestConfigTransport(t *testing.T) {
	var c Config
	// Transport is per-platform now (not per-binding mode). Discord defaults to
	// gateway; telegram defaults to poll.
	if got := c.Transport(Binding{Platform: PlatformDiscord}); got != TransportGateway {
		t.Fatalf("discord default = %q, want gateway", got)
	}
	if got := c.Transport(Binding{Platform: PlatformTelegram}); got != TransportPoll {
		t.Fatalf("telegram default = %q, want poll", got)
	}
	// Explicit enum wins.
	c.Discord.Transport = TransportPoll
	if got := c.Transport(Binding{Platform: PlatformDiscord}); got != TransportPoll {
		t.Fatalf("discord transport=poll = %q", got)
	}
	c.Telegram.Transport = TransportWebhook
	if got := c.Transport(Binding{Platform: PlatformTelegram}); got != TransportWebhook {
		t.Fatalf("telegram transport=webhook = %q", got)
	}
	// Legacy boolean still honoured as fallback when enum empty.
	var c2 Config
	c2.Discord.GatewayDemux = true
	if got := c2.DiscordTransport(); got != TransportGateway {
		t.Fatalf("legacy gateway_demux fallback = %q", got)
	}
	c2.Telegram.Webhook = true
	if got := c2.TelegramTransport(); got != TransportWebhook {
		t.Fatalf("legacy webhook fallback = %q", got)
	}
}
