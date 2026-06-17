package channelagent

import "testing"

func TestWorktreePathSibling(t *testing.T) {
	if got := WorktreePath("/home/conray/project/fatgame", "fatgame-jfg-4512"); got != "/home/conray/project/fatgame-jfg-4512" {
		t.Fatalf("got %q", got)
	}
	// trailing slash / relative cleanup
	if got := WorktreePath("/home/conray/project/fatgame/", "x"); got != "/home/conray/project/x" {
		t.Fatalf("trailing slash: got %q", got)
	}
}
