package channelagent

import (
	"reflect"
	"sort"
	"testing"
)

func TestOrphanCCSessions(t *testing.T) {
	names := []string{"cc-control", "cc-calc", "cc-fgtest", "cron-serve", "claude_cron", "cc-old", ""}
	valid := map[string]bool{"cc-control": true, "cc-calc": true}
	got := orphanCCSessions(names, valid)
	sort.Strings(got)
	want := []string{"cc-fgtest", "cc-old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans = %v, want %v", got, want)
	}
	// non-cc sessions (cron-serve, claude_cron) must never be reaped.
	for _, o := range got {
		if o == "cron-serve" || o == "claude_cron" {
			t.Fatalf("must not reap %q", o)
		}
	}
}
