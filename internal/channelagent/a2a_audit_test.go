package channelagent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestAppendAuditTruncatesSummary(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("字", 5000)
	if err := AppendAudit(root, AuditEntry{At: "t", Summary: long, Outcome: "accepted"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAudit(root)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadAudit = %#v, %v", got, err)
	}
	if r := []rune(got[0].Summary); len(r) > maxSummaryRunes+8 {
		t.Fatalf("summary kept %d runes; the caller's raw text must be truncated", len(r))
	}
}

// 一行壞掉不得讓整份 log 讀不出來。JSON 對控制字元做 6 倍展開，約 180 KB
// 控制位元組就能造出超過舊的 1 MiB scanner 上限的行。
func TestReadAuditSkipsOverlongAndBrokenLines(t *testing.T) {
	root := t.TempDir()
	_ = AppendAudit(root, AuditEntry{At: "1", Outcome: "accepted"})
	f, err := os.OpenFile(AuditPath(root), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(strings.Repeat("x", 2<<20) + "\n")
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	_ = AppendAudit(root, AuditEntry{At: "2", Outcome: "queued"})

	got, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("one bad line must not fail the whole read: %v", err)
	}
	if len(got) != 2 || got[0].At != "1" || got[1].At != "2" {
		t.Fatalf("entries = %#v, want the two good ones", got)
	}
}

func TestAppendAuditRotatesAtCap(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// 直接造一個超過上限的檔，不用真的寫 32 MiB 的稽核條目。
	big := make([]byte, AuditMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	big[len(big)-1] = '\n'
	if err := os.WriteFile(AuditPath(root), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendAudit(root, AuditEntry{At: "after", Outcome: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AuditPath(root) + ".1"); err != nil {
		t.Fatalf("the oversized log must be rotated to .1: %v", err)
	}
	got, _ := ReadAudit(root)
	if len(got) != 1 || got[0].At != "after" {
		t.Fatalf("post-rotation log = %#v", got)
	}
}

// round 11 review, Important 2: only Summary was truncated; CallerID/Agent/
// ContextID/TaskID were written raw. auditBadRequest's contextId-format-error
// and unknown-agent branches pass the caller's own unvalidated (and
// therefore unbounded, up to the 1 MiB body cap) input into exactly these
// fields — an approved caller could destroy the entire audit history in
// well under a hundred requests. AppendAudit must now bound every one of
// them, not just Summary.
func TestAppendAuditTruncatesCallerControlledIdentityFields(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("a", 900_000)
	if err := AppendAudit(root, AuditEntry{
		At: "t", CallerID: long, Agent: long, ContextID: long, TaskID: long, Outcome: "bad_request",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAudit(root)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadAudit = %#v, %v", got, err)
	}
	e := got[0]
	for name, v := range map[string]string{
		"CallerID": e.CallerID, "Agent": e.Agent, "ContextID": e.ContextID, "TaskID": e.TaskID,
	} {
		if n := len([]rune(v)); n > maxAuditFieldRunes+8 {
			t.Fatalf("%s kept %d runes; a caller-controlled identity field reaching the log must be bounded", name, n)
		}
	}
}

// round 11 review, Important 3: the old truncateBytes called utf8.ValidString
// on the WHOLE cut prefix, so a single invalid byte ANYWHERE before the cut
// point (not just at the boundary) made it walk back one byte at a time all
// the way to empty — O(n^2) under the global tasksMu lock, and it destroyed
// the entire truncated content, not just the split rune. A leading 0xFF
// followed by 64 KiB of ordinary text must not lose that 64 KiB.
func TestTruncateBytesDoesNotWalkBackPastTheCutPoint(t *testing.T) {
	long := "\xff" + strings.Repeat("a", 64<<10)
	got := truncateBytes(long, 100)
	if len(got) < 90 {
		t.Fatalf("an invalid byte far from the cut point destroyed the truncation: kept %d bytes: %q", len(got), got)
	}
}

// truncateBytes must never produce a cut that splits a multi-byte rune,
// regardless of the limit chosen — the whole point of byte-based truncation
// over naive slicing.
func TestTruncateBytesNeverSplitsARune(t *testing.T) {
	s := strings.Repeat("字", 100) // 每個字 3 bytes
	for _, limit := range []int{1, 2, 4, 5, 10, 250, 298, 299} {
		got := truncateBytes(s, limit)
		trimmed := strings.TrimSuffix(got, "…（截斷）")
		if !utf8.ValidString(trimmed) {
			t.Fatalf("truncateBytes(_, %d) produced invalid UTF-8: %q", limit, trimmed)
		}
	}
}

// credentialFingerprint must not degrade to a bare, unkeyed hash: round 11
// review, Minor 5 — a plain SHA-256 makes the audit log a 32-bit oracle that
// confirms whether a specific guessed credential was ever presented (hash
// the guess, compare to any logged fingerprint). Two different installs
// (different on-disk keys) must derive different fingerprints for the exact
// same credential, and neither must match the bare SHA-256 of that
// credential.
func TestCredentialFingerprintIsKeyedNotBareHash(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	fpA := credentialFingerprint(rootA, "same-secret")
	fpB := credentialFingerprint(rootB, "same-secret")
	if fpA == fpB {
		t.Fatal("two installs derived the same fingerprint for the same credential — a per-install key must make cross-install correlation impossible")
	}
	sum := sha256.Sum256([]byte("same-secret"))
	bare := hex.EncodeToString(sum[:])[:8]
	if fpA == bare || fpB == bare {
		t.Fatal("fingerprint degraded to a bare, unkeyed SHA-256 — that turns the log into an oracle for confirming a guessed credential")
	}
}

func TestCredentialFingerprintIsStablePerInstall(t *testing.T) {
	root := t.TempDir()
	a := credentialFingerprint(root, "tok")
	b := credentialFingerprint(root, "tok")
	if a != b {
		t.Fatalf("fingerprint must be stable within the same install: %q vs %q", a, b)
	}
}

// The key must be persisted to disk, not regenerated every process start —
// otherwise the same credential would fingerprint differently across a
// serve restart, breaking the one thing this field exists for (correlating
// repeated attempts).
func TestCredentialFingerprintKeyPersistsAcrossProcessRestart(t *testing.T) {
	root := t.TempDir()
	a := credentialFingerprint(root, "tok")
	// 模擬行程重啟：清掉記憶體快取，強迫下一次呼叫重新從磁碟讀金鑰。
	auditKeyMu.Lock()
	delete(auditKeyCache, root)
	auditKeyMu.Unlock()
	b := credentialFingerprint(root, "tok")
	if a != b {
		t.Fatalf("fingerprint changed across a simulated restart: %q vs %q — the key must be persisted to disk", a, b)
	}
}
