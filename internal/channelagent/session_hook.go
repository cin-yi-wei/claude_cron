package channelagent

import (
	"encoding/json"
	"io"
	"os"
)

// SessionStart hook: Claude Code runs `claude-cron session-hook` at session
// start, passing the real session id + transcript path. We record them per
// binding so activity-tailing + the stall watchdog use the EXACT transcript path
// instead of guessing it from the encoded project dir (robust against encoding
// drift / multiple transcripts).

type sessionHookInput struct {
	CWD            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

type sessionInfo struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// RecordSessionHook reads the SessionStart hook payload and stores the session
// id + transcript path in the owning binding's state/session.json.
func RecordSessionHook(registryRoot string, in io.Reader) error {
	data, err := io.ReadAll(in)
	if err != nil {
		return nil
	}
	var hi sessionHookInput
	if json.Unmarshal(data, &hi) != nil || hi.TranscriptPath == "" {
		return nil
	}
	reg, err := LoadRegistry(registryRoot)
	if err != nil {
		return nil
	}
	b, ok := bindingByWorktree(reg, hi.CWD)
	if !ok {
		return nil
	}
	return AtomicWriteJSON(pathIn(b.Root, "state", "session.json"), sessionInfo{SessionID: hi.SessionID, TranscriptPath: hi.TranscriptPath})
}

// sessionTranscriptPath returns the binding's transcript: the exact path the
// SessionStart hook recorded (if it still exists), else the latestTranscript
// guess from the worktree.
func sessionTranscriptPath(bRoot, worktree string) string {
	var si sessionInfo
	if ReadJSON(pathIn(bRoot, "state", "session.json"), &si) == nil && si.TranscriptPath != "" {
		if _, err := os.Stat(si.TranscriptPath); err == nil {
			return si.TranscriptPath
		}
	}
	return transcriptPath(worktree)
}
