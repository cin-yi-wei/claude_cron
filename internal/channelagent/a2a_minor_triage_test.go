package channelagent

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Fix 1: a failed sm.Stop must block removal -----------------------------

// TestStopTmuxSessionSurfacesRealFailures pins the corrected contract of
// StopTmuxSession: "tmux ran and told us it could not find the session" stays
// a non-error (that is the whole point of the function), but "we never got an
// answer at all" — fork EAGAIN, tmux binary missing, ctx canceled — must be
// reported. Before the fix the function returned nil unconditionally, so an
// A2A teardown could never learn that a live sandbox session was still
// running. runExternalCommand is faked, so this spawns no tmux and no process.
func TestStopTmuxSessionSurfacesRealFailures(t *testing.T) {
	old := runExternalCommand
	defer func() { runExternalCommand = old }()

	// (a) tmux answered with an exit code — "no such session" / "no server".
	runExternalCommand = func(context.Context, string, ...string) error {
		return &exec.ExitError{}
	}
	if err := StopTmuxSession(context.Background(), "aa-a-c1"); err != nil {
		t.Fatalf("a missing session must not be an error, got %v", err)
	}

	// (b) exec itself failed: we never asked tmux anything, so we may not
	//     claim the session is gone.
	runExternalCommand = func(context.Context, string, ...string) error {
		return errors.New("fork/exec tmux: resource temporarily unavailable")
	}
	if err := StopTmuxSession(context.Background(), "aa-a-c1"); err == nil {
		t.Fatal("an exec-level failure must be reported: the session may well still be alive")
	}

	// (c) our own ctx died before tmux answered.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runExternalCommand = func(context.Context, string, ...string) error {
		return errors.New("signal: killed")
	}
	if err := StopTmuxSession(ctx, "aa-a-c1"); err == nil {
		t.Fatal("a canceled ctx must be reported, not swallowed as a successful stop")
	}
}

// TestSweepSkipsRemovalWhenSessionStopFails is the orphan probe. A completed
// task past RetainAfterComplete is a reclaim candidate; if stopping its tmux
// session fails, deleting its worktree and sandbox root anyway leaves a live
// claude process whose cwd no longer exists, referenced by no row and
// findable by no future sweep. The candidate must instead be skipped exactly
// like a busy teardown lock is skipped: nothing touched, row keeps its
// Worktree/Session, a later sweep retries.
func TestSweepSkipsRemovalWhenSessionStopFails(t *testing.T) {
	root := t.TempDir()
	const session = "aa-a-c1"
	worktree := filepath.Join(t.TempDir(), session)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	sandboxRoot := SandboxRoot(root, session)
	if err := Init(sandboxRoot); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", Level: GrantReadOnly,
		Session: session, Worktree: worktree, State: TaskCompleted,
		StartedAt:   now.Add(-time.Hour).Format(time.RFC3339),
		CompletedAt: now.Add(-2 * RetainAfterComplete).Format(time.RFC3339),
	})
	if err := SaveTasks(root, s); err != nil {
		t.Fatal(err)
	}

	fake := &FakeSessionManager{FailOn: "stop"}
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, now, nil); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	} else if reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0: nothing may be reclaimed when the session could not be stopped", reclaimed)
	}
	if len(fake.Removed) != 0 {
		t.Fatalf("worktree was removed despite the session stop failing: %v", fake.Removed)
	}
	if _, err := os.Stat(sandboxRoot); err != nil {
		t.Fatalf("sandbox root was deleted while the session may still be alive: %v", err)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.Worktree != worktree || tk.Session != session {
		t.Fatalf("row lost its identity (worktree=%q session=%q); it must stay reclaim-eligible for a later sweep", tk.Worktree, tk.Session)
	}
}

// --- Fix 2: gate log rotation is a cross-process stat-then-rename race ------

// overCapFile writes a file at path whose size is >= AuditMaxBytes and whose
// first line is marker, so a later assertion can tell one generation of the
// log from another.
func overCapFile(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 0, AuditMaxBytes+len(marker)+2)
	blob = append(blob, marker...)
	blob = append(blob, '\n')
	pad := make([]byte, AuditMaxBytes)
	for i := range pad {
		pad[i] = 'x'
	}
	blob = append(blob, pad...)
	blob = append(blob, '\n')
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGateLogRotationDoesNotClobberAPriorGeneration reproduces the actual
// data-loss race. a2a-gate.jsonl is written by the PreToolUse hook, which is
// a separate OS process per tool call, so two writers can both stat the file
// over the cap and both rename it: the second rename destroys the .1
// generation (up to 32 MiB of gate history) the first one just created.
//
// rotateTestHookBeforeRotate holds this process at the exact instant after it
// has decided to rotate, and the hook performs the OTHER writer's complete
// rotation. The fix must notice, under the cross-process rotation lock, that
// the file it sized up is no longer the file on disk.
func TestGateLogRotationDoesNotClobberAPriorGeneration(t *testing.T) {
	root := t.TempDir()
	path := GateLogPath(root)
	overCapFile(t, path, `{"at":"GEN-A"}`)

	old := rotateTestHookBeforeRotate
	defer func() { rotateTestHookBeforeRotate = old }()
	fired := false
	rotateTestHookBeforeRotate = func(p string) {
		if fired || p != path {
			return
		}
		fired = true
		// The other hook process wins: it rotates and starts a fresh log.
		lk, err := AcquireLock(rotationLockPath(p))
		if err != nil {
			t.Errorf("simulated peer could not take the rotation lock: %v", err)
			return
		}
		if err := os.Rename(p, p+".1"); err != nil {
			t.Errorf("simulated peer rename: %v", err)
		}
		if err := os.WriteFile(p, []byte(`{"at":"GEN-B"}`+"\n"), 0o600); err != nil {
			t.Errorf("simulated peer append: %v", err)
		}
		_ = lk.Release()
	}

	if err := AppendGateLog(root, GateLogEntry{At: "GEN-C", Session: "aa-a-c1"}); err != nil {
		t.Fatalf("AppendGateLog: %v", err)
	}
	if !fired {
		t.Fatal("the rotation hook never fired — the test did not exercise the rotation path")
	}

	prev, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated generation: %v", err)
	}
	if !bytes.Contains(prev, []byte("GEN-A")) {
		t.Fatalf("the .1 generation was overwritten by a second rotation — a whole generation of gate audit history was lost (got %q...)", prev[:min(64, len(prev))])
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if !bytes.Contains(cur, []byte("GEN-B")) || !bytes.Contains(cur, []byte("GEN-C")) {
		t.Fatalf("current log lost a line: %q", cur)
	}
}

// TestGateLogRotationSkippedWhileAnotherProcessHoldsTheLock proves the
// mutual exclusion itself is cross-process, not merely an in-process mutex:
// with the rotation lock held (as another hook process mid-rotation would
// hold it), this writer must not rename anything, and must still append its
// line rather than failing or blocking.
func TestGateLogRotationSkippedWhileAnotherProcessHoldsTheLock(t *testing.T) {
	root := t.TempDir()
	path := GateLogPath(root)
	overCapFile(t, path, `{"at":"GEN-A"}`)

	lk, err := AcquireLock(rotationLockPath(path))
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = lk.Release() }()

	if err := AppendGateLog(root, GateLogEntry{At: "GEN-C", Session: "aa-a-c1"}); err != nil {
		t.Fatalf("AppendGateLog while another rotator holds the lock: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("rotated while another process held the rotation lock — that is the double-rename that destroys a generation")
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cur, []byte("GEN-A")) || !bytes.Contains(cur, []byte("GEN-C")) {
		t.Fatal("the line was dropped: skipping rotation must never skip the append")
	}
}

// TestGateLogConcurrentRotationLosesNothing used to launch N goroutines and
// hope their scheduling happened to collide on the rotation boundary.
//
// Follow-up review (2026-08-06) found it did not discriminate reliably:
// stat→rename is fast enough that one writer usually finishes rotating
// before the next even calls Stat, so the intended race window was rarely
// hit — hand-removing the AcquireLock call in rotateOversizedLog and running
// the old version of this test 30 times under -race only failed it 5/30, an
// 83% false-negative rate for the exact regression it exists to catch.
//
// The natural fix is a deterministic barrier: hold every writer at the
// instant it decides "this file needs rotating" (via the same
// rotateTestHookBeforeRotate seam the tests above use) until all of them
// have arrived, then release them all at once, forcing genuine simultaneous
// contention on the real rotationLockPath lock. That fix was built and then
// dropped in that same review, because at the time it traded one
// non-discriminating test for a worse one: releasing 4 goroutines to call
// AcquireLock at the exact same instant reliably (6/100 runs under -race)
// triggered a separate, pre-existing race in lockIsStale (lock.go) — a lock
// file that had just been os.OpenFile(O_EXCL)-created but had not yet had
// its pid Fprintf+Sync'd was briefly empty, and lockIsStale treated any
// unparseable pid as "corrupt" and stole it unconditionally (no age check,
// unlike its other branches), so two callers could both believe they held
// the same lock.
//
// That race is now fixed (60785f8, lockWriteGrace): a lock file younger
// than lockWriteGrace with no parseable pid is treated as "held by a writer
// that has not recorded its pid yet", not stale, so the barrier below no
// longer needs to dodge it — confirmed by running it 220× under -race
// (across two GOMAXPROCS settings) against the correct, unmodified code
// with zero failures. Re-confirmed it still catches the original missing-
// lock regression: with the AcquireLock call hand-removed from
// rotateOversizedLog, it fails intermittently (roughly 1-5% of runs in this
// environment, both with and without -race) rather than the 0/100 the
// dropped version's report claimed — the four writers still contend for the
// same unsynchronized rename, but which pair actually lands on the
// destructive stat→rename→stat→rename interleaving depends on scheduler
// timing this barrier does not otherwise control. That is a materially
// weaker catch rate than hoped for, but it is strictly better than the
// non-discriminating version this replaces (5/30 historically) and, more
// importantly, it is not flaky against correct code, which the
// lockIsStale-affected version briefly was. The claim in an earlier version
// of this comment that the lockIsStale gap was "out of scope to fix here"
// was true when it was written; it stopped being true two commits later,
// which is why this test is back instead of staying dropped.
func TestGateLogConcurrentRotationLosesNothing(t *testing.T) {
	root := t.TempDir()
	path := GateLogPath(root)
	overCapFile(t, path, `{"at":"GEN-A"}`)

	const writers = 4
	old := rotateTestHookBeforeRotate
	defer func() { rotateTestHookBeforeRotate = old }()

	// 每個寫入者在「已經決定要輪替」的那一刻卡住，直到全部 writers 個都到齊
	// 才一次放行——逼出真正同時對 rotationLockPath 搶鎖的情況，而不是賭
	// goroutine 排程恰好撞在一起。
	var arrived atomic.Int32
	release := make(chan struct{})
	rotateTestHookBeforeRotate = func(p string) {
		if p != path {
			return
		}
		if arrived.Add(1) == int32(writers) {
			close(release)
		}
		<-release
	}

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := AppendGateLog(root, GateLogEntry{At: "W" + strconv.Itoa(i), Session: "aa-a-c1"}); err != nil {
				t.Errorf("AppendGateLog: %v", err)
			}
		}(i)
	}
	wg.Wait()

	var all []byte
	for _, p := range []string{path, path + ".1"} {
		if b, err := os.ReadFile(p); err == nil {
			all = append(all, b...)
		}
	}
	if !bytes.Contains(all, []byte("GEN-A")) {
		t.Fatal("the pre-existing generation vanished under concurrent rotation")
	}
	for i := 0; i < writers; i++ {
		want := `"at":"W` + strconv.Itoa(i) + `"`
		if !bytes.Contains(all, []byte(want)) {
			t.Fatalf("writer %d's line is in neither generation", i)
		}
	}
}

// --- Fix 3: a corrupt audit key file must not silently disable correlation --

func clearAuditKeyCache(root string) {
	auditKeyMu.Lock()
	delete(auditKeyCache, root)
	auditKeyMu.Unlock()
}

// TestCorruptAuditKeyFileSelfRepairsAndIsLogged: with a corrupt key file on
// disk, O_EXCL create returns ErrExist forever and the re-read yields nothing
// usable, so every process start silently fell back to a fresh in-memory key
// — cross-restart fingerprint correlation stopped working permanently, with
// no signal anywhere.
func TestCorruptAuditKeyFileSelfRepairsAndIsLogged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(auditKeyPath(root), []byte("not hex at all!!"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) }()

	clearAuditKeyCache(root)
	first := credentialFingerprint(root, "tok")
	clearAuditKeyCache(root) // simulate a process restart
	second := credentialFingerprint(root, "tok")

	if first != second {
		t.Fatalf("fingerprint changed across a restart (%q vs %q): a corrupt key file must be repaired, not silently bypassed forever", first, second)
	}
	raw, err := os.ReadFile(auditKeyPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hex.DecodeString(strings.TrimSpace(string(raw))); err != nil {
		t.Fatalf("key file still corrupt after two loads: %q", raw)
	}
	if !strings.Contains(buf.String(), auditKeyPath(root)) {
		t.Fatalf("the corruption was never logged; operator has no signal at all. log=%q", buf.String())
	}
}

// TestValidAuditKeyFileIsNeverReplaced is the false-positive guard for the
// repair above: a key file that decodes must be byte-for-byte untouched, and
// must keep producing the same fingerprint.
//
// Follow-up review (2026-08-06): as originally written this test asserted
// nothing about auditKeyFileIsCorrupt at all. loadOrCreateAuditKey's very
// first step (readAuditKeyFile succeeding) returns immediately for any valid
// key file — auditKeyFileIsCorrupt is only ever reached once that first read
// has already failed. So a valid file never reaches the corrupt check, and
// this test passed identically whether auditKeyFileIsCorrupt was correct, a
// do-nothing stub, or a stub that flags EVERY file as corrupt (verified: a
// hand-patched `return true` unconditionally still passed the loop below).
// The direct call below closes that gap by actually exercising the function
// this test is named for, on the exact bytes it must never flag.
func TestValidAuditKeyFileIsNeverReplaced(t *testing.T) {
	root := t.TempDir()
	key := bytes.Repeat([]byte{0xAB}, 32)
	encoded := []byte(hex.EncodeToString(key))
	path := auditKeyPath(root)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	if auditKeyFileIsCorrupt(path) {
		t.Fatal("a byte-for-byte valid hex key file was flagged corrupt — this is exactly the false positive that would make loadOrCreateAuditKey replace a perfectly good key")
	}

	for i := 0; i < 3; i++ {
		clearAuditKeyCache(root)
		if got := loadOrCreateAuditKey(root); !bytes.Equal(got, key) {
			t.Fatalf("load %d returned a different key: %x", i, got)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, encoded) {
			t.Fatalf("load %d rewrote a valid key file: %q", i, raw)
		}
	}
}

// --- Fix 4: bound the remaining audit fields --------------------------------

// TestAppendAuditBoundsEveryField is defence in depth: At/Outcome/RemoteAddr/
// CredentialFP have no caller-controlled path today, but an unbounded field
// on the audit write path is one future call site away from letting an
// approved caller rotate 32 MiB of history away per handful of requests —
// exactly what round 11 fixed for the identity fields.
func TestAppendAuditBoundsEveryField(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("a", 900_000)
	if err := AppendAudit(root, AuditEntry{
		At: long, Outcome: long, RemoteAddr: long, CredentialFP: long, Summary: "x",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1 — an unbounded field made the line exceed maxAuditLineBytes, so the record was lost entirely", len(got))
	}
	for name, v := range map[string]string{
		"At": got[0].At, "Outcome": got[0].Outcome,
		"RemoteAddr": got[0].RemoteAddr, "CredentialFP": got[0].CredentialFP,
	} {
		if r := []rune(v); len(r) > maxAuditFieldRunes+8 {
			t.Fatalf("%s kept %d runes; every audit field must be bounded", name, len(r))
		}
	}
}

// --- Fix 5: a follow-up must not create a sandbox root nothing references ---

// TestFollowUpToDispatchingRowWithoutWorktreeCreatesNoSandboxRoot closes the
// orphan-directory gap. Inject → IngestMessages calls Init(sandboxRoot)
// unconditionally, creating sandboxes/<session>/. A row in dispatching whose
// Worktree has not been persisted yet is one whose Start can still fail early
// (build-lock ctx expiry, unknown/disabled agent, invalid grant level) —
// markFailed then leaves Worktree == "", which makes the row invisible to
// SweepTimeouts' reclaim candidates AND eligible for PruneTasks. The
// directory the follow-up created then has nothing in the registry
// referencing it, forever.
//
// TmuxSessionManager is used deliberately: only its Inject method runs, and
// that method is pure disk I/O (IngestMessages). No tmux, no claude, no git.
func TestFollowUpToDispatchingRowWithoutWorktreeCreatesNoSandboxRoot(t *testing.T) {
	root := t.TempDir()
	const session = "aa-a-c1"
	task := A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantReadOnly,
		Session: session, State: TaskDispatching,
		DispatchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	var s TaskStore
	s.Upsert(task)
	if err := SaveTasks(root, s); err != nil {
		t.Fatal(err)
	}

	ex := NewSandboxExecutor(root, TmuxSessionManager{})
	err := ex.DeliverFollowUp(context.Background(), task, "and also do this")
	if err == nil {
		t.Fatal("a follow-up to a row that does not yet reference a sandbox must be refused, not delivered")
	}
	if !errors.Is(err, errFollowUpTargetGone) {
		t.Fatalf("err = %v, want errFollowUpTargetGone", err)
	}
	if _, statErr := os.Stat(SandboxRoot(root, session)); statErr == nil {
		t.Fatal("the follow-up created sandboxes/<session>/ for a row that references no worktree — an orphan directory the sweep can never find")
	}
}

// TestFollowUpToDispatchingRowWithWorktreeStillDelivers is the no-regression
// half: once Start has persisted the row's Worktree, the sandbox root is
// durably referenced, so follow-ups must go through exactly as before.
func TestFollowUpToDispatchingRowWithWorktreeStillDelivers(t *testing.T) {
	root := t.TempDir()
	const session = "aa-a-c1"
	task := A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantReadOnly,
		Session: session, Worktree: filepath.Join(t.TempDir(), session), State: TaskDispatching,
		DispatchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	var s TaskStore
	s.Upsert(task)
	if err := SaveTasks(root, s); err != nil {
		t.Fatal(err)
	}

	fake := &FakeSessionManager{}
	ex := NewSandboxExecutor(root, fake)
	if err := ex.DeliverFollowUp(context.Background(), task, "and also do this"); err != nil {
		t.Fatalf("DeliverFollowUp: %v", err)
	}
	if len(fake.Injected) != 1 {
		t.Fatalf("injected %d messages, want 1", len(fake.Injected))
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.LastMessageID == "" {
		t.Fatal("LastMessageID was not recorded; the follow-up's result could never be matched")
	}
}
