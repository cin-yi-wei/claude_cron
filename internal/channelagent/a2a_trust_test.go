package channelagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func readTrust(t *testing.T, path, project string) (bool, bool) {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	p, ok := cfg.Projects[project]
	if !ok {
		return false, false
	}
	v, _ := p["hasTrustDialogAccepted"].(bool)
	return v, true
}

func TestEnsureFolderTrustedAddsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(path, []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
		t.Fatalf("EnsureFolderTrusted: %v", err)
	}
	trusted, present := readTrust(t, path, "/p/x")
	if !present || !trusted {
		t.Fatalf("trust not recorded: present=%v trusted=%v", present, trusted)
	}
}

// The config file is shared with every running claude process and holds far
// more than trust. Seeding must preserve everything else byte-for-byte in
// meaning — clobbering it would break unrelated sessions.
func TestEnsureFolderTrustedPreservesOtherData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	original := `{"numStartups":42,"projects":{"/p/other":{"hasTrustDialogAccepted":true,"lastCost":1.5}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
		t.Fatalf("EnsureFolderTrusted: %v", err)
	}

	blob, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["numStartups"] != float64(42) {
		t.Fatalf("unrelated top-level key lost: %#v", got["numStartups"])
	}
	other := got["projects"].(map[string]any)["/p/other"].(map[string]any)
	if other["lastCost"] != 1.5 || other["hasTrustDialogAccepted"] != true {
		t.Fatalf("unrelated project data lost: %#v", other)
	}
}

func TestEnsureFolderTrustedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	_ = os.WriteFile(path, []byte(`{"projects":{}}`), 0o600)
	for i := 0; i < 3; i++ {
		if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	trusted, _ := readTrust(t, path, "/p/x")
	if !trusted {
		t.Fatal("trust lost across repeated calls")
	}
}

func TestEnsureFolderTrustedRejectsMissingConfig(t *testing.T) {
	err := EnsureFolderTrusted(filepath.Join(t.TempDir(), "absent.json"), "/p/x")
	if err == nil {
		t.Fatal("a missing config must error rather than create a fresh one")
	}
}

// The config is live-shared by every running claude process on the box; a
// crash mid-write must never leave it truncated or with altered
// permissions. This pins both the atomic-write behaviour and that the
// original file mode survives the rewrite unchanged.
func TestEnsureFolderTrustedPreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(path, []byte(`{"projects":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFolderTrusted(path, "/p/x"); err != nil {
		t.Fatalf("EnsureFolderTrusted: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("file mode changed: got %v, want 0640", info.Mode().Perm())
	}
}

// TestEnsureFolderTrustedConcurrentCallsDoNotLoseEntries pins trustMu: a
// unique-per-call temp file (fileutil.go) stops two concurrent writers from
// interleaving bytes into one corrupt file, but does NOT by itself make the
// read-decode-mutate-encode-write sequence atomic — two goroutines could
// each read the same "before" snapshot, each add their own project's trust
// entry only in memory, and whichever writes last would publish a version
// that silently lacks the other's entry (a lost update). Sandbox starts are
// concurrent by design in production (a2a_server.go's HTTP dispatch and
// a2a_lifecycle.go's DrainQueue), so this is run under -race and asserts
// every one of many concurrent callers' entries survives. This never
// touches ~/.claude — configPath is a t.TempDir() file.
func TestEnsureFolderTrustedConcurrentCallsDoNotLoseEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(path, []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = EnsureFolderTrusted(path, fmt.Sprintf("/p/sandbox-%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}

	for i := 0; i < n; i++ {
		trusted, present := readTrust(t, path, fmt.Sprintf("/p/sandbox-%d", i))
		if !present || !trusted {
			t.Fatalf("caller %d's trust entry was lost to a concurrent writer (present=%v trusted=%v)", i, present, trusted)
		}
	}
}
