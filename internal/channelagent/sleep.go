package channelagent

import (
	"os"
	"path/filepath"
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
	if tp := sessionTranscriptPath(bRoot, worktree); tp != "" {
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

// busyState is a per-binding marker a worker sets before kicking off long
// autonomous/background work that produces no inbox/outbox/transcript activity
// on its own (e.g. dispatching a subagent and waiting on it), so the idle-sleep
// check doesn't kill the session out from under it. See BusyMarkerPath.
type busyState struct {
	UntilUnixNano int64 `json:"until_unix_nano"`
}

// BusyMarkerPath is the per-binding busy-marker file path, exported so the CLI
// (busy set/clear) and this package agree on the exact same location.
func BusyMarkerPath(bRoot string) string {
	return pathIn(bRoot, "state", "busy.json")
}

// isBusy reports whether bRoot has a live (non-expired) busy marker.
func isBusy(bRoot string) bool {
	var st busyState
	if err := ReadJSON(BusyMarkerPath(bRoot), &st); err != nil {
		return false
	}
	return st.UntilUnixNano > 0 && time.Now().UnixNano() < st.UntilUnixNano
}

// SetBusy marks bRoot busy for d, suppressing idle-sleep until it expires or
// ClearBusy is called. Calling it again while already busy extends/replaces
// the expiry (last call wins).
func SetBusy(bRoot string, d time.Duration) error {
	return AtomicWriteJSON(BusyMarkerPath(bRoot), busyState{UntilUnixNano: time.Now().Add(d).UnixNano()})
}

// ClearBusy removes the busy marker, restoring normal idle-sleep behavior
// immediately instead of waiting for it to expire.
func ClearBusy(bRoot string) error {
	return AtomicWriteJSON(BusyMarkerPath(bRoot), busyState{})
}

// shouldSleep reports whether an active binding has been idle longer than the
// timeout (with no queued input). timeout<=0 disables. A binding with no
// activity signal yet (0) is left alone. A live busy marker (see isBusy)
// overrides everything else — never sleep while one is set.
func shouldSleep(bRoot, worktree string, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	if isBusy(bRoot) {
		return false // worker declared itself busy — don't reap it
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

// Stall watchdog: a session that has queued work but whose transcript hasn't
// advanced for `timeout` is stuck (wedged / hung on a prompt). We kill it so the
// next cycle recreates it (--resume retries). After maxKills consecutive kills
// with no progress, the stuck job is dropped to inbox/failed (poison-job guard).

type stallState struct {
	LastMtime int64 `json:"last_mtime"`
	Kills     int   `json:"kills"`
}

func transcriptMtime(bRoot, worktree string) int64 {
	if tp := sessionTranscriptPath(bRoot, worktree); tp != "" {
		if fi, err := os.Stat(tp); err == nil {
			return fi.ModTime().UnixNano()
		}
	}
	return 0
}

// stallAction decides what to do about a possibly-stuck session, updating the
// per-binding stall state. Returns "" (ok), "kill" (restart the session), or
// "giveup" (drop the stuck job, then restart).
func stallAction(bRoot, worktree string, timeout time.Duration, maxKills int) string {
	if timeout <= 0 {
		return ""
	}
	sp := pathIn(bRoot, "state", "stall.json")
	var st stallState
	_ = ReadJSON(sp, &st)
	tm := transcriptMtime(bRoot, worktree)
	// Progress since last check → reset the kill counter.
	if tm > st.LastMtime {
		if st.Kills != 0 || st.LastMtime != tm {
			st.Kills, st.LastMtime = 0, tm
			_ = AtomicWriteJSON(sp, st)
		}
		return ""
	}
	hasWork := countJSON(pathIn(bRoot, "inbox", "pending")) > 0 || countJSON(pathIn(bRoot, "inbox", "processing")) > 0
	if !hasWork || tm == 0 {
		return ""
	}
	if time.Since(time.Unix(0, tm)) <= timeout {
		return "" // still within the silence grace period
	}
	st.Kills++
	st.LastMtime = tm
	action := "kill"
	if st.Kills >= maxKills {
		action, st.Kills = "giveup", 0
	}
	_ = AtomicWriteJSON(sp, st)
	return action
}

// failStuckJobs moves the oldest in-flight job to inbox/failed (poison-job guard
// when a session stalls repeatedly).
func failStuckJobs(bRoot string) {
	for _, sub := range []string{"processing", "pending"} {
		if p, err := oldestJSON(pathIn(bRoot, "inbox", sub)); err == nil && p != "" {
			_ = moveFile(p, pathIn(bRoot, "inbox", "failed", filepath.Base(p)))
			return
		}
	}
}
