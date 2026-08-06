package channelagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeSessionManagerRecordsCalls(t *testing.T) {
	var m SessionManager = &FakeSessionManager{}
	f := m.(*FakeSessionManager)
	ctx := context.Background()

	if err := m.EnsureWorkspace(ctx, "/p/x", "br", "/w/x"); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := m.Start(ctx, "aa-x-c1", "/w/x", "/root"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Inject(ctx, "/root", SourceMessage{Content: "go"}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := m.Stop(ctx, "aa-x-c1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(f.Workspaces) != 1 || f.Workspaces[0] != "/w/x" {
		t.Fatalf("Workspaces = %#v", f.Workspaces)
	}
	if len(f.Started) != 1 || f.Started[0] != "aa-x-c1" {
		t.Fatalf("Started = %#v", f.Started)
	}
	if len(f.Injected) != 1 || f.Injected[0].Content != "go" {
		t.Fatalf("Injected = %#v", f.Injected)
	}
	if len(f.Stopped) != 1 {
		t.Fatalf("Stopped = %#v", f.Stopped)
	}
}

func TestFakeSessionManagerCanFail(t *testing.T) {
	f := &FakeSessionManager{FailOn: "start"}
	if err := f.Start(context.Background(), "aa-x-c1", "/w/x", "/root"); err == nil {
		t.Fatal("expected Start to fail when FailOn=start")
	}
}

// TrustFolder 走 SessionManager 介面而不是直接呼叫 EnsureFolderTrusted 是
// 強制要求:後者寫的是 ~/.claude.json,那是這台機器上所有 claude 行程共用的
// 活檔,一個直接呼叫它的單元測試會改寫 operator 的線上設定。FakeSessionManager
// 只記錄呼叫,絕不碰真實檔案。
func TestFakeSessionManagerTrustFolderRecordsAndCanFail(t *testing.T) {
	var m SessionManager = &FakeSessionManager{}
	f := m.(*FakeSessionManager)
	if err := m.TrustFolder(context.Background(), "/w/x"); err != nil {
		t.Fatalf("TrustFolder: %v", err)
	}
	if len(f.Trusted) != 1 || f.Trusted[0] != "/w/x" {
		t.Fatalf("Trusted = %#v", f.Trusted)
	}

	failing := &FakeSessionManager{FailOn: "trust"}
	if err := failing.TrustFolder(context.Background(), "/w/x"); err == nil {
		t.Fatal("expected TrustFolder to fail when FailOn=trust")
	}
	if len(failing.Trusted) != 0 {
		t.Fatalf("Trusted should stay empty on failure: %#v", failing.Trusted)
	}
}

// TestTmuxSessionManagerStartWritesSandboxSettingsNotWorkerSettings pins
// a2a_session.go's TmuxSessionManager.Start call to StartTmuxClaudeSandbox
// (not StartTmuxClaude): SessionManager only ever serves aa- sandboxes, so
// reverting that one line back to StartTmuxClaude would silently reinstall
// the SessionStart hook — and the managed-settings gate it causes — on every
// sandbox's first boot, with no other test in the suite able to catch it.
// runExternalCommand is faked so this never spawns tmux or a real claude
// process.
func TestTmuxSessionManagerStartWritesSandboxSettingsNotWorkerSettings(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()
	oldDelay := sessionBootDelay
	sessionBootDelay = 0
	defer func() { sessionBootDelay = oldDelay }()
	runExternalCommand = func(_ context.Context, _ string, args ...string) error {
		if len(args) > 0 && args[0] == "has-session" {
			return context.Canceled // simulate "no such session"
		}
		return nil
	}

	cwd := t.TempDir()
	sm := TmuxSessionManager{}
	if err := sm.Start(context.Background(), "aa-x-c1", cwd, "/root"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(blob), "SessionStart") {
		t.Fatal("TmuxSessionManager.Start must write sandbox settings (no SessionStart), not the cc- worker settings")
	}
}

// TestTmuxSessionManagerInjectErrorsWhenDeduped pins the fix for the other
// half of the task-5 bug: IngestMessages reports a deduped message by
// returning created=0 with a nil error, so a caller that only checks the
// error (as SandboxExecutor.Start originally did) believes the message was
// queued when it was actually dropped. Inject must surface that as an error.
// This only exercises Inject's own disk I/O (via IngestMessages) — it never
// touches tmux or spawns a claude process.
func TestTmuxSessionManagerInjectErrorsWhenDeduped(t *testing.T) {
	root := t.TempDir()
	sm := TmuxSessionManager{}
	msg := SourceMessage{Platform: "a2a", ChannelID: "c1", MessageID: "same-id", Content: "x", CreatedAt: "2026-08-06T00:00:00Z"}

	if err := sm.Inject(context.Background(), root, msg); err != nil {
		t.Fatalf("first Inject: %v", err)
	}
	// Same Platform/ChannelID/MessageID: IngestMessages' dedup key matches,
	// so this second call genuinely queues nothing.
	if err := sm.Inject(context.Background(), root, msg); err == nil {
		t.Fatal("expected Inject to error when the message was deduped (created=0), got nil")
	}
}
