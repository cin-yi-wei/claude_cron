package channelagent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
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

	// sink delivers per-sandbox visibility lines (lifecycle, errors, transcript
	// activity) to each task's agent channel. Guarded by its own mutex (not mu)
	// since SetOutputSink is typically called once at startup while loop()
	// goroutines may already be reading it. Nil is a valid, common state (no
	// AgentOutputSink wired, e.g. most tests): bindOutputChannel then returns
	// the zero AgentChannel, which is a silent no-op — exactly like an agent
	// with no ChannelID.
	sinkMu sync.RWMutex
	sink   *AgentOutputSink
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

// SetOutputSink wires the driver's per-sandbox visibility lines (lifecycle,
// errors, and transcript activity — see loop()) to sink, which delivers each
// line to its task's agent channel. Safe to call before or after Ensure has
// started loops: bindOutputChannel reads the field under sinkMu. Passing nil
// (the zero-value default) turns emission back off.
func (d *SandboxDriver) SetOutputSink(sink *AgentOutputSink) {
	d.sinkMu.Lock()
	d.sink = sink
	d.sinkMu.Unlock()
}

// bindOutputChannel resolves task.Agent's output channel ONCE (a single
// agents.json read via AgentOutputSink.Bind) and returns a handle whose
// SendLine calls do no further disk I/O for the rest of this loop's
// lifetime — see loop(), which calls this exactly once per sandbox rather
// than once per emitted line. No sink wired → the zero AgentChannel, which
// is a silent, I/O-free no-op, exactly mirroring an agent with no ChannelID.
func (d *SandboxDriver) bindOutputChannel(task A2ATask) AgentChannel {
	d.sinkMu.RLock()
	sink := d.sink
	d.sinkMu.RUnlock()
	return sink.Bind(task.Agent)
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
		d.loop(loopCtx, task, inj)
	}()
}

// loop drives one sandbox until ctx is cancelled. task is the SNAPSHOT Ensure
// was called with — it is only used for its Session/Agent/ContextID/Worktree
// identity (labelling emitted lines and locating the sandbox/transcript), not
// re-read from tasks.json here, so a Prompt/Detail/State change elsewhere
// during the loop's lifetime is irrelevant to it.
func (d *SandboxDriver) loop(ctx context.Context, task A2ATask, inj Injector) {
	session := task.Session
	defer func() {
		d.mu.Lock()
		delete(d.running, session)
		d.mu.Unlock()
	}()

	sandbox := SandboxRoot(d.root, session)
	// Resolved ONCE for this loop's whole lifetime (see bindOutputChannel) —
	// not per line — so per-line emission below (a CollectActivity burst can
	// yield many lines per tick) never re-reads agents.json from disk.
	channel := d.bindOutputChannel(task)
	channel.SendLine(task.ContextID, "🟢 driver started")
	defer channel.SendLine(task.ContextID, "🔴 driver stopped")
	// lastAnsweredHash debounces every auto-answer path (confirm dialog,
	// managed-settings gate, login-continue gate): it's the hash of the last
	// screen this loop actually answered, so a screen that hasn't dismissed
	// yet by the next capture (pane not redrawn) is not re-typed into — a
	// second "1"/"Enter" landing after the real dismissal would submit as a
	// stray prompt to a live sandbox. Reset to "" the moment the tick's
	// classification stops matching whatever produced the stored hash, so a
	// genuinely new occurrence (even with identical text) is still answered.
	// See autoAnswerSandboxConfirm and gateHash.
	var lastAnsweredHash string
	// loginStrikes counts CONSECUTIVE ticks classified as a genuine login
	// failure (i.e. ScreenLogin but neither of the two known, answerable
	// post-login gates). A real logout persists tick over tick; a sandbox
	// grepping/reading screen.go or relogin.go (or displaying a diff that
	// quotes their literal phrases) renders those same substrings in its own
	// pane for one capture and then moves on — see loginFailureStrikes.
	var loginStrikes int
	// throttle bounds how many "⚠️ <err>" lines this loop can push to the
	// agent channel: a sandbox stuck failing every tick has no other backoff,
	// and without this a two-hour hang can push ~7200 identical lines before
	// the sweep finally kills it. See driverErrorThrottle.
	throttle := newDriverErrorThrottle()
	// reportErr is the single place that turns an in-loop error into visible
	// output — stderr always, the agent channel when the throttle allows it.
	// Every keystroke-sending site below routes its error here instead of
	// discarding it: a silently-failing send-keys (tmux gone, session dead)
	// must not reproduce the exact silent hang this task exists to remove.
	reportErr := func(err error) {
		fmt.Fprintf(os.Stderr, "a2a driver %s: %v\n", session, err)
		if throttle.allow(err.Error(), time.Now()) {
			channel.SendLine(task.ContextID, "⚠️ "+err.Error())
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Sandboxes have no channel and no human to ask, so Claude Code's own
		// screens that the permission hook does not cover — folder trust, the
		// managed-settings approval gate, "Do you want to proceed?", or a
		// genuine logout — would otherwise block the pane forever:
		// RunWorkerOnce's Inject would see the pane busy (errSessionBusy) for a
		// confirm dialog, or — worse — for the managed-settings/login-continue
		// gates, which classifyScreen reports as ScreenLogin and paneBusy does
		// NOT treat as busy (adapters.go:208-217) — Inject would type the
		// prompt straight into the approval screen and report success, and the
		// prompt would simply vanish. Capture the pane exactly ONCE per loop
		// iteration (capture-pane is a fork/exec; with up to 8 sandboxes ticking
		// every second, a second capture would double that syscall traffic) and
		// branch every screen decision off this single snapshot.
		pane := capturePane(ctx, session)
		low := strings.ToLower(stripANSI(pane))
		skip := false
		switch {
		case pane == "":
			// 抓不到畫面（session 還沒起來 / tmux 不可用）：交給 RunWorkerOnce
			// 的 errSessionBusy 路徑處理，不要在這裡猜；也不要把這次的空拍當
			// 成任何登入狀態消失的證據。
			lastAnsweredHash = ""
			loginStrikes = 0
		default:
			switch classifyScreen(pane) {
			case ScreenConfirm:
				// Sandboxes have no channel and no human to ask, so Claude
				// Code's own confirm dialogs (trust folder, "Do you want to
				// proceed?") — which are NOT PreToolUse-gated and therefore
				// invisible to the permission gate — are answered with option
				// 1 (trust / proceed) here; the a2a authorization layer is the
				// actual policy gate, not a per-dialog human. This reuses the
				// exact same session-name-scoped primitives
				// (classifyScreen/parseConfirmDialog/sendConfirmChoice) the
				// cc- confirm watchdog uses in supervisor.go — verified they
				// carry no Binding/registry coupling, so they parse a sandbox
				// pane identically to a binding pane. cc- behaviour (which
				// still asks the human) is untouched: autoAnswerSandboxConfirm
				// refuses any non-aa- session, and this call only ever runs
				// from the sandbox driver loop, never from supervisor.go.
				h, err := autoAnswerSandboxConfirm(ctx, session, pane, lastAnsweredHash)
				lastAnsweredHash = h
				if err != nil {
					reportErr(fmt.Errorf("auto-answer confirm dialog: %w", err))
				}
				skip = true
				loginStrikes = 0
			case ScreenLogin:
				// screen.go collapses THREE different situations into
				// ScreenLogin: two known, safe-to-answer post-login gates
				// (managed settings / login continue, screen.go:55-57), and a
				// genuine logout (screen.go:38's conclusive phrases, matched
				// ANYWHERE in the pane — including a healthy sandbox's own
				// grep/Read output while it works on THIS repo's screen.go or
				// relogin.go, or a diff that quotes them). The conclusive
				// phrases take priority over the gate match — mirroring
				// classifyScreen's own order, where the phrase check
				// (screen.go:38) runs before the gate check (screen.go:55) —
				// so a pane that happens to carry both never gets stuck
				// re-answering a gate it will never actually need to answer
				// again. See loginConclusivePhrases.
				switch {
				case !paneHasConclusiveLoginPhrase(low) && paneAwaitingManagedSettings(low):
					// screen.go:180。SelectTrustSettings 是 supervisor.go 的
					// 登入 watchdog 用的同一個 helper；沙盒沒有人可以問，得自
					// 己回這個閘。
					h := gateHash("managedSettings")
					if h != lastAnsweredHash {
						if err := guardedKeystrokes(session, func() error {
							return TmuxInjector{Session: session}.SelectTrustSettings(ctx)
						}); err != nil {
							reportErr(fmt.Errorf("answer managed-settings gate: %w", err))
						} else {
							lastAnsweredHash = h
						}
					}
					skip = true
					loginStrikes = 0
				case !paneHasConclusiveLoginPhrase(low) && paneAwaitingLoginContinue(low):
					// screen.go:174。同上，送一個 Enter 推進去。
					h := gateHash("loginContinue")
					if h != lastAnsweredHash {
						if err := guardedKeystrokes(session, func() error {
							return TmuxInjector{Session: session}.PressEnter(ctx)
						}); err != nil {
							reportErr(fmt.Errorf("answer login-continue gate: %w", err))
						} else {
							lastAnsweredHash = h
						}
					}
					skip = true
					loginStrikes = 0
				default:
					// 確鑿字串（真的登出，或至少不是這兩個已知安全的閘），或
					// classifyScreen 其他子情況（select-login-method / paste
					// code / URL / transient「請執行 /login」）。沙盒永遠不
					// 驅動登入流程：那是 operator 的事，一個沙盒去操作 /login
					// 會動到全機共用的憑證。
					//
					// 但這裡還不能就此判定失敗：如果畫面同時還亮著 spinner（esc
					// to interrupt / ↓ tokens），這個沙盒明明還在工作——真的登出
					// 的畫面不會有 spinner，這幾乎必然是它自己在 grep/Read
					// screen.go 或 relogin.go（或顯示一份引用了這些字面字串的
					// diff），字面字串恰好落在這一拍的截圖裡。這道檢查跟
					// loginStrikes 是兩道獨立防線：只要 spinner 還在，這一拍就
					// 完全不計 strike，不管門檻設多少——「還在工作」本身就是
					// 判定「不是登出」的直接證據，不必等連續多拍才確認。見
					// paneIsActivelyWorking。
					//
					// spinner 消失之後才看 loginStrikes：同一段話只要連續
					// loginFailureStrikes 拍都還在，才真的像是登出而不是一次性
					// 的 grep 命中；worktree 保留供 forensics。
					if paneIsActivelyWorking(low) {
						lastAnsweredHash = ""
						loginStrikes = 0
					} else {
						lastAnsweredHash = ""
						loginStrikes++
						if loginStrikes >= loginFailureStrikes {
							markSandboxLoginFailure(d.root, task, channel)
							return
						}
					}
					skip = true
				}
			default:
				lastAnsweredHash = ""
				loginStrikes = 0
			}
		}
		// 命中任一畫面分支就 skip 本輪 RunWorkerOnce —— 這是這條修正真正的
		// 重點。paneBusy 不把 ScreenLogin 算成忙碌（adapters.go:208-217），
		// RunWorkerOnce 會把 prompt 打進核准畫面並回報成功（adapters.go:94
		// 的驗證條件在無輸入框的畫面上必然為 false），prompt 就此消失。
		if skip {
			select {
			case <-ctx.Done():
				return
			case <-time.After(driverPollInterval):
			}
			continue
		}

		processed, err := RunWorkerOnce(ctx, sandbox, inj, d.timeout)
		if err != nil {
			reportErr(err)
		}
		// Stream the sandbox's transcript activity (thinking/tool-use) to its
		// agent channel — the "what is this sandbox actually doing" visibility
		// the whole agent-channel feature exists for. Reuses CollectActivity
		// verbatim (same offset-tracked tailing cc- bindings use), rooted at
		// the sandbox's own root/worktree so its state/activity.json offset is
		// independent of any cc- binding's.
		for _, line := range CollectActivity(sandbox, task.Worktree) {
			channel.SendLine(task.ContextID, line)
		}
		if processed {
			continue // more may be queued; drain before idling
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(driverPollInterval):
		}
	}
}

// markSandboxLoginFailure 把任務標成 failed 並在 agent 頻道留一行。worktree
// 保留（依 2026-08-05 規格第 124 行的 forensics 規則），由 sweep 的
// MaxRetainedFailedSandboxes 上限約束。
func markSandboxLoginFailure(root string, task A2ATask, channel AgentChannel) {
	const detail = "sandbox session needs login"
	_ = WithTasks(root, func(tasks *TaskStore) error {
		cur, ok := tasks.ByContext(task.ContextID)
		if !ok || !CanTransition(cur.State, TaskFailed) {
			return nil
		}
		cur.State = TaskFailed
		cur.Detail = detail
		cur.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		tasks.Upsert(cur)
		return nil
	})
	channel.SendLine(task.ContextID, "🔴 "+detail)
}

// driverErrorThrottle 讓一個卡住的沙盒不會把 agent 頻道灌爆：同一段錯誤文字
// 60 秒內最多一行，且每個 session 在任何連續 60 秒的滾動視窗內最多 60 行。
// 沒有它時，一個 session 消失的沙盒會在兩小時硬逾時前推出約 7200 則
// （a2a_driver.go 原本的錯誤路徑無去重、無退避、無次數上限）。
//
// emitted 是真的「滾動」視窗（每次呼叫都把 60 秒前的時間戳丟掉再判斷），不是
// tumbling window：tumbling window 用固定時間格（例如「這一分鐘」）算數，會在
// 格與格的邊界重置，讓 t=59s 塞滿 60 行、t=61s 又能再塞 60 行——同一段 60 秒
// 裡實際跑出 120 行，量能上限形同虛設。
type driverErrorThrottle struct {
	lastSeen map[string]time.Time
	emitted  []time.Time
}

func newDriverErrorThrottle() *driverErrorThrottle {
	return &driverErrorThrottle{lastSeen: map[string]time.Time{}}
}

func (t *driverErrorThrottle) allow(msg string, now time.Time) bool {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(t.emitted) && t.emitted[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		t.emitted = t.emitted[i:]
	}
	if len(t.emitted) >= 60 {
		return false
	}
	if last, ok := t.lastSeen[msg]; ok && now.Sub(last) < time.Minute {
		return false
	}
	// map 只受同一 session 的相異錯誤文字數量約束；超過 256 種就整批清空，
	// 避免一個會產生唯一錯誤字串的失敗模式把記憶體吃光。
	if len(t.lastSeen) > 256 {
		t.lastSeen = map[string]time.Time{}
	}
	t.lastSeen[msg] = now
	t.emitted = append(t.emitted, now)
	return true
}

// driverPollInterval is how long loop sleeps between ticks after a skipped or
// idle iteration. A package var (like injectSubmitDelay/injectVerifyDelay in
// adapters.go) purely so tests can speed it up without changing behaviour —
// production always runs at the real time.Second default.
var driverPollInterval = time.Second

// loginFailureStrikes is how many CONSECUTIVE ticks must classify a pane as a
// genuine login failure (ScreenLogin, neither known post-login gate, AND not
// actively working — see paneIsActivelyWorking) before the task is actually
// failed. A real logout persists tick over tick, indefinitely, until an
// operator intervenes — there is no cost to waiting tens of ticks for
// certainty. A healthy sandbox that greps/reads screen.go or relogin.go, or
// renders a diff quoting their literal phrases, can leave that text sitting
// on the pane for the rest of a long tool call or turn — tens of SECONDS at
// this driver's ~1s poll interval, not merely one capture. 3 strikes (pinned
// by an earlier round as "small enough that a genuine logout still fails
// within a few ticks") tolerated only about three seconds of that — nowhere
// near enough, and it is what let a healthy sandbox reading its own source
// code get marked failed and abandoned. Raised to 30 (~30s at the real
// driverPollInterval): still resolves a genuine logout in well under a
// minute, but gives comfortable headroom over how long a single grep/Read
// tool call's output can plausibly linger on screen without the working
// indicator (paneIsActivelyWorking) also being true — which is checked FIRST
// and is the primary defense; this count is the backstop for the moments
// between tool calls where the spinner may not be up yet a stale phrase is
// still on screen.
const loginFailureStrikes = 30

// loginConclusivePhrases mirrors screen.go:38's list. classifyScreen treats
// any of these as conclusive proof of a genuine logout no matter where in the
// pane they appear — that is the root cause of the false positive this driver
// must not reproduce for its OWN "is this the known safe gate, or a real
// failure" decision. screen.go is out of scope for this task (its
// classification is relied on elsewhere, e.g. supervisor.go's login
// watchdog), so the phrases are re-checked here rather than exported from
// there. Keep in sync with screen.go:38 if that list ever changes.
var loginConclusivePhrases = []string{
	"invalid authentication credentials", "not logged in", "/login to authenticate",
}

// paneHasConclusiveLoginPhrase reports whether low (ANSI-stripped, lowercased)
// contains one of loginConclusivePhrases. Used to give the conclusive-phrase
// signal priority over the two known post-login gates — mirroring
// classifyScreen's own order (the phrase check runs before the gate check) —
// so a pane that happens to carry both is treated as a failure candidate, not
// re-answered as a gate forever.
func paneHasConclusiveLoginPhrase(low string) bool {
	for _, sig := range loginConclusivePhrases {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// paneIsActivelyWorking mirrors screen.go:106's ScreenWorking spinner cue
// (low is ANSI-stripped, lowercased). classifyScreen itself never reaches
// that check for a pane also carrying a conclusive login phrase — the phrase
// check at screen.go:38 runs first and wins unconditionally — so a sandbox
// that is genuinely still generating/running a tool WHILE its own pane
// happens to display "not logged in" (from grepping/reading screen.go,
// relogin.go, or a diff quoting them) is classified ScreenLogin, not
// ScreenWorking, by classifyScreen. This driver cannot change that priority
// (screen.go is out of scope; supervisor.go relies on its exact ordering for
// ~40 production cc- bindings), so it re-checks the same spinner signal here,
// independently, for its OWN narrower decision: "is this pane showing signs
// of active work RIGHT NOW." A pane that is actively working is never a
// genuine logout — an authentication failure does not render a spinner — so
// this is checked before loginStrikes is ever incremented, not merely
// factored into the threshold. Keep in sync with screen.go:106 if that
// detector's signal set ever changes.
func paneIsActivelyWorking(low string) bool {
	return strings.Contains(low, "esc to interrupt") || strings.Contains(low, "· ↓") ||
		strings.Contains(low, "↓ ") && strings.Contains(low, "tokens")
}

// gateHash produces a debounce key for one of the two known screen-and-answer
// gates (managed settings / login continue) — the loop equivalent of
// confirmDialog.hash() for the confirm-dialog path.
//
// Unlike an earlier version of this function, it does NOT hash any pane text
// — only kind, the gate's own stable identity. Both gates are fixed-phrase
// matches (paneAwaitingManagedSettings/paneAwaitingLoginContinue): there is no
// "different dialog with the same kind" case the way a confirm dialog can have
// a different question/options, so kind alone fully identifies "which
// occurrence this is." Hashing the whole (ANSI-stripped, lowercased) pane text
// instead — as this function used to — meant ANY incidental redraw while the
// gate was still up (the selection highlight repainting after our own "1"
// lands, a terminal resize reflowing the line wrap) changed the hash even
// though the same gate was still showing, making the loop treat it as a brand
// new occurrence and re-send [1 Enter]. A caller that has already moved past
// this gate (pane now idle, or a fresh occurrence later) is still detected
// correctly: every OTHER branch in loop() resets lastAnsweredHash to "" the
// moment classification stops matching whatever produced it, so a genuinely
// new occurrence of the same gate — after the pane visibly left this state in
// between — is answered again regardless of kind alone being unchanged.
func gateHash(kind string) string {
	h := sha1.Sum([]byte("gate\x00" + kind))
	return hex.EncodeToString(h[:8])
}

// guardedKeystrokes re-checks the aa- prefix immediately before send actually
// runs. Ensure's check (a2a_driver.go:97) decides whether to start driving a
// session at all; this is the DIFFERENT, narrower guarantee that every site
// which types into a live pane carries its own defense-in-depth — the same
// property autoAnswerSandboxConfirm already had for the confirm-dialog path.
// The error is returned, never swallowed: the caller routes it through
// reportErr so a refusal (which should be structurally unreachable in
// production, since loop only ever runs for aa- sessions) is loud, not silent.
func guardedKeystrokes(session string, send func() error) error {
	if !strings.HasPrefix(session, "aa-") {
		return fmt.Errorf("refusing to auto-answer non-aa- session %q", session)
	}
	return send()
}

// autoAnswerSandboxConfirm 依 loop 已經抓好的 pane 判斷是否停在 Claude Code
// 自己的 confirm 對話框上，是的話答 option 1（trust / proceed），這樣 loop
// 就不會被一個沒人看得到的對話框卡住。空 pane 或未被判為 ScreenConfirm 的
// pane 都視為 no-op —— 若 session 真的還沒 ready，下一次 RunWorkerOnce 仍會
// 走 errSessionBusy 照常延後。
//
// pane 由呼叫端傳入而不是自己 capture：loop 每輪只能 capture 一次
// （capture-pane 是 fork/exec，8 個沙盒 = 每秒 8 次），新增畫面分支不得讓它
// 變成兩次。
//
// ONLY aa- sandboxes are ever auto-answered — checked here too (not just in
// Ensure) as defense in depth, since this is the function that actually types
// the keystroke. A cc- binding must keep asking the human via supervisor.go's
// confirm watchdog. The check is a strict prefix match, not a substring
// match: a session name that merely contains "aa-" somewhere else in its name
// must not qualify.
//
// lastHash is the hash (confirmDialog.hash()) of the dialog this function
// last answered for this session, or "" if none. It is returned updated:
// unchanged (echoed back) when the SAME dialog is still on screen — this is
// the debounce that stops a second "1"+Enter from being sent before the pane
// has redrawn past the first one, which would otherwise land as a stray "1"
// submitted as a prompt once the dialog actually dismisses. Reset to "" the
// moment the pane stops showing a confirm dialog at all, so a genuinely new
// dialog (even one with identical text) is still answered.
//
// The returned error is the keystroke-send failure, if any — never swallowed
// internally; the caller (loop) surfaces it via reportErr. On error the
// returned hash is UNCHANGED (lastHash echoed back), matching guardedKeystrokes'
// contract for the other two gate sites: a failed send must not be recorded
// as "already answered", or the loop would never retry it.
func autoAnswerSandboxConfirm(ctx context.Context, session, pane, lastHash string) (string, error) {
	if !strings.HasPrefix(session, "aa-") {
		return "", nil
	}
	if pane == "" || classifyScreen(pane) != ScreenConfirm {
		return "", nil
	}
	dlg, ok := parseConfirmDialog(pane)
	if !ok {
		return "", nil
	}
	h := dlg.hash()
	if h == lastHash {
		return lastHash, nil // already answered this exact dialog; pane likely hasn't redrawn yet
	}
	if err := guardedKeystrokes(session, func() error { return sendConfirmChoice(ctx, session, 1) }); err != nil {
		return lastHash, err
	}
	return h, nil
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

// EnsureSandboxDrivers reconciles the running drivers with tasks.json: every
// `working` task gets a driver (Ensure is idempotent per session), and every
// driver whose task has since reached a terminal state is stopped.
//
// Stopping terminal tasks here is not cosmetic: SweepTimeouts reclaims a
// finished sandbox's worktree and root, and a driver still looping over that
// root would keep injecting into — and re-creating files under — storage that
// is being deleted. Stop blocks until the goroutine has actually exited, which
// is precisely the guarantee reclamation needs; that is also why this runs on
// the A2A cycle's own goroutine and not the cron scheduler's.
//
// The injector is deliberately AutoStart:false. The sandbox's tmux session is
// created by SandboxExecutor.Start with the right worktree and sandbox root; a
// driver that could conjure a session would paper over a genuinely missing
// sandbox with a differently-configured one.
func EnsureSandboxDrivers(ctx context.Context, root string, d *SandboxDriver) {
	tasks, err := LoadTasks(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "a2a driver: load tasks: %v\n", err)
		return
	}
	live := make(map[string]bool, len(tasks.Tasks))
	for _, t := range tasks.Tasks {
		if t.State != TaskWorking || t.Session == "" {
			continue
		}
		live[t.Session] = true
		d.Ensure(ctx, t, TmuxInjector{Session: t.Session, Root: SandboxRoot(root, t.Session)})
	}
	for _, s := range d.Running() {
		if !live[s] {
			d.Stop(s)
		}
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
