package channelagent

import "context"

// Ingester produces any new inbound messages for one serve cycle and writes
// them into the binding's inbox, returning how many jobs it created.
//
// It is the seam between "how messages arrive" (pull/poll vs push/webhook vs
// websocket) and the rest of the pipeline (worker → injector → sender), which
// only ever reads the inbox. Adding a new arrival mode means adding a new
// Ingester; nothing downstream changes.
type Ingester interface {
	Ingest(ctx context.Context, root string) (created int, err error)
}

// PollIngester is the passive/pull Ingester: each cycle it Fetches from a
// MessageSource and writes the new messages to the inbox. It wraps the existing
// poll behavior so the current Discord/Telegram/mock sources plug in unchanged.
type PollIngester struct {
	Source MessageSource
}

func (p PollIngester) Ingest(ctx context.Context, root string) (int, error) {
	return RunWatcherWithSource(ctx, root, p.Source)
}
