package channelagent

import (
	"os"
	"time"
)

// Auto-sleep: a worker binding idle past the configured threshold has its tmux
// session killed to free RAM (Sleeping=true). It auto-wakes when a new message
// lands in its inbox. The conversation is preserved (transcript on disk +
// --resume on wake), so waking costs only the claude cold-start, not tokens.

// lastActivityUnixNano returns the most recent activity signal for a binding:
// the max mtime across its current transcript (updated at session start and on
// every turn) and its processed inbox/outbox files. 0 = no signal yet.
func lastActivityUnixNano(bRoot, worktree string) int64 {
	var newest int64
	bump := func(t int64) {
		if t > newest {
			newest = t
		}
	}
	if tp := transcriptPath(worktree); tp != "" {
		if fi, err := os.Stat(tp); err == nil {
			bump(fi.ModTime().UnixNano())
		}
	}
	for _, fp := range jsonFilesByMtime(pathIn(bRoot, "inbox", "done")) {
		bump(fp.mtime)
	}
	for _, fp := range jsonFilesByMtime(pathIn(bRoot, "outbox", "sent")) {
		bump(fp.mtime)
	}
	return newest
}

// shouldSleep reports whether an active binding has been idle longer than the
// timeout (with no queued input). timeout<=0 disables. A binding with no
// activity signal yet (0) is left alone.
func shouldSleep(bRoot, worktree string, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	if countJSON(pathIn(bRoot, "inbox", "pending")) > 0 {
		return false // work queued → not idle
	}
	la := lastActivityUnixNano(bRoot, worktree)
	if la == 0 {
		return false
	}
	return time.Since(time.Unix(0, la)) > timeout
}
