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
	if id := oldestPendingPermission(root); id != "" {
		if allow, remember, ok := parseDecision(job.Source.Content); ok {
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
		// Inject failure usually means the message never landed (e.g. a session
		// still cold from --resume). Requeue for a retry rather than losing it;
		// only give up (→ failed) after a few attempts.
		requeueOrFail(root, processingPath, name, job)
		return true, err
	}

	output, err := waitOutput(ctx, outputPath, timeout)
	if err != nil {
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
	id := oldestPendingPermission(root)
	if id == "" {
		return false, nil // nothing waiting → let the normal worker handle the inbox
	}
	pendingPath, err := oldestJSON(pathIn(root, "inbox", "pending"))
	if err != nil || pendingPath == "" {
		return false, err
	}
	var job InputJob
	if err := ReadJSON(pendingPath, &job); err != nil {
		return false, nil // malformed → leave it for the worker to fail properly
	}
	allow, remember, ok := parseDecision(job.Source.Content)
	if !ok {
		return false, nil // not a y/n → leave for normal injection once the turn ends
	}
	// Write the decision first (idempotent), then clear the pending request so the
	// gate hook unblocks, then archive the reply so it isn't re-injected later.
	if err := resolvePermission(root, id, allow, remember); err != nil {
		return false, err
	}
	_ = os.Remove(pathIn(root, "permissions", "pending", id+".json"))
	name := filepath.Base(pendingPath)
	_ = moveFile(pendingPath, pathIn(root, "inbox", "done", name))
	return true, nil
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

func waitOutput(ctx context.Context, path string, timeout time.Duration) (OutputJob, error) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", nil
	}
	return filepath.Join(dir, names[0]), nil
}

func moveFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}
