package channelagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePermissionCustomID(t *testing.T) {
	cases := []struct{ in, act, id string; ok bool }{
		{"ccperm|allow|Bash-x", "allow", "Bash-x", true},
		{"ccperm|remember|Bash-y", "remember", "Bash-y", true},
		{"ccperm|deny|Bash-z", "deny", "Bash-z", true},
		{"ccperm|bogus|Bash-x", "", "", false},
		{"ccperm|allow|", "", "", false},
		{"other|allow|x", "", "", false},
		{"nonsense", "", "", false},
	}
	for _, c := range cases {
		a, id, ok := parsePermissionCustomID(c.in)
		if a != c.act || id != c.id || ok != c.ok {
			t.Errorf("%q → (%q,%q,%v), want (%q,%q,%v)", c.in, a, id, ok, c.act, c.id, c.ok)
		}
	}
}

// A button click routes the decision to the RIGHT binding by channel id and
// writes the decision file + clears the pending marker.
func TestApplyPermissionInteractionWritesDecision(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil { t.Fatal(err) }
	pid := "Bash-20260716T120000000"
	os.MkdirAll(permPendingDir(root), 0o755)
	AtomicWriteJSON(filepath.Join(permPendingDir(root), pid+".json"), map[string]string{"id": pid})
	reg := Registry{Bindings: []Binding{{Name: "w1", Root: root, ChannelID: "chan1"}}}

	line, name, gotID, ok := applyPermissionInteraction(reg, "chan1", "ccperm|remember|"+pid)
	if !ok || name != "w1" || gotID != pid || line == "" {
		t.Fatalf("apply → ok=%v name=%q id=%q line=%q", ok, name, gotID, line)
	}
	var d struct{ Allow, Remember bool }
	if err := ReadJSON(filepath.Join(permDecisionDir(root), pid+".json"), &d); err != nil {
		t.Fatalf("no decision file: %v", err)
	}
	if !d.Allow || !d.Remember {
		t.Errorf("decision allow=%v remember=%v, want true/true", d.Allow, d.Remember)
	}
	if _, err := os.Stat(filepath.Join(permPendingDir(root), pid+".json")); !os.IsNotExist(err) {
		t.Errorf("pending marker not removed")
	}
	// unknown channel → no-op
	if _, _, _, ok := applyPermissionInteraction(reg, "nope", "ccperm|allow|"+pid); ok {
		t.Errorf("unknown channel should not resolve")
	}
}

// The edited message must keep showing what was actually requested (tool +
// detail), not just a bare result line — otherwise the user can't tell what
// they approved once the buttons are gone.
func TestApplyPermissionInteractionKeepsRequestDetail(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	pid := "Write-20260803T134400000"
	os.MkdirAll(permPendingDir(root), 0o755)
	AtomicWriteJSON(filepath.Join(permPendingDir(root), pid+".json"), map[string]string{
		"id": pid, "tool": "Write", "detail": "/home/conray/.claude/skills/gstack-review/SKILL.md",
	})
	reg := Registry{Bindings: []Binding{{Name: "w1", Root: root, ChannelID: "chan1"}}}

	line, _, _, ok := applyPermissionInteraction(reg, "chan1", "ccperm|allow|"+pid)
	if !ok {
		t.Fatal("apply failed")
	}
	if !strings.Contains(line, "Write") || !strings.Contains(line, "gstack-review/SKILL.md") || !strings.Contains(line, "已允許") {
		t.Fatalf("edited message lost the request detail: %q", line)
	}
}
