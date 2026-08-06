package channelagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

// EnsureSandboxDrivers is what makes the driver actually run in production: the
// A2A cycle calls it every tick, so a working task must gain a driver and a task
// that has since gone terminal must lose one (its sandbox is about to be
// reclaimed — nothing may still be injecting into it).
func TestEnsureSandboxDriversStartsWorkingAndStopsTerminal(t *testing.T) {
	var sent []string
	stubTmuxPane(t, "", &sent) // no real tmux: capture-pane always returns empty
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	if err := Init(SandboxRoot(root, task.Session)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	store := TaskStore{}
	store.Upsert(task)
	if err := SaveTasks(root, store); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer d.StopAll()

	EnsureSandboxDrivers(ctx, root, d)
	if got := d.Running(); len(got) != 1 || got[0] != task.Session {
		t.Fatalf("Running() = %#v, want [%s]", got, task.Session)
	}
	// Idempotent: a second pass must not spawn a second goroutine.
	EnsureSandboxDrivers(ctx, root, d)
	if got := d.Running(); len(got) != 1 {
		t.Fatalf("Running() = %#v after second pass, want exactly one", got)
	}

	task.State = TaskCompleted
	store = TaskStore{}
	store.Upsert(task)
	if err := SaveTasks(root, store); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	EnsureSandboxDrivers(ctx, root, d)
	if got := d.Running(); len(got) != 0 {
		t.Fatalf("Running() = %#v after the task went terminal, want empty", got)
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

	got, err := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", confirmPaneFixture, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	got, err := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", proseNumberedListFixture, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	got, err := autoAnswerSandboxConfirm(context.Background(), "cc-somebinding", confirmPaneFixture, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	h1, err1 := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", confirmPaneFixture, "")
	h2, err2 := autoAnswerSandboxConfirm(context.Background(), "aa-codereview-c1", confirmPaneFixture, h1)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v, %v", err1, err2)
	}

	if h1 == "" || h1 != h2 {
		t.Fatalf("hash changed across identical dialog captures: h1=%q h2=%q", h1, h2)
	}
	if len(sent) != 2 {
		t.Fatalf("keystrokes = %#v, want exactly one [1 Enter] pair total", sent)
	}
}

// The aa- guard is a strict prefix check, not a substring check: a session
// name that merely CONTAINS "aa-" somewhere in the middle (e.g. because it
// embeds a context id that happens to contain that substring) must still be
// refused — only a session actually NAMED with the aa- sandbox prefix may be
// auto-answered.
func TestAutoAnswerSandboxConfirmRequiresPrefixNotSubstring(t *testing.T) {
	var sent []string
	stubTmuxPane(t, confirmPaneFixture, &sent) // a REAL dialog is showing

	got, err := autoAnswerSandboxConfirm(context.Background(), "cc-aa-not-a-sandbox", confirmPaneFixture, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("hash = %q, want empty — a session merely containing \"aa-\" must not be auto-answered", got)
	}
	if len(sent) != 0 {
		t.Fatalf("keystrokes = %#v, want none", sent)
	}
}

// The aa- prefix check exists on every keystroke-sending site, not just at the
// top of autoAnswerSandboxConfirm — guardedKeystrokes is the shared primitive
// each site (managed-settings gate, login-continue gate, confirm dialog) routes
// through. Unit-tested directly here since it's cheap and precise.
func TestGuardedKeystrokesRefusesNonAASession(t *testing.T) {
	called := false
	err := guardedKeystrokes("cc-somebinding", func() error { called = true; return nil })
	if err == nil {
		t.Fatal("want an error for a non-aa- session")
	}
	if called {
		t.Fatal("send must never be invoked for a non-aa- session")
	}
}

func TestGuardedKeystrokesInvokesSendForAASession(t *testing.T) {
	called := false
	err := guardedKeystrokes("aa-codereview-c1", func() error { called = true; return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("send must be invoked for an aa- session")
	}
}

// This is the Part B wiring itself, proven end to end through the driver —
// not merely that SandboxOutputPrefix/AgentChannelFor format/resolve
// correctly in isolation. A real driver loop must pick up new transcript
// activity and hand it to the wired AgentOutputSink, labelled with the
// task's contextId, for the task's agent's channel.
func TestSandboxDriverStreamsActivityToAgentChannel(t *testing.T) {
	var sentKeys []string
	stubTmuxPane(t, "", &sentKeys) // no real tmux; auto-answer/confirm checks just no-op

	root := t.TempDir()
	agents := AgentStore{}
	if err := agents.Add(Agent{Name: "pm", ChannelID: "chan-pm", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

	task := A2ATask{ContextID: "ctx9", Agent: "pm", Session: SessionNameFor("pm", "ctx9"), State: TaskWorking}
	sandbox := SandboxRoot(root, task.Session)
	if err := Init(sandbox); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Seed a transcript with one assistant "thinking" event already past the
	// activity-tailer's offset — i.e. pre-establish the baseline (as if the
	// tailer had already seen an earlier, shorter transcript) so the very
	// first CollectActivity call inside the driver loop reads it as NEW,
	// rather than CollectActivity's own "first sight of a transcript → skip
	// to EOF, no backlog replay" rule swallowing it before the driver ever
	// gets a chance to emit it.
	tp := sandbox + "-transcript.jsonl"
	line := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"looking into the flaky retry test"}]}}` + "\n"
	if err := os.WriteFile(tp, []byte(line), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := AtomicWriteJSON(pathIn(sandbox, "state", "session.json"), sessionInfo{SessionID: "s1", TranscriptPath: tp}); err != nil {
		t.Fatalf("write session.json: %v", err)
	}
	if err := AtomicWriteJSON(pathIn(sandbox, "state", "activity.json"), activityState{Path: tp, Offset: 0}); err != nil {
		t.Fatalf("write activity.json: %v", err)
	}

	type sent struct{ channelID, text string }
	got := make(chan sent, 8)
	send := func(_ context.Context, channelID, text string) error {
		got <- sent{channelID, text}
		return nil
	}
	sinkCtx, sinkCancel := context.WithCancel(context.Background())
	defer sinkCancel()
	sink := newAgentOutputSink(sinkCtx, root, send)

	d := NewSandboxDriver(root, time.Second)
	d.SetOutputSink(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer d.StopAll()
	d.Ensure(ctx, task, &recordingInjector{})

	// The driver also emits lifecycle lines (e.g. "driver started") through the
	// same sink, so scan every message that arrives — not just the first —
	// for the one carrying the transcript activity.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-got:
			if msg.channelID != "chan-pm" {
				t.Fatalf("channelID = %q, want chan-pm", msg.channelID)
			}
			if !strings.Contains(msg.text, "ctx9") {
				t.Fatalf("text = %q, missing the context label", msg.text)
			}
			if strings.Contains(msg.text, "looking into the flaky retry test") {
				return
			}
		case <-deadline:
			t.Fatal("driver never streamed transcript activity to the agent channel")
		}
	}
}

// The output-sink send must never slow down (let alone stall) the driver's
// actual job — a hung/slow agent-channel send is a visibility loss, not
// something that may delay message delivery to the sandbox's real caller.
func TestSandboxDriverDeliversJobWhileAgentChannelSendHangs(t *testing.T) {
	var sent []string
	stubTmuxPane(t, "", &sent) // no real tmux: capture-pane always returns empty

	root := t.TempDir()
	agents := AgentStore{}
	if err := agents.Add(Agent{Name: "codereview", ChannelID: "chan-cr", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}

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

	// A send that hangs until the test releases it — standing in for a
	// throttled/429'd/hung Discord channel.
	release := make(chan struct{})
	slowSend := func(ctx context.Context, _, _ string) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	sinkCtx, sinkCancel := context.WithCancel(context.Background())
	defer func() { close(release); sinkCancel() }()
	sink := newAgentOutputSink(sinkCtx, root, slowSend)

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, 2*time.Second)
	d.SetOutputSink(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer d.StopAll()
	d.Ensure(ctx, task, inj)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && inj.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if inj.count() == 0 {
		t.Fatal("driver never delivered the staged job — a slow agent-channel send blocked it")
	}
}

const managedSettingsPaneFixture = `
 Managed settings require approval

 This project has managed settings. Do you trust these settings?

 ❯ 1. Yes, I trust these settings
   2. No, exit
`

const loginContinuePaneFixture = `
 Login successful.

 Press Enter to continue…
`

const loggedOutPaneFixture = `
 Invalid authentication credentials

 Please run /login to authenticate
`

// 這條修正真正的重點：命中任一畫面分支就 skip 本輪 RunWorkerOnce。paneBusy 不
// 把 ScreenLogin 算成忙碌（adapters.go:208-217），所以 RunWorkerOnce 會照常
// typeAndSubmit，把 prompt 打進核准畫面；驗證條件是「輸入框裡還有沒有字」
// （adapters.go:94），核准畫面沒有輸入框 → Inject 回報成功、job 移進 done、
// prompt 消失。skip 掉才擋得住。
//
// This also pins the debounce (review Important 1): the fixture pane never
// changes, so without a debounce the gate would be re-answered every tick for
// as long as the driver runs. driverPollInterval is sped up so the test can
// observe several ticks — and therefore several chances to double-send —
// without a multi-second sleep.
func TestSandboxDriverSkipsWorkOnManagedSettingsGate(t *testing.T) {
	var sent []string
	stubTmuxPane(t, managedSettingsPaneFixture, &sent)
	oldPoll := driverPollInterval
	driverPollInterval = 30 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 5 * time.Millisecond
	defer func() { injectSubmitDelay = oldDelay }()

	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	sandbox := SandboxRoot(root, task.Session)
	if err := Init(sandbox); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestMessages(context.Background(), sandbox, []SourceMessage{{
		Platform: "a2a", ChannelID: "c1", MessageID: "m1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Content: "do the thing",
	}}); err != nil {
		t.Fatal(err)
	}

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, inj)
	time.Sleep(500 * time.Millisecond) // many ticks at the sped-up poll interval
	d.StopAll()

	if inj.count() != 0 {
		t.Fatalf("driver injected %d job(s) into a managed-settings approval screen; the prompt would have vanished", inj.count())
	}
	if len(sent) != 2 || sent[0] != "1" || sent[1] != "Enter" {
		t.Fatalf("keystrokes = %#v, want the gate answered EXACTLY ONCE with [1 Enter] — a persisting gate must not be re-answered every tick", sent)
	}
	// 該 job 必須還在 pending，沒有被 RunWorkerOnce 消化掉。
	entries, _ := os.ReadDir(pathIn(sandbox, "inbox", "pending"))
	if len(entries) != 1 {
		t.Fatalf("pending jobs = %d, want the job still queued", len(entries))
	}
}

// Same debounce guarantee (review Important 1) for the OTHER new gate branch:
// a persisting login-continue screen must get exactly one bare Enter, not one
// per tick.
func TestSandboxDriverAdvancesLoginContinueScreen(t *testing.T) {
	var sent []string
	stubTmuxPane(t, loginContinuePaneFixture, &sent)
	oldPoll := driverPollInterval
	driverPollInterval = 30 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()

	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	time.Sleep(400 * time.Millisecond) // many ticks at the sped-up poll interval
	d.StopAll()

	if len(sent) != 1 || sent[0] != "Enter" {
		t.Fatalf("keystrokes = %#v, want a SINGLE bare Enter — a persisting gate must not be re-answered every tick", sent)
	}
}

// managedSettingsPaneFixtureResized carries the exact two substrings
// paneAwaitingManagedSettings requires, and is still the SAME gate as
// managedSettingsPaneFixture — but differs in incidental whitespace/line
// layout (one blank line collapsed), standing in for a resize reflow or the
// selection-highlight repaint after our own "1" lands. Neither of those is a
// new occurrence of the gate.
const managedSettingsPaneFixtureResized = `
 Managed settings require approval
 This project has managed settings. Do you trust these settings?
 ❯ 1. Yes, I trust these settings
   2. No, exit
`

// TestSandboxDriverGateDebounceSurvivesIncidentalRedraw pins review Minor 1:
// gateHash must key off the gate's STABLE identity (which of the two known
// gates this is), the way confirmDialog.hash() keys off the parsed
// question+options rather than raw pane text. The old gateHash hashed the
// whole (ANSI-stripped, lowercased) pane — so ANY incidental redraw while the
// SAME gate was still up (a resize, the selection highlight repainting after
// our own "1" lands) changed the hash and made the loop treat it as a brand
// new occurrence, re-sending [1 Enter]. That stray "1" would land in the
// input box of a session that has since gone idle and submit as a prompt —
// exactly the risk the debounce exists to prevent. This test alternates
// between two textually different renders of the SAME gate every tick; the
// gate must still be answered exactly once.
func TestSandboxDriverGateDebounceSurvivesIncidentalRedraw(t *testing.T) {
	oldOut, oldRun := runExternalCommandOutput, runExternalCommand
	defer func() { runExternalCommandOutput, runExternalCommand = oldOut, oldRun }()
	oldPoll := driverPollInterval
	driverPollInterval = 20 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()
	oldDelay := injectSubmitDelay
	injectSubmitDelay = 5 * time.Millisecond
	defer func() { injectSubmitDelay = oldDelay }()

	var mu sync.Mutex
	var sent []string
	tick := 0
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		tick++
		if tick%2 == 1 {
			return managedSettingsPaneFixture, nil
		}
		return managedSettingsPaneFixtureResized, nil
	}
	runExternalCommand = func(_ context.Context, _ string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, args[len(args)-1])
		return nil
	}

	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	time.Sleep(500 * time.Millisecond) // many ticks, alternating render each time
	d.StopAll()

	if len(sent) != 2 || sent[0] != "1" || sent[1] != "Enter" {
		t.Fatalf("keystrokes = %#v, want the gate answered EXACTLY ONCE with [1 Enter] despite incidental pane redraws between ticks", sent)
	}
}

// gateHash must depend only on which gate this is, not on any pane text —
// see TestSandboxDriverGateDebounceSurvivesIncidentalRedraw for the
// behavioural version of this guarantee through the full driver loop.
func TestGateHashDependsOnlyOnKind(t *testing.T) {
	if gateHash("managedSettings") != gateHash("managedSettings") {
		t.Fatal("gateHash must be stable for the same gate kind")
	}
	if gateHash("managedSettings") == gateHash("loginContinue") {
		t.Fatal("gateHash must distinguish the two different gate kinds")
	}
}

// 真的登出時，沙盒永遠不驅動登入流程：那是 operator 的事，一個沙盒去操作
// /login 會動到全機共用的憑證。任務標 failed 並停掉本 driver — 但要連續
// loginFailureStrikes 拍都判為登出才失敗（review Critical），所以這裡把
// driverPollInterval 調快，讓測試不用真的等好幾秒。
func TestSandboxDriverFailsTaskWhenLoggedOut(t *testing.T) {
	var sent []string
	stubTmuxPane(t, loggedOutPaneFixture, &sent)
	oldPoll := driverPollInterval
	driverPollInterval = 30 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()

	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))
	_ = WithTasks(root, func(s *TaskStore) error { s.Upsert(task); return nil })

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, inj)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(d.Running()) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := d.Running(); len(got) != 0 {
		t.Fatalf("driver still running on a logged-out session: %#v", got)
	}
	tasks, _ := LoadTasks(root)
	tk, _ := tasks.ByContext("c1")
	if tk.State != TaskFailed || !strings.Contains(tk.Detail, "login") {
		t.Fatalf("task = %q / %q, want failed with a login detail", tk.State, tk.Detail)
	}
	// Meaningful in place of the old vacuous loop (review Minor 4): a genuine,
	// persisting logout must never send ANY keystroke (unlike the two known
	// gates, which do send [1 Enter] / [Enter]) — the sandbox only ever
	// observes and, once persistent, fails; it never types anything back,
	// /login included.
	if len(sent) != 0 {
		t.Fatalf("keystrokes = %#v, want none — a sandbox must never drive the login flow", sent)
	}
	if inj.count() != 0 {
		t.Fatalf("driver injected %d job(s) into a session it was about to fail for being logged out", inj.count())
	}
}

// screenGrepHitFixture 模擬一個健康沙盒正在讀/grep screen.go 或 relogin.go（或
// 顯示一份引用了它們字面字串的 diff）：畫面上出現了 classifyScreen 視為「確
// 鑿」的登出片語（screen.go:38 的 "not logged in"），但這只是檔案內容，不是
// 真的登出。這正是 review 抓到的 Critical 缺陷：單拍命中就判定失敗，會讓一個
// 健康的沙盒因為讀自己的原始碼而被判死。
const screenGrepHitFixture = `
 ● Bash(grep -n "not logged in" internal/channelagent/screen.go)
   ⎿  38:  for _, sig := range []string{"invalid authentication credentials", "not logged in", "/login to authenticate"} {

`

// healthyIdlePaneFixture 是完全正常、沒有任何登入相關字串的待命畫面。
const healthyIdlePaneFixture = "\n 好，已經看完了。\n\n ❯ \n"

// screenGrepHitWhileWorkingFixture is screenGrepHitFixture's payload PLUS the
// spinner status line Claude Code renders while a turn is actually running
// ("esc to interrupt · <elapsed> · ↓ <tokens>"). This is the actual review
// Critical repro, not the old round's alternating fixture: a healthy sandbox
// mid-tool-call has BOTH the grep hit's literal phrase AND the working
// indicator on the SAME capture, and the phrase can stay on screen — with the
// spinner still up — for tens of seconds, not just one tick.
const screenGrepHitWhileWorkingFixture = `
 ● Bash(grep -n "not logged in" internal/channelagent/screen.go)
   ⎿  38:  for _, sig := range []string{"invalid authentication credentials", "not logged in", "/login to authenticate"} {

 ✳ Thinking… (esc to interrupt · 12s · ↓ 3.2k tokens)
`

// TestSandboxDriverToleratesPersistentLoginPhraseWhileWorking pins the review
// Critical fix, reproduced faithfully this time: the PREVIOUS round's test
// (TestSandboxDriverToleratesTransientLoginPhraseInHealthyPane, since removed)
// alternated the grep-hit fixture with a completely clean pane every other
// tick — exactly the one shape that survives ANY strike count trivially,
// because the phrase never appeared on two consecutive captures, so the
// strike counter (and therefore the defect it was meant to catch) was never
// actually exercised. Real healthy sandboxes don't clear the screen between
// ticks; the phrase plus the spinner sit on the SAME capture, unchanged, for
// as long as the tool call/turn keeps running. This fixture never changes and
// is held for far longer than the (now-raised) loginFailureStrikes count —
// the task must never be failed while the pane keeps showing the active-work
// indicator, no matter how many ticks pass.
func TestSandboxDriverToleratesPersistentLoginPhraseWhileWorking(t *testing.T) {
	var sent []string
	stubTmuxPane(t, screenGrepHitWhileWorkingFixture, &sent)
	oldPoll := driverPollInterval
	driverPollInterval = 10 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()

	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))
	_ = WithTasks(root, func(s *TaskStore) error { s.Upsert(task); return nil })

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	// (loginFailureStrikes+20) 拍，遠遠超過門檻 —— 連舊的 3 拍門檻都撐不住這
	// 個常數畫面，證明擋下失敗的是 spinner 檢查本身，不是門檻高低。
	time.Sleep(time.Duration(loginFailureStrikes+20) * 10 * time.Millisecond)
	d.StopAll()

	tasks, _ := LoadTasks(root)
	tk, _ := tasks.ByContext("c1")
	if tk.State == TaskFailed {
		t.Fatalf("task failed while the pane still showed the active-working indicator: state=%q detail=%q", tk.State, tk.Detail)
	}
	if len(sent) != 0 {
		t.Fatalf("keystrokes = %#v, want none — this pane is not one of the two known gates", sent)
	}
}

// TestSandboxDriverToleratesTenConsecutiveLoginPhraseHitsBelowRaisedThreshold
// pins the OTHER half of the review Critical fix — raising loginFailureStrikes
// from 3 to a number that reflects a real logout — independently of the
// spinner check above. Ten consecutive hits with NO working indicator at all
// must still not fail the task under the raised threshold, even though it
// would have failed at (old) loginFailureStrikes=3 by the third hit.
func TestSandboxDriverToleratesTenConsecutiveLoginPhraseHitsBelowRaisedThreshold(t *testing.T) {
	const hits = 10
	if loginFailureStrikes <= hits {
		t.Fatalf("test assumption broken: loginFailureStrikes=%d must exceed %d for this test to discriminate the raised threshold", loginFailureStrikes, hits)
	}
	oldOut, oldRun := runExternalCommandOutput, runExternalCommand
	defer func() { runExternalCommandOutput, runExternalCommand = oldOut, oldRun }()
	oldPoll := driverPollInterval
	driverPollInterval = 10 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()

	var mu sync.Mutex
	tick := 0
	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		tick++
		if tick <= hits {
			// 連續 hits 拍都是確鑿字串（沒有 spinner），模擬一次持續數秒但終
			// 究會過去的殘留畫面。
			return screenGrepHitFixture, nil
		}
		return healthyIdlePaneFixture, nil
	}
	runExternalCommand = func(_ context.Context, _ string, _ ...string) error { return nil }

	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))
	_ = WithTasks(root, func(s *TaskStore) error { s.Upsert(task); return nil })

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	time.Sleep(time.Duration(loginFailureStrikes+20) * 10 * time.Millisecond)
	d.StopAll()

	tasks, _ := LoadTasks(root)
	tk, _ := tasks.ByContext("c1")
	if tk.State == TaskFailed {
		t.Fatalf("task failed after only %d consecutive hits, below loginFailureStrikes=%d: state=%q detail=%q", hits, loginFailureStrikes, tk.State, tk.Detail)
	}
}

// TestSandboxDriverSurfacesGateSendKeysFailure pins the review Important 2
// fix: when send-keys itself fails on one of the two gate branches, the error
// must reach the agent channel (through the same throttle RunWorkerOnce's
// errors use) instead of being discarded with `_ =`. Before the fix a
// permanently failing send-keys reproduced a silent hang indistinguishable
// from the one this whole task exists to remove — the loop would skip
// RunWorkerOnce forever and never tell anyone why.
func TestSandboxDriverSurfacesGateSendKeysFailure(t *testing.T) {
	oldOut, oldRun := runExternalCommandOutput, runExternalCommand
	defer func() { runExternalCommandOutput, runExternalCommand = oldOut, oldRun }()
	oldPoll := driverPollInterval
	driverPollInterval = 20 * time.Millisecond
	defer func() { driverPollInterval = oldPoll }()

	runExternalCommandOutput = func(_ context.Context, _ string, _ ...string) (string, error) {
		return managedSettingsPaneFixture, nil
	}
	sendErr := errors.New("boom: tmux not reachable")
	runExternalCommand = func(_ context.Context, _ string, _ ...string) error { return sendErr }

	root := t.TempDir()
	agents := AgentStore{}
	if err := agents.Add(Agent{Name: "pm", ChannelID: "chan-pm", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveAgents(root, agents); err != nil {
		t.Fatalf("SaveAgents: %v", err)
	}
	task := A2ATask{ContextID: "c1", Agent: "pm", Session: SessionNameFor("pm", "c1"), State: TaskWorking}
	if err := Init(SandboxRoot(root, task.Session)); err != nil {
		t.Fatal(err)
	}

	type sentLine struct{ channelID, text string }
	got := make(chan sentLine, 32)
	send := func(_ context.Context, channelID, text string) error {
		got <- sentLine{channelID, text}
		return nil
	}
	sinkCtx, sinkCancel := context.WithCancel(context.Background())
	defer sinkCancel()
	sink := newAgentOutputSink(sinkCtx, root, send)

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, time.Second)
	d.SetOutputSink(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, inj)
	defer d.StopAll()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-got:
			if strings.Contains(msg.text, sendErr.Error()) {
				if inj.count() != 0 {
					t.Fatalf("driver injected %d job(s) while the gate could not even be answered", inj.count())
				}
				return
			}
		case <-deadline:
			t.Fatal("send-keys failure on the managed-settings gate was never surfaced to the agent channel")
		}
	}
}

// 一個卡住的沙盒每輪都會失敗；沒有去重與退避時最長兩小時可以往 agent 頻道
// 推約 7200 則。
func TestDriverErrorThrottleDeduplicatesAndCaps(t *testing.T) {
	now := time.Now()
	th := newDriverErrorThrottle()
	if !th.allow("boom", now) {
		t.Fatal("first occurrence must be emitted")
	}
	if th.allow("boom", now.Add(30*time.Second)) {
		t.Fatal("the same error within 60s must be suppressed")
	}
	if !th.allow("boom", now.Add(61*time.Second)) {
		t.Fatal("the same error after 60s must be emitted again")
	}
	// 每分鐘 60 行的上限：61 個各不相同的錯誤，最後一個要被擋。
	th2 := newDriverErrorThrottle()
	for i := 0; i < 60; i++ {
		if !th2.allow(fmt.Sprintf("e%d", i), now) {
			t.Fatalf("distinct error %d must be emitted", i)
		}
	}
	if th2.allow("e60", now) {
		t.Fatal("the 61st line in one minute must be suppressed")
	}
}

// review Minor 2: the ORIGINAL version of this test (kept here in the
// comment, not the code) filled the cap with 60 entries all timestamped at
// t=59s, then asserted a 61st was still blocked at t=61s. That assertion
// holds under BOTH a genuine rolling window AND a tumbling one whose anchor
// happens to be set by the first call (t=59s): from either implementation's
// perspective only 2s have elapsed since the anchor, so no reset fires
// either way — the test could not fail against the pre-fix tumbling code, it
// only happened to also pass against the post-fix rolling code. A test that
// cannot fail proves nothing.
//
// The actual distinguishing behaviour between rolling and tumbling is HOW
// capacity is freed as time passes, not merely "does old traffic still
// count shortly after." A rolling window frees capacity one entry at a time,
// exactly as each individual entry ages past 60s. A tumbling window frees
// capacity in one lump when the whole bucket resets. So: fill the cap with
// 60 DISTINCT entries spread one per second (t=0..59s) — at any point during
// that fill, no entry is yet 60s old, so nothing is pruned and all 60 succeed
// (same as the dedup/cap test). Then, at t=60.5s — just past when the SINGLE
// t=0s entry (and only that one) has aged out of the last-60s window — a new
// distinct message must be admitted (exactly one slot freed). Immediately
// asking for a SECOND new message at the same instant must still be
// refused: only one entry (t=0s) has aged out so far, entries from t=1s
// onward are all still within the window. A tumbling implementation that
// resets its whole bucket once 60s have elapsed since its anchor would
// instead admit a burst of new messages right at that boundary, not exactly
// one — so this genuinely fails against a reintroduced tumbling
// implementation, unlike the original assertion.
func TestDriverErrorThrottleIsRollingNotTumbling(t *testing.T) {
	th := newDriverErrorThrottle()
	base := time.Now()
	for i := 0; i < 60; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		if !th.allow(fmt.Sprintf("a%d", i), at) {
			t.Fatalf("distinct error a%d at t=%ds must be emitted while filling the window", i, i)
		}
	}
	justPastFirstExpiry := base.Add(60*time.Second + 500*time.Millisecond)
	if !th.allow("f0", justPastFirstExpiry) {
		t.Fatal("a rolling window must free exactly the one slot (t=0s) that has aged past 60s")
	}
	if th.allow("f1", justPastFirstExpiry) {
		t.Fatal("only ONE slot aged out — a second brand-new message at the same instant must still be suppressed, not admitted by a whole-bucket reset")
	}
}
