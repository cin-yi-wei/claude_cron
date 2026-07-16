package channelagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Injector interface {
	Inject(ctx context.Context, job InputJob, outputPath string) error
}

// glitchInspector is an optional Injector capability: report whether the session
// is sitting in a broken turn (literal tool-call markup printed as text) rather
// than actually working. Used to decide whether a no-reply timeout should retry.
type glitchInspector interface {
	LooksGlitched(ctx context.Context) bool
}

// workingInspector is an optional Injector capability: report whether the pane is
// actively occupied (generating/spinner, or showing a confirm/login prompt) — as
// opposed to a plain idle prompt. Used by the hung-turn watchdog so a long but
// working turn is never cut short, while a hung/no-output turn releases the lock
// after the stall window instead of holding it the full claude timeout.
type workingInspector interface {
	SessionWorking(ctx context.Context) bool
}

// stallWindow is how long the pane may sit not-working with no reply before the
// turn is treated as hung. Kept well above normal between-tool gaps.
const stallWindow = 90 * time.Second

func RunWorkerOnce(ctx context.Context, root string, injector Injector, timeout time.Duration) (bool, error) {
	if err := Init(root); err != nil {
		return false, err
	}
	lock, err := AcquireLock(pathIn(root, "locks", "claude.lock"))
	if err != nil {
		return false, err
	}
	defer lock.Release()

	// Recover orphaned jobs: anything left in processing/ is from a worker that
	// was killed mid-job (the worker is single-threaded under the lock, so no
	// job is legitimately in processing/ at this point). Requeue them so they
	// are retried instead of being stuck forever.
	if err := requeueProcessing(root); err != nil {
		return false, err
	}

	pendingPath, err := oldestJSON(pathIn(root, "inbox", "pending"))
	if err != nil {
		return false, err
	}
	if pendingPath == "" {
		return false, nil
	}

	name := filepath.Base(pendingPath)
	processingPath := pathIn(root, "inbox", "processing", name)
	if err := os.Rename(pendingPath, processingPath); err != nil {
		return false, err
	}

	var job InputJob
	if err := ReadJSON(processingPath, &job); err != nil {
		_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
		return true, err
	}

	// Permission-gate side-route: if a tool is waiting on the channel for
	// approval and this message is a y/n decision, resolve it instead of
	// injecting it into the session (the session is blocked in the hook).
	if id := newestPendingPermission(root); id != "" {
		if allow, remember, ok := parseDecision(job.Source.Content); ok {
			gcOrphanPermissions(root, id)
			if err := resolvePermission(root, id, allow, remember); err != nil {
				_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
				return true, err
			}
			// Also clear the pending file ourselves. Normally the blocked gate hook
			// removes it on return, but if that process died (e.g. Claude's own hook
			// timeout killed it before the user replied), the pending would linger and
			// poison every future y/n for this binding. Removing it here self-heals.
			_ = os.Remove(pathIn(root, "permissions", "pending", id+".json"))
			_ = moveFile(processingPath, pathIn(root, "inbox", "done", name))
			return true, nil
		}
	}

	if err := AtomicWriteJSON(pathIn(root, "current_job.json"), job); err != nil {
		_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
		return true, err
	}

	outputPath := pathIn(root, "outbox", "pending", job.JobID+".json")
	if err := injector.Inject(ctx, job, outputPath); err != nil {
		// Session busy (mid-turn / dialog): not a failure, just "not now". Put the
		// job back UNCHANGED — don't increment Attempt — so a long legitimate turn
		// can't exhaust the retry budget and drop the message. Retried next cycle.
		if errors.Is(err, errSessionBusy) {
			_ = moveFile(processingPath, pathIn(root, "inbox", "pending", name))
			return false, nil
		}
		// Inject failure usually means the message never landed (e.g. a session
		// still cold from --resume). Requeue for a retry rather than losing it;
		// only give up (→ failed) after a few attempts.
		requeueOrFail(root, processingPath, name, job)
		return true, err
	}

	// Hung-turn watchdog: if the injector can report working-state, let waitOutput
	// bail after stallWindow when the pane goes idle with no reply (turn hung /
	// produced nothing) — releasing claude.lock in ~90s instead of the full
	// timeout. A pending permission keeps the turn "occupied" so a confirm dialog
	// awaiting the user is never mistaken for a stall.
	var working func() bool
	if wi, ok := injector.(workingInspector); ok {
		working = func() bool {
			return wi.SessionWorking(ctx) || oldestPendingPermission(root) != ""
		}
	}
	output, err := waitOutput(ctx, outputPath, timeout, working, stallWindow)
	if err != nil {
		// A hung turn (no output + pane went idle): requeue for a clean retry and
		// release the lock now, rather than holding it the whole timeout.
		if errors.Is(err, errStalled) {
			requeueOrFail(root, processingPath, name, job)
			return true, err
		}
		// No reply within the window. If the session emitted a broken turn — e.g.
		// it printed the literal tool-call markup as text instead of executing it,
		// a known transient model glitch — re-queue for a fresh retry rather than
		// dropping the user's message. A genuinely-working long task (still showing
		// a spinner) is NOT glitched, so it falls through to failed as before (its
		// reply, if it lands later, is still delivered by the sender).
		if g, ok := injector.(glitchInspector); ok && g.LooksGlitched(ctx) {
			requeueOrFail(root, processingPath, name, job)
			return true, err
		}
		_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
		return true, err
	}
	if err := ValidateOutput(job, output); err != nil {
		_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
		return true, err
	}
	if err := moveFile(processingPath, pathIn(root, "inbox", "done", name)); err != nil {
		return true, err
	}
	return true, nil
}

// ResolvePendingDecisionOnce resolves a y/n permission reply WITHOUT taking
// claude.lock, then archives the reply message. It exists to break a deadlock:
// when a tool triggers the gate, the Claude turn blocks inside the gate hook
// waiting for the decision — but that turn is mid-Inject/waitOutput inside
// RunWorkerOnce, which holds claude.lock for its whole duration. The user's "y"
// arrives as a new inbox message, but the only code that writes the decision
// (the worker side-route) also needs claude.lock, which it can never get while
// the blocked turn holds it. The gate then times out and denies — exactly the
// "I replied y but it died" symptom. Running the resolution out-of-band, before
// the lock, lets the decision through to the waiting hook.
//
// It only acts when a permission is actually pending (which means a turn is
// blocked and the lock is held), so it never races the normal worker path:
// in that state the locked worker cannot process the inbox anyway.
// Returns true if it consumed a message as a decision.
func ResolvePendingDecisionOnce(root string) (bool, error) {
	if err := Init(root); err != nil {
		return false, err
	}
	// Resolve the NEWEST pending request — the single live gate — not the oldest.
	// Older markers are dead orphans; sending the decision there starves the live
	// gate into a timeout-deny (the y/n race). See newestPendingPermission.
	id := newestPendingPermission(root)
	if id == "" {
		return false, nil // nothing waiting → let the normal worker handle the inbox
	}
	// Scan ALL pending inbox messages for the FIRST y/n — not just the oldest. If
	// the user chatted (a non-decision message) before replying y, checking only
	// the oldest would see that chatter, bail, and never reach the y → the gate
	// starves to a timeout-deny even though the user answered. (Mirrors
	// ResolvePendingReloginOnce's all-scan for the same reason.) Non-decision
	// messages are left in place for normal injection once the turn ends.
	pendingDir := pathIn(root, "inbox", "pending")
	for _, n := range jsonNames(pendingDir) {
		p := filepath.Join(pendingDir, n)
		var job InputJob
		if err := ReadJSON(p, &job); err != nil {
			continue
		}
		allow, remember, ok := parseDecision(job.Source.Content)
		if !ok {
			continue // chatter → skip; keep scanning for a real y/n
		}
		// Write the decision (idempotent), clear the live pending request so the
		// gate hook unblocks, GC stale orphans, then archive the reply.
		if err := resolvePermission(root, id, allow, remember); err != nil {
			return false, err
		}
		_ = os.Remove(pathIn(root, "permissions", "pending", id+".json"))
		gcOrphanPermissions(root, "")
		_ = moveFile(p, pathIn(root, "inbox", "done", n))
		return true, nil
	}
	return false, nil // no y/n anywhere in the queue yet → keep waiting
}

// maxJobAttempts bounds inject retries before a job is moved to failed.
const maxJobAttempts = 3

// requeueOrFail puts a job back in pending for another attempt (incrementing
// Attempt), or moves it to failed once attempts are exhausted. Used for inject
// failures, which usually mean the message never reached the session.
func requeueOrFail(root, processingPath, name string, job InputJob) {
	job.Attempt++
	if job.Attempt < maxJobAttempts {
		if AtomicWriteJSON(pathIn(root, "inbox", "pending", name), job) == nil {
			_ = os.Remove(processingPath)
			return
		}
	}
	_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
}

func ValidateOutput(job InputJob, output OutputJob) error {
	if output.Schema != 1 {
		return fmt.Errorf("schema = %d, want 1", output.Schema)
	}
	if output.JobID != job.JobID {
		return fmt.Errorf("job_id mismatch: %s != %s", output.JobID, job.JobID)
	}
	if output.RequestID != job.RequestID {
		return fmt.Errorf("request_id mismatch: %s != %s", output.RequestID, job.RequestID)
	}
	if output.InputHash != job.InputHash {
		return fmt.Errorf("input_hash mismatch: %s != %s", output.InputHash, job.InputHash)
	}
	if output.Send && strings.TrimSpace(output.Text) == "" {
		return errors.New("send=true requires non-empty text")
	}
	return nil
}

// errStalled means the turn produced no reply AND the pane stopped making
// progress (idle, not the spinner) for longer than the stall window — i.e. a
// hung/no-output turn, not a long-but-working one. The caller requeues it and
// returns, so the binding lock is released in ~stallWindow instead of being held
// the full `timeout` (which silences the channel; see INJECT_LOCK_FIX_SPEC.md).
var errStalled = errors.New("turn stalled: no output and pane idle")

// progressFn reports whether the session is actively working (spinner). nil
// disables stall detection (full-timeout behavior preserved).
func waitOutput(ctx context.Context, path string, timeout time.Duration, working func() bool, stallWindow time.Duration) (OutputJob, error) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	// Stall check runs on a slow sub-ticker (pane capture is not free). lastProg
	// is the last time the pane was observed working; if it stays not-working for
	// stallWindow with no reply, the turn is hung → errStalled.
	lastProg := time.Now()
	var stallC <-chan time.Time
	if working != nil && stallWindow > 0 {
		st := time.NewTicker(2 * time.Second)
		defer st.Stop()
		stallC = st.C
	}

	for {
		var output OutputJob
		err := ReadJSON(path, &output)
		if err == nil {
			return output, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return output, err
		}
		select {
		case <-ctx.Done():
			return output, ctx.Err()
		case <-stallC:
			if working() {
				lastProg = time.Now()
			} else if time.Since(lastProg) > stallWindow {
				return output, errStalled
			}
		case <-ticker.C:
		}
	}
}

// requeueProcessing moves any leftover jobs from inbox/processing back to
// inbox/pending so a worker that died mid-job does not strand them.
func requeueProcessing(root string) error {
	dir := pathIn(root, "inbox", "processing")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		from := filepath.Join(dir, entry.Name())
		to := pathIn(root, "inbox", "pending", entry.Name())
		if err := moveFile(from, to); err != nil {
			return err
		}
	}
	return nil
}

func oldestJSON(dir string) (string, error) {
	all, err := sortedJSON(dir)
	if err != nil || len(all) == 0 {
		return "", err
	}
	return all[0], nil
}

// sortedJSON returns all *.json file paths in dir sorted by name (the timestamp
// prefix makes that chronological), oldest first.
func sortedJSON(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = filepath.Join(dir, n)
	}
	return out, nil
}

// jsonNames returns the .json basenames in dir, sorted ascending (ids are
// timestamp-prefixed, so lexical order == chronological).
func jsonNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func moveFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}
