package channelagent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// SandboxDriver runs one goroutine per active sandbox, each repeatedly calling
// RunWorkerOnce against that sandbox's own root.
//
// One goroutine per sandbox rather than a shared loop: RunWorkerOnce blocks for
// the length of a job, and with up to 8 sandboxes a shared loop would serialize
// them behind each other — and if that loop were the cron scheduler's, it would
// also stall scheduling for every production binding.
//
// This is the fix for the defect the whole plan exists for: Inject only ever
// staged a job file in the sandbox's inbox/pending — nothing drained it into
// the tmux pane, so every delegated task sat "working" until the two-hour sweep
// killed it. RunWorkerOnce (worker.go:70) is root-generic — it takes only a
// root and an Injector, with no reference to any registry or Binding — so it
// can drive a sandbox exactly the way it drives a cc- binding.
type SandboxDriver struct {
	root    string
	timeout time.Duration

	mu      sync.Mutex
	running map[string]*sandboxLoop
}

// sandboxLoop tracks one running driver goroutine: cancel requests its exit,
// done is closed once the goroutine has actually returned (and already
// removed itself from SandboxDriver.running). Stop/StopAll block on done —
// merely calling cancel is not enough to guarantee the loop has stopped
// touching the sandbox root, which matters because Stop is used right before
// reclaiming a sandbox's worktree/files.
type sandboxLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewSandboxDriver(root string, timeout time.Duration) *SandboxDriver {
	return &SandboxDriver{root: root, timeout: timeout, running: map[string]*sandboxLoop{}}
}

// Ensure starts a driver for the task's sandbox if one is not already running.
// Safe to call every cycle: a session already being driven is left alone, so
// the caller (a future per-cycle scan over working tasks) never spawns a
// second goroutine for the same sandbox.
func (d *SandboxDriver) Ensure(ctx context.Context, task A2ATask, inj Injector) {
	if task.Session == "" {
		return
	}
	// Only aa- sandboxes are driven (and auto-answer confirm dialogs) this way.
	// A cc- (or any other) session name here means a corrupted task row or a
	// caller miswiring — refuse LOUDLY rather than silently no-oping, so the
	// mistake shows up in logs instead of just looking like a session that
	// never got picked up. cc- bindings must keep going through supervisor.go's
	// confirm watchdog, which still asks the human.
	if !strings.HasPrefix(task.Session, "aa-") {
		fmt.Fprintf(os.Stderr, "a2a driver: refusing to drive session %q — not an aa- sandbox\n", task.Session)
		return
	}
	d.mu.Lock()
	if _, live := d.running[task.Session]; live {
		d.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	d.running[task.Session] = &sandboxLoop{cancel: cancel, done: done}
	d.mu.Unlock()

	go func() {
		defer close(done)
		d.loop(loopCtx, task.Session, inj)
	}()
}

func (d *SandboxDriver) loop(ctx context.Context, session string, inj Injector) {
	defer func() {
		d.mu.Lock()
		delete(d.running, session)
		d.mu.Unlock()
	}()

	sandbox := SandboxRoot(d.root, session)
	// lastConfirmHash debounces the auto-answer: it's the hash of the last
	// dialog this loop actually answered, so a dialog that hasn't dismissed
	// yet by the next capture (pane not redrawn) is not re-typed into — a
	// second "1"+Enter landing after the real dismissal would submit as a
	// stray prompt to a live sandbox. See autoAnswerSandboxConfirm.
	var lastConfirmHash string
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Sandboxes have no channel and no human to ask, so Claude Code's own
		// confirm dialogs (trust folder, "Do you want to proceed?") — which are
		// NOT PreToolUse-gated and therefore invisible to the permission gate —
		// would otherwise block the pane forever: RunWorkerOnce's Inject would
		// see the pane busy (errSessionBusy) and just defer every cycle with no
		// way out. Auto-answer with option 1 (trust / proceed) before attempting
		// delivery; the a2a authorization layer is the actual policy gate here,
		// not a per-dialog human. This reuses the exact same session-name-scoped
		// primitives (capturePane/classifyScreen/parseConfirmDialog/
		// sendConfirmChoice) the cc- confirm watchdog uses in supervisor.go —
		// verified they carry no Binding/registry coupling, so they parse a
		// sandbox pane identically to a binding pane. cc- behaviour (which still
		// asks the human) is untouched: autoAnswerSandboxConfirm refuses any
		// non-aa- session, and this call only ever runs from the sandbox driver
		// loop, never from supervisor.go.
		lastConfirmHash = autoAnswerSandboxConfirm(ctx, session, lastConfirmHash)

		processed, err := RunWorkerOnce(ctx, sandbox, inj, d.timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "a2a driver %s: %v\n", session, err)
		}
		if processed {
			continue // more may be queued; drain before idling
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// autoAnswerSandboxConfirm checks the sandbox's pane and, if it is sitting on
// one of Claude Code's own confirm dialogs, answers it with option 1 (trust /
// proceed) so the loop is never blocked by a prompt nobody can see. Errors
// (no such tmux session, tmux unavailable) resolve to ScreenUnknown/empty pane
// and are silently ignored — the next RunWorkerOnce attempt will simply defer
// again via errSessionBusy if the session truly isn't ready yet.
//
// ONLY aa- sandboxes are ever auto-answered — checked here too (not just in
// Ensure) as defense in depth, since this is the function that actually types
// the keystroke. A cc- binding must keep asking the human via supervisor.go's
// confirm watchdog.
//
// lastHash is the hash (confirmDialog.hash()) of the dialog this function
// last answered for this session, or "" if none. It is returned updated:
// unchanged (echoed back) when the SAME dialog is still on screen — this is
// the debounce that stops a second "1"+Enter from being sent before the pane
// has redrawn past the first one, which would otherwise land as a stray "1"
// submitted as a prompt once the dialog actually dismisses. Reset to "" the
// moment the pane stops showing a confirm dialog at all, so a genuinely new
// dialog (even one with identical text) is still answered.
func autoAnswerSandboxConfirm(ctx context.Context, session, lastHash string) string {
	if !strings.HasPrefix(session, "aa-") {
		return ""
	}
	pane := capturePane(ctx, session)
	if pane == "" || classifyScreen(pane) != ScreenConfirm {
		return ""
	}
	dlg, ok := parseConfirmDialog(pane)
	if !ok {
		return ""
	}
	h := dlg.hash()
	if h == lastHash {
		return lastHash // already answered this exact dialog; pane likely hasn't redrawn yet
	}
	_ = sendConfirmChoice(ctx, session, 1)
	return h
}

// Stop ends the driver for one sandbox and BLOCKS until its goroutine has
// actually exited (not just been asked to). Safe on an unknown session
// (no-op). Callers that reclaim a sandbox's worktree/files right after Stop
// depend on this: a cancel that only requests exit could race a concurrent
// RunWorkerOnce still touching the sandbox root.
func (d *SandboxDriver) Stop(session string) {
	d.mu.Lock()
	l, ok := d.running[session]
	d.mu.Unlock()
	if !ok {
		return
	}
	l.cancel()
	<-l.done
}

// StopAll ends every driver and blocks until all of them have actually
// exited — used on serve shutdown. Cancels are fired first (so every loop
// starts winding down concurrently), then waited on one at a time, so this
// does not serialize N loops' shutdown behind each other.
func (d *SandboxDriver) StopAll() {
	d.mu.Lock()
	loops := make([]*sandboxLoop, 0, len(d.running))
	for _, l := range d.running {
		loops = append(loops, l)
	}
	d.mu.Unlock()
	for _, l := range loops {
		l.cancel()
	}
	for _, l := range loops {
		<-l.done
	}
}

// Running lists the sessions currently being driven.
func (d *SandboxDriver) Running() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.running))
	for s := range d.running {
		out = append(out, s)
	}
	return out
}
