package channelagent

import "testing"

func TestInputBoxHasText(t *testing.T) {
	submitted := "❯ relay to the Discord thread\n✢ Zigzagging…\n────\n❯ \n────\n  /path [b] ctx:7%"
	if inputBoxHasText(submitted) {
		t.Error("submitted (empty bottom ❯) should be false")
	}
	stuck := "  - asked 2 blockers\n────\n❯ safe，跑 production\n────\n  /path [b] ctx:6%"
	if !inputBoxHasText(stuck) {
		t.Error("stuck (text in bottom ❯) should be true")
	}
	none := "some output\nno prompt here"
	if inputBoxHasText(none) {
		t.Error("no prompt line → false")
	}
}
