package channelagent

import (
	"reflect"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cmd, ok := ParseCommand("/bind proj-a /home/u/a ticket-1")
	if !ok {
		t.Fatal("expected a command")
	}
	if cmd.Name != "bind" {
		t.Fatalf("Name = %q", cmd.Name)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"proj-a", "/home/u/a", "ticket-1"}) {
		t.Fatalf("Args = %#v", cmd.Args)
	}

	cmd2, ok := ParseCommand("/unbind proj-a --delete-channel")
	if !ok || cmd2.Name != "unbind" || !cmd2.Flags["delete-channel"] {
		t.Fatalf("unbind parse wrong: %#v ok=%v", cmd2, ok)
	}
	if !reflect.DeepEqual(cmd2.Args, []string{"proj-a"}) {
		t.Fatalf("unbind Args = %#v", cmd2.Args)
	}

	if _, ok := ParseCommand("hello world"); ok {
		t.Fatal("non-slash text should not parse as command")
	}
}
