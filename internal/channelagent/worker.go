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
	if err := AtomicWriteJSON(pathIn(root, "current_job.json"), job); err != nil {
		_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
		return true, err
	}

	outputPath := pathIn(root, "outbox", "pending", job.JobID+".json")
	if err := injector.Inject(ctx, job, outputPath); err != nil {
		_ = moveFile(processingPath, pathIn(root, "inbox", "failed", name))
		return true, err
	}

	output, err := waitOutput(ctx, outputPath, timeout)
	if err != nil {
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
