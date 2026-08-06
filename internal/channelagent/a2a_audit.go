package channelagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// AuditEntry is one delegation event. The log is append-only JSONL: an
// externally reachable system needs a durable record of who asked for what.
type AuditEntry struct {
	At        string `json:"at"`
	CallerID  string `json:"caller_id"`
	Agent     string `json:"agent"`
	ContextID string `json:"context_id"`
	TaskID    string `json:"task_id,omitempty"`
	Summary   string `json:"summary"`
	Outcome   string `json:"outcome"`
	Branch    string `json:"branch,omitempty"`
}

func AuditPath(root string) string { return filepath.Join(root, "a2a-audit.jsonl") }

func AppendAudit(root string, e AuditEntry) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(AuditPath(root), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	blob, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(blob, '\n'))
	return err
}

func ReadAudit(root string) ([]AuditEntry, error) {
	f, err := os.Open(AuditPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
