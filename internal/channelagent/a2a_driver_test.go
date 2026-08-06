package channelagent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingInjector stands in for TmuxInjector: it records what it was asked to
// deliver instead of typing into a real pane.
type recordingInjector struct {
	mu       sync.Mutex
	Injected []InputJob
}

func (r *recordingInjector) Inject(_ context.Context, job InputJob, outputPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Injected = append(r.Injected, job)
	return nil
}

func (r *recordingInjector) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Injected)
}

// The defect this plan exists to fix: a staged job must actually be delivered.
// Asserting that Inject was CALLED is not enough — that is what let the missing
// delivery slip through thirteen reviews.
func TestSandboxDriverDeliversStagedJob(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskWorking}
	sandbox := SandboxRoot(root, task.Session)
	if err := Init(sandbox); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := IngestMessages(context.Background(), sandbox, []SourceMessage{{
		Platform: "a2a", ChannelID: "c1", MessageID: "m1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Content: "review this",
	}}); err != nil {
		t.Fatalf("stage job: %v", err)
	}

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, inj)
	defer d.StopAll()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && inj.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if inj.count() == 0 {
		t.Fatal("driver never delivered the staged job to the injector")
	}
	if got := inj.Injected[0].Source.Content; got != "review this" {
		t.Fatalf("delivered content = %q, want %q", got, "review this")
	}
}

func TestSandboxDriverIsIdempotentPerSession(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "a", Session: SessionNameFor("a", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 5; i++ {
		d.Ensure(ctx, task, &recordingInjector{})
	}
	defer d.StopAll()

	if got := d.Running(); len(got) != 1 {
		t.Fatalf("Running() = %#v, want exactly one driver for the session", got)
	}
}

func TestSandboxDriverStopEndsTheLoop(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "a", Session: SessionNameFor("a", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	d.Stop(task.Session)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(d.Running()) != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.Running(); len(got) != 0 {
		t.Fatalf("Running() = %#v after Stop, want empty", got)
	}
}

// confirmPaneFixture is a realistic Claude Code confirm-dialog snapshot — the
// same shape TestParseConfirmDialog (confirm_test.go) uses — reused here so
// the auto-answer tests exercise a real dialog structure, not a hand-rolled
// shortcut.
const confirmPaneFixture = "blah\n Do you want to create SKILL.md?\n ❯ 1. Yes\n   2. Yes, and allow Claude to edit its own settings for this session\n   3. No\n Esc to cancel"

// proseNumberedListFixture is ordinary prose that happens to contain a
// numbered list but no "❯" selection cursor — must never be mistaken for a
// live confirm dialog (mirrors TestParseConfirmDialogRejectsProse).
const proseNumberedListFixture = "steps:\n1. clone\n2. build\n3. run"

// stubTmuxPane replaces runExternalCommandOutput/runExternalCommand for the
// life of the calling test: every capture-pane call returns pane, and every
// send-keys call's final argument (the key/Enter) is appended to *sent. Both
// package vars are restored via t.Cleanup. This also removes any dependency
// on a real tmux binary — the whole point being tested is what the auto-answer
// function DECIDES to type, not tmux itself.
func stubTmuxPane(t *testing.T, pane string, sent *[]string) {
	t.Helper()
	oldOut, oldRun := runExternalCommandOutput, runExternalCommand
	var mu sync.Mutex
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		return pane, nil
	}
	runExternalCommand = func(_ context.Context, _ string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		*sent = append(*sent, args[len(args)-1])
		return nil
	}
	t.Cleanup(func() { runExternalCommandOutput, runExternalCommand = oldOut, oldRun })
}

// A live confirm dialog on an aa- sandbox pane must be answered with option 1
// (trust/proceed) — sandboxes have no channel and no human to ask.
func TestAutoAnswerSandboxConfirmAnswersRealDialog(t *testing.T) {
	var sent []string
	stubTmuxPane(t, confirmPaneFixture, &sent)

	got := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", "")
	if got == "" {
		t.Fatal("expected a non-empty dialog hash after answering a real dialog")
	}
	if len(sent) != 2 || sent[0] != "1" || sent[1] != "Enter" {
		t.Fatalf("keystrokes = %#v, want [1 Enter]", sent)
	}
}

// Prose containing a numbered list (no selection cursor) must never trigger a
// keystroke — parseConfirmDialog's own cursor requirement is what prevents a
// false positive here.
func TestAutoAnswerSandboxConfirmIgnoresProse(t *testing.T) {
	var sent []string
	stubTmuxPane(t, proseNumberedListFixture, &sent)

	got := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", "")
	if got != "" {
		t.Fatalf("hash = %q, want empty — prose must not parse as a confirm dialog", got)
	}
	if len(sent) != 0 {
		t.Fatalf("keystrokes = %#v, want none", sent)
	}
}

// The defect this finding guards against: only aa- sandboxes may be
// auto-answered. A cc- (or any non-aa-) session name must produce ZERO
// keystrokes even when the pane genuinely shows a confirm dialog — cc-
// bindings keep asking the human via supervisor.go's confirm watchdog, which
// this code path must never bypass.
func TestAutoAnswerSandboxConfirmNeverTouchesCCSession(t *testing.T) {
	var sent []string
	stubTmuxPane(t, confirmPaneFixture, &sent) // a REAL dialog is showing

	got := autoAnswerSandboxConfirm(context.Background(), "cc-somebinding", "")
	if got != "" {
		t.Fatalf("hash = %q, want empty — cc- sessions must never be auto-answered", got)
	}
	if len(sent) != 0 {
		t.Fatalf("keystrokes = %#v, want none — a cc- binding must keep asking the human", sent)
	}
}

// Debounce: the same dialog captured twice in a row (e.g. the pane hasn't
// redrawn yet after the first answer) must produce exactly one [1 Enter]
// pair, not two — a second "1" landing after the dialog already dismissed
// would submit as a stray prompt to a live sandbox.
func TestAutoAnswerSandboxConfirmDebouncesRepeatedDialog(t *testing.T) {
	var sent []string
	stubTmuxPane(t, confirmPaneFixture, &sent)

	h1 := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", "")
	h2 := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", h1)

	if h1 == "" || h1 != h2 {
		t.Fatalf("hash changed across identical dialog captures: h1=%q h2=%q", h1, h2)
	}
	if len(sent) != 2 {
		t.Fatalf("keystrokes = %#v, want exactly one [1 Enter] pair total", sent)
	}
}
