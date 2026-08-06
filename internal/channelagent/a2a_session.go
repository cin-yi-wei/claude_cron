package channelagent

import (
	"context"
	"errors"
	"fmt"
)

// SessionManager isolates every side effect that touches git or tmux, so tests
// can substitute FakeSessionManager and never spawn a real claude session.
type SessionManager interface {
	EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error
	Start(ctx context.Context, session, cwd, registryRoot string) error
	Stop(ctx context.Context, session string) error
	Inject(ctx context.Context, root string, msg SourceMessage) error
	// RemoveWorkspace tears down a sandbox checkout. Reclaiming only the tmux
	// session leaves ~80MB per task on disk, and contextId is caller-chosen, so
	// without this one approved caller can grow the disk without bound.
	RemoveWorkspace(ctx context.Context, projectDir, worktree string) error
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

func (TmuxSessionManager) RemoveWorkspace(ctx context.Context, projectDir, worktree string) error {
	return RemoveWorktree(ctx, projectDir, worktree)
}

func (TmuxSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	created, err := IngestMessages(ctx, root, []SourceMessage{msg})
	if err != nil {
		return err
	}
	if created == 0 {
		// IngestMessages dedups on platform:channel:messageID and reports a
		// duplicate by returning created=0 with a nil error: nothing landed
		// in the inbox, but the call otherwise looks like it succeeded.
		// Silently returning nil here is exactly how a caller
		// (SandboxExecutor.Start) came to believe a message was queued when
		// it had actually been dropped. Treat it as an error so the
		// caller's failure handling fires instead of lying about success.
		return fmt.Errorf("inject: message %s:%s:%s was deduped, nothing queued", msg.Platform, msg.ChannelID, msg.MessageID)
	}
	return nil
}

// FakeSessionManager records calls for assertions. FailOn makes one method
// return an error: "workspace", "start", "stop", or "inject".
type FakeSessionManager struct {
	Workspaces []string
	Started    []string
	Stopped    []string
	Injected   []SourceMessage
	Removed    []string
	FailOn     string
	// OnRemove, if set, fires once on the first RemoveWorkspace call, then
	// clears itself. Tests use it to inject a state change into tasks.json
	// from inside SweepTimeouts' teardown window — the gap between its step 1
	// (candidate identification) and step 3 (clearing fields for confirmed
	// matches) — to simulate a caller resubmitting the same contextId while
	// teardown for the previous, terminal task is in flight (task-8 review
	// round 3, finding 1).
	OnRemove func()
	// OnStart, if set, fires on every Start call after the session has been
	// recorded. Tests use it to observe the on-disk state that must already be
	// in place by the time a real tmux session would come up — the sandbox
	// policy in particular.
	OnStart func(session string)
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
	if f.OnStart != nil {
		f.OnStart(session)
	}
	return nil
}

func (f *FakeSessionManager) Stop(_ context.Context, session string) error {
	if f.FailOn == "stop" {
		return errors.New("fake stop failure")
	}
	f.Stopped = append(f.Stopped, session)
	return nil
}

func (f *FakeSessionManager) RemoveWorkspace(_ context.Context, _, worktree string) error {
	if f.OnRemove != nil {
		hook := f.OnRemove
		f.OnRemove = nil
		hook()
	}
	if f.FailOn == "remove" {
		return errors.New("fake remove failure")
	}
	f.Removed = append(f.Removed, worktree)
	return nil
}

func (f *FakeSessionManager) Inject(_ context.Context, _ string, msg SourceMessage) error {
	if f.FailOn == "inject" {
		return errors.New("fake inject failure")
	}
	f.Injected = append(f.Injected, msg)
	return nil
}
