package channelagent

import (
	"context"
	"time"
)

type ServeResult struct {
	Created   int
	Processed bool
	Sent      int
}

func RunServeOnce(ctx context.Context, root string, ingester Ingester, injector Injector, sender Sender, timeout time.Duration) (ServeResult, error) {
	created, err := ingester.Ingest(ctx, root)
	if err != nil {
		return ServeResult{}, err
	}
	processed, werr := RunWorkerOnce(ctx, root, injector, timeout)
	// ALWAYS flush the outbox, even if the worker errored or its claude.lock was
	// held by a still-running (long) worker from a prior cycle. A session blocked
	// mid-job writes permission prompts + replies to the outbox; the sender has
	// its own sender.lock + dedup, so delivering them must not be gated behind the
	// worker — otherwise a long task starves the channel (the prompt the user must
	// answer never arrives → deadlock).
	sent, serr := RunSenderOnce(ctx, root, sender)
	if werr != nil {
		return ServeResult{Created: created, Processed: processed, Sent: sent}, werr
	}
	if serr != nil {
		return ServeResult{Created: created, Processed: processed, Sent: sent}, serr
	}
	return ServeResult{Created: created, Processed: processed, Sent: sent}, nil
}
