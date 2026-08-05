package channelagent

import (
	"context"
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
