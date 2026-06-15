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

func RunServeOnce(ctx context.Context, root string, source MessageSource, injector Injector, sender Sender, timeout time.Duration) (ServeResult, error) {
	created, err := RunWatcherWithSource(ctx, root, source)
	if err != nil {
		return ServeResult{}, err
	}
	processed, err := RunWorkerOnce(ctx, root, injector, timeout)
	if err != nil {
		return ServeResult{Created: created, Processed: processed}, err
	}
	sent, err := RunSenderOnce(ctx, root, sender)
	if err != nil {
		return ServeResult{Created: created, Processed: processed}, err
	}
	return ServeResult{Created: created, Processed: processed, Sent: sent}, nil
}
