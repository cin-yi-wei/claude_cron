package channelagent

import (
	"os"
	"strings"
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
