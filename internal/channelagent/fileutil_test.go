package channelagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAtomicWriteJSONCreatesFinalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	payload := SourceMessage{Platform: "mock", ChannelID: "local", MessageID: "m1"}

	if err := AtomicWriteJSON(path, payload); err != nil {
		t.Fatalf("AtomicWriteJSON: %v", err)
	}

	var got SourceMessage
	if err := ReadJSON(path, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if got.MessageID != payload.MessageID {
		t.Fatalf("MessageID = %q, want %q", got.MessageID, payload.MessageID)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file remains after atomic write: %v", err)
	}
}

func TestAtomicWriteJSONDoesNotReplaceFinalWhenMarshalFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("seed final: %v", err)
	}

	err := AtomicWriteJSON(path, map[string]any{"bad": func() {}})
	if err == nil {
		t.Fatal("AtomicWriteJSON succeeded with unmarshalable payload")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("final file changed after failed write: %s", got)
	}
}

// TestAtomicWriteFileConcurrentWritersDoNotCorrupt pins the fix for a
// critical bug: the old implementation used a FIXED temp name (path+".tmp"),
// so two concurrent AtomicWriteFile calls to the SAME path both opened the
// same temp file with O_TRUNC and wrote from independent offsets — an
// interleave that could publish invalid content into a file every running
// claude process shares (~/.claude.json via EnsureFolderTrusted). Each call
// now gets its own os.CreateTemp file, so every rename publishes one
// writer's payload whole, never a byte-mix of two. This never touches
// ~/.claude — it runs against a t.TempDir() path with synthetic payloads.
func TestAtomicWriteFileConcurrentWritersDoNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A distinctive, sizeable payload per writer makes a byte-interleave
			// from two concurrent writes produce content that is neither valid
			// JSON nor equal to any single writer's payload — easy to catch.
			payload := fmt.Sprintf(`{"writer":%d,"padding":%q}`, i, fmt.Sprintf("%060d", i))
			errs[i] = AtomicWriteFile(path, []byte(payload), 0o644)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	var parsed struct {
		Writer int `json:"writer"`
	}
	if err := json.Unmarshal(final, &parsed); err != nil {
		t.Fatalf("final content is not one writer's whole payload (byte-interleave corruption): %v; got: %s", err, final)
	}
	if parsed.Writer < 0 || parsed.Writer >= n {
		t.Fatalf("final content has an impossible writer id %d; got: %s", parsed.Writer, final)
	}

	// No leftover temp files: every writer's own uniquely-named temp file was
	// either renamed into place or cleaned up on failure.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries after %d concurrent writers, want exactly 1 (no leftover temp files): %v", len(entries), n, entries)
	}
}
