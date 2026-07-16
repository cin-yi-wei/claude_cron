package channelagent

import (
	"os"
	"path/filepath"
	"testing"
)

// A dead orphan pending sits ahead of the live (newest) gate. The y/n must
// resolve the NEWEST and GC the orphan — not the old race behavior.
func TestDecisionResolvesNewestAndGCsOrphans(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil { t.Fatal(err) }
	pend := permPendingDir(root)
	os.MkdirAll(pend, 0o755)
	// orphan (older id) + live (newer id) — lexical order = chronological
	orphan := "Bash-20260716T010000000"
	live := "Bash-20260716T020000000"
	for _, id := range []string{orphan, live} {
		if err := AtomicWriteJSON(filepath.Join(pend, id+".json"), map[string]string{"id": id}); err != nil { t.Fatal(err) }
	}
	if got := newestPendingPermission(root); got != live {
		t.Fatalf("newestPendingPermission = %q, want %q", got, live)
	}
	// user replies "ya" (allow+remember)
	job := InputJob{Schema: 1, JobID: "j1", Source: SourceMessage{Content: "ya"}}
	if err := AtomicWriteJSON(pathIn(root, "inbox", "pending", "j1.json"), job); err != nil { t.Fatal(err) }
	consumed, err := ResolvePendingDecisionOnce(root)
	if err != nil || !consumed { t.Fatalf("resolve consumed=%v err=%v", consumed, err) }
	// decision written for the LIVE id
	if _, err := os.Stat(filepath.Join(permDecisionDir(root), live+".json")); err != nil {
		t.Errorf("no decision for live gate %s: %v", live, err)
	}
	// orphan swept
	if _, err := os.Stat(filepath.Join(pend, orphan+".json")); !os.IsNotExist(err) {
		t.Errorf("orphan pending not GC'd")
	}
}
