package channelagent

import (
	"testing"
	"time"
)

// A2A 監聽埠必須與 admin API 埠不同：admin 可建立具 shell 能力的 binding，
// 絕不能意外對外開放；A2A 監聽埠若不慎與其相同就是重大安全回歸。
func TestA2AListenDefaultDiffersFromAdminPort(t *testing.T) {
	var c Config
	got := c.A2AListen()
	if got == "" {
		t.Fatal("A2AListen must have a default")
	}
	if got == "127.0.0.1:8787" {
		t.Fatal("A2A listener must not share the admin API port")
	}
}

func TestA2AListenHonoursConfig(t *testing.T) {
	c := Config{A2A: A2AConfig{Listen: "127.0.0.1:9999"}}
	if got := c.A2AListen(); got != "127.0.0.1:9999" {
		t.Fatalf("A2AListen = %q", got)
	}
}

// The A2A cycle must not share the cron scheduler's goroutine: DrainQueue can
// block for minutes starting sandboxes, which would stall scheduling for every
// production binding.
func TestA2ACycleIntervalIsIndependent(t *testing.T) {
	var c Config
	if c.A2ACycleInterval() <= 0 {
		t.Fatal("A2ACycleInterval must have a positive default")
	}
}

func TestA2ACycleIntervalHonoursConfig(t *testing.T) {
	c := Config{A2A: A2AConfig{CycleSeconds: 3}}
	if got := c.A2ACycleInterval(); got != 3*time.Second {
		t.Fatalf("A2ACycleInterval = %v, want 3s", got)
	}
}
