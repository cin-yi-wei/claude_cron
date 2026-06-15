package channelagent

import (
	"os"
	"path/filepath"
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
