package channelagent

import (
	"testing"
	"time"
)

func TestAppendAuditIsAppendOnly(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, e := range []AuditEntry{
		{At: now, CallerID: "peer-a", Agent: "codereview", ContextID: "c1", Summary: "review x", Outcome: "accepted"},
		{At: now, CallerID: "peer-b", Agent: "codereview", ContextID: "c2", Summary: "review y", Outcome: "forbidden"},
	} {
		if err := AppendAudit(root, e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	got, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].ContextID != "c1" || got[1].Outcome != "forbidden" {
		t.Fatalf("audit order or content wrong: %#v", got)
	}
}

func TestReadAuditOnEmptyRootIsEmptyNotError(t *testing.T) {
	got, err := ReadAudit(t.TempDir())
	if err != nil {
		t.Fatalf("ReadAudit on empty root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %d, want 0", len(got))
	}
}
