package channelagent

import (
	"context"
	"testing"
)

func ctrlDeps(root string) ControlDeps {
	return ControlDeps{
		Root:         root,
		InitRoot:     Init,
		StopSession:  func(context.Context, string) error { return nil },
		StartSession: func(context.Context, string, string) error { return nil },
	}
}

func bindControl(t *testing.T, reg *Registry, deps ControlDeps, name, platform string) (string, bool) {
	t.Helper()
	cmd := Command{Name: "bind", Args: []string{name}, Flags: map[string]bool{"control": true}, Opts: map[string]string{}}
	if platform != "" {
		cmd.Opts["platform"] = platform
	}
	reply, changed, err := HandleCommand(context.Background(), deps, reg, cmd, ControlPlane{Name: PlatformDiscord, Platform: PlatformDiscord})
	if err != nil {
		t.Fatalf("bind control %s: %v", name, err)
	}
	return reply, changed
}

func TestBindControlFirstIsDefault(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	reg := Registry{}
	deps := ctrlDeps(root)

	_, changed := bindControl(t, &reg, deps, "webctl", "web")
	if !changed {
		t.Fatal("first control bind not changed")
	}
	b, ok := reg.Get("webctl")
	if !ok || !b.Control || !b.Default || b.PlatformOf() != PlatformWeb {
		t.Fatalf("webctl = %#v", b)
	}
	if b.ChannelID != "webctl" {
		t.Fatalf("web control channel = %q, want name", b.ChannelID)
	}

	// Second control is NOT default.
	bindControl(t, &reg, deps, "webctl2", "web")
	b2, _ := reg.Get("webctl2")
	if !b2.Control || b2.Default {
		t.Fatalf("second control should not be default: %#v", b2)
	}
}

func TestDefaultControlProtected(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	reg := Registry{}
	deps := ctrlDeps(root)
	bindControl(t, &reg, deps, "webctl", "web")
	plane := ControlPlane{Name: PlatformDiscord, Platform: PlatformDiscord}

	// Unbind the default → refused (changed=false).
	_, changed, _ := HandleCommand(context.Background(), deps, &reg, Command{Name: "unbind", Args: []string{"webctl"}, Flags: map[string]bool{}}, plane)
	if changed {
		t.Fatal("default control unbind should be refused")
	}
	if _, ok := reg.Get("webctl"); !ok {
		t.Fatal("default control was removed despite guard")
	}
	// Pause the default → refused.
	_, changed, _ = HandleCommand(context.Background(), deps, &reg, Command{Name: "pause", Args: []string{"webctl"}, Flags: map[string]bool{}}, plane)
	if changed {
		t.Fatal("default control pause should be refused")
	}
}

func TestSetDefaultTransferThenDelete(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	reg := Registry{}
	deps := ctrlDeps(root)
	plane := ControlPlane{Name: PlatformDiscord, Platform: PlatformDiscord}
	bindControl(t, &reg, deps, "c1", "web")
	bindControl(t, &reg, deps, "c2", "web")

	// Transfer default to c2.
	_, changed, _ := HandleCommand(context.Background(), deps, &reg, Command{Name: "set-default", Args: []string{"c2"}, Flags: map[string]bool{}}, plane)
	if !changed {
		t.Fatal("set-default not changed")
	}
	c1, _ := reg.Get("c1")
	c2, _ := reg.Get("c2")
	if c1.Default || !c2.Default {
		t.Fatalf("after transfer c1.default=%v c2.default=%v", c1.Default, c2.Default)
	}
	// Now c1 (no longer default) can be unbound.
	_, changed, _ = HandleCommand(context.Background(), deps, &reg, Command{Name: "unbind", Args: []string{"c1"}, Flags: map[string]bool{}}, plane)
	if !changed {
		t.Fatal("non-default control unbind should succeed")
	}
	if _, ok := reg.Get("c1"); ok {
		t.Fatal("c1 still present after unbind")
	}
}
