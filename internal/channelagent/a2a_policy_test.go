package channelagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGrantLevelOrdering(t *testing.T) {
	for _, c := range []struct {
		a, b, want GrantLevel
	}{
		{GrantReadOnly, GrantFull, GrantReadOnly},
		{GrantFull, GrantDevelop, GrantDevelop},
		{GrantDevelop, GrantDevelop, GrantDevelop},
		// revoked 不是可授予等級，任何一邊出現它就沒有有效等級可言。
		{GrantRevoked, GrantFull, ""},
		{"", GrantFull, ""},
		{GrantFull, "bogus", ""},
	} {
		if got := MinGrantLevel(c.a, c.b); got != c.want {
			t.Errorf("MinGrantLevel(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
	for _, l := range []GrantLevel{GrantReadOnly, GrantDevelop, GrantFull} {
		if !ValidGrantLevel(l) {
			t.Errorf("%q must be a grantable level", l)
		}
	}
	for _, l := range []GrantLevel{GrantRevoked, "", "root"} {
		if ValidGrantLevel(l) {
			t.Errorf("%q must NOT be grantable", l)
		}
	}
}

// 政策檔帶著呼叫方的授權等級，且與沙盒的 worktree 路徑綁定，必須是 0600：
// callers.json 世界可讀就是本輪要修的缺陷之一，政策檔不可以重蹈覆轍。
func TestWriteSandboxPolicyIsPrivateAndRoundTrips(t *testing.T) {
	root := t.TempDir()
	want := SandboxPolicy{
		Session:     "aa-pm-c1",
		ContextID:   "c1",
		Agent:       "pm",
		CallerID:    "peer-a",
		Level:       GrantDevelop,
		Worktree:    "/p/aa-pm-c1",
		SandboxRoot: SandboxRoot(root, "aa-pm-c1"),
	}
	if err := WriteSandboxPolicy(root, want); err != nil {
		t.Fatalf("WriteSandboxPolicy: %v", err)
	}
	path, err := PolicyPath(root, "aa-pm-c1")
	if err != nil {
		t.Fatalf("PolicyPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat policy: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("policy mode = %o, want 0600", got)
	}
	got, err := LoadSandboxPolicy(root, "aa-pm-c1")
	if err != nil {
		t.Fatalf("LoadSandboxPolicy: %v", err)
	}
	if got.Level != GrantDevelop || got.Worktree != want.Worktree || got.CallerID != "peer-a" {
		t.Fatalf("round trip lost fields: %#v", got)
	}
	if got.WrittenAt == "" {
		t.Fatal("WrittenAt must be stamped on write")
	}
}

// 撤銷是「覆寫成 revoked」，不是刪檔：刪掉會落到 denied_no_policy，語意上分不
// 出「還沒發」與「被撤銷」，gate log 也就查不出撤銷是否真的生效。
func TestRevokeSandboxPolicyOverwritesInPlace(t *testing.T) {
	root := t.TempDir()
	_ = WriteSandboxPolicy(root, SandboxPolicy{
		Session: "aa-pm-c1", Level: GrantFull, Worktree: "/p/x", SandboxRoot: "/s/x",
	})
	if err := RevokeSandboxPolicy(root, "aa-pm-c1"); err != nil {
		t.Fatalf("RevokeSandboxPolicy: %v", err)
	}
	got, err := LoadSandboxPolicy(root, "aa-pm-c1")
	if err != nil {
		t.Fatalf("LoadSandboxPolicy after revoke: %v", err)
	}
	if got.Level != GrantRevoked {
		t.Fatalf("level = %q after revoke, want %q", got.Level, GrantRevoked)
	}
}

// session 名會被直接拼進檔案路徑。含 '/' 或 '..' 的名字必須在這一層就被擋下，
// 不能倚賴 LoadAgents 的驗證（D10(b) 之前它根本沒有驗證）。
func TestPolicyPathRejectsTraversalSessionNames(t *testing.T) {
	for _, s := range []string{"aa-../../etc/passwd", "aa-a/b", "cc-worker", "", "aa-", "../aa-x"} {
		if _, err := PolicyPath(t.TempDir(), s); err == nil {
			t.Errorf("PolicyPath accepted unsafe session %q", s)
		}
	}
}

func TestSandboxSessionFromRegistryRoot(t *testing.T) {
	base := t.TempDir()
	sandbox := filepath.Join(base, "sandboxes", "aa-pm-c1")
	root, session, ok := SandboxSessionFromRegistryRoot(sandbox)
	if !ok || session != "aa-pm-c1" || root != base {
		t.Fatalf("= %q,%q,%v; want %q,%q,true", root, session, ok, base, "aa-pm-c1")
	}
	// 兩個條件缺一即視為非沙盒 —— cc- binding 的 registryRoot 是 <root> 本身。
	for _, bad := range []string{
		base,
		filepath.Join(base, "sandboxes", "cc-worker"),
		filepath.Join(base, "bindings", "aa-pm-c1"),
		filepath.Join(base, "sandboxes", "aa-pm-c1", "inbox"),
	} {
		if _, _, ok := SandboxSessionFromRegistryRoot(bad); ok {
			t.Errorf("%q must not be classified as a sandbox registry root", bad)
		}
	}
}
