package channelagent

import (
	"context"
	"errors"
)

// SessionManager isolates every side effect that touches git or tmux, so tests
// can substitute FakeSessionManager and never spawn a real claude session.
type SessionManager interface {
	EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error
	Start(ctx context.Context, session, cwd, registryRoot string) error
	Stop(ctx context.Context, session string) error
	Inject(ctx context.Context, root string, msg SourceMessage) error
}

// TmuxSessionManager is the production implementation, delegating to the same
// helpers the cc- supervisor uses.
type TmuxSessionManager struct{}

func (TmuxSessionManager) EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error {
	return EnsureWorktree(ctx, projectDir, branch, worktree)
}

func (TmuxSessionManager) Start(ctx context.Context, session, cwd, registryRoot string) error {
	return StartTmuxClaude(ctx, session, cwd, registryRoot)
}

func (TmuxSessionManager) Stop(ctx context.Context, session string) error {
	return StopTmuxSession(ctx, session)
}

func (TmuxSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	_, err := IngestMessages(ctx, root, []SourceMessage{msg})
	return err
}

// FakeSessionManager records calls for assertions. FailOn makes one method
// return an error: "workspace", "start", "stop", or "inject".
type FakeSessionManager struct {
	Workspaces []string
	Started    []string
	Stopped    []string
	Injected   []SourceMessage
	FailOn     string
}

func (f *FakeSessionManager) EnsureWorkspace(_ context.Context, _, _, worktree string) error {
	if f.FailOn == "workspace" {
		return errors.New("fake workspace failure")
	}
	f.Workspaces = append(f.Workspaces, worktree)
	return nil
}

func (f *FakeSessionManager) Start(_ context.Context, session, _, _ string) error {
	if f.FailOn == "start" {
		return errors.New("fake start failure")
	}
	f.Started = append(f.Started, session)
	return nil
}

func (f *FakeSessionManager) Stop(_ context.Context, session string) error {
	if f.FailOn == "stop" {
		return errors.New("fake stop failure")
	}
	f.Stopped = append(f.Stopped, session)
	return nil
}

func (f *FakeSessionManager) Inject(_ context.Context, _ string, msg SourceMessage) error {
	if f.FailOn == "inject" {
		return errors.New("fake inject failure")
	}
	f.Injected = append(f.Injected, msg)
	return nil
}
