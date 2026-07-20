package channelagent

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func setupSchedulerRoot(t *testing.T, bindingRoot string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".channel-agent")
	reg := Registry{Bindings: []Binding{{
		Name:      "worker1",
		ChannelID: "chan1",
		Platform:  PlatformDiscord,
		Root:      bindingRoot,
	}}}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
	return root
}

func TestRunSchedulerOnceFiresDueTrigger(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "binding")
	root := setupSchedulerRoot(t, bindingRoot)

	now := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	cfg := TriggerConfig{Triggers: []Trigger{{
		Name:        "morning",
		Binding:     "worker1",
		Cron:        "0 9 * * *",
		Timezone:    "UTC",
		Message:     "早安檢查",
		CatchUp:     true,
		Enabled:     true,
		LastChecked: now.Add(-2 * time.Minute).Format(time.RFC3339),
	}}}
	if err := SaveTriggers(root, cfg); err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	fired, err := RunSchedulerOnce(context.Background(), root, now)
	if err != nil {
		t.Fatalf("RunSchedulerOnce: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if got := countJSONFiles(t, filepath.Join(bindingRoot, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs in binding inbox = %d, want 1", got)
	}

	// LastChecked 應該被推進，第二次呼叫不再重複觸發。
	fired2, err := RunSchedulerOnce(context.Background(), root, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("RunSchedulerOnce (2nd): %v", err)
	}
	if fired2 != 0 {
		t.Fatalf("second call fired = %d, want 0 (should not double-fire)", fired2)
	}
}

func TestRunSchedulerOnceSkipsNotYetDue(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "binding")
	root := setupSchedulerRoot(t, bindingRoot)

	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC) // 還沒到 09:00
	cfg := TriggerConfig{Triggers: []Trigger{{
		Name:        "morning",
		Binding:     "worker1",
		Cron:        "0 9 * * *",
		Timezone:    "UTC",
		Message:     "早安檢查",
		CatchUp:     true,
		Enabled:     true,
		LastChecked: now.Add(-1 * time.Minute).Format(time.RFC3339),
	}}}
	if err := SaveTriggers(root, cfg); err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	fired, err := RunSchedulerOnce(context.Background(), root, now)
	if err != nil {
		t.Fatalf("RunSchedulerOnce: %v", err)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 (not due yet)", fired)
	}
}

func TestRunSchedulerOnceSkipsDisabled(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "binding")
	root := setupSchedulerRoot(t, bindingRoot)

	now := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	cfg := TriggerConfig{Triggers: []Trigger{{
		Name:        "morning",
		Binding:     "worker1",
		Cron:        "0 9 * * *",
		Timezone:    "UTC",
		Message:     "早安檢查",
		CatchUp:     true,
		Enabled:     false,
		LastChecked: now.Add(-2 * time.Minute).Format(time.RFC3339),
	}}}
	if err := SaveTriggers(root, cfg); err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	fired, err := RunSchedulerOnce(context.Background(), root, now)
	if err != nil {
		t.Fatalf("RunSchedulerOnce: %v", err)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 (disabled)", fired)
	}
}

func TestRunSchedulerOnceCatchesUpAfterDowntime(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "binding")
	root := setupSchedulerRoot(t, bindingRoot)

	// 排程器停機好幾天，LastChecked 停在很久以前；CatchUp=true 應該補跑一次。
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cfg := TriggerConfig{Triggers: []Trigger{{
		Name:        "daily",
		Binding:     "worker1",
		Cron:        "0 9 * * *",
		Timezone:    "UTC",
		Message:     "補跑檢查",
		CatchUp:     true,
		Enabled:     true,
		LastChecked: now.Add(-72 * time.Hour).Format(time.RFC3339),
	}}}
	if err := SaveTriggers(root, cfg); err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	fired, err := RunSchedulerOnce(context.Background(), root, now)
	if err != nil {
		t.Fatalf("RunSchedulerOnce: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (catch-up)", fired)
	}
}

func TestRunSchedulerOnceDropsStaleMissWithoutCatchUp(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "binding")
	root := setupSchedulerRoot(t, bindingRoot)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cfg := TriggerConfig{Triggers: []Trigger{{
		Name:        "daily",
		Binding:     "worker1",
		Cron:        "0 9 * * *",
		Timezone:    "UTC",
		Message:     "不補跑檢查",
		CatchUp:     false,
		Enabled:     true,
		LastChecked: now.Add(-72 * time.Hour).Format(time.RFC3339),
	}}}
	if err := SaveTriggers(root, cfg); err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	fired, err := RunSchedulerOnce(context.Background(), root, now)
	if err != nil {
		t.Fatalf("RunSchedulerOnce: %v", err)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 (stale miss dropped, CatchUp=false)", fired)
	}

	updated, err := LoadTriggers(root)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	got, ok := updated.Get("daily")
	if !ok {
		t.Fatal("trigger missing after run")
	}
	if got.LastChecked != now.UTC().Format(time.RFC3339) {
		t.Fatalf("LastChecked = %q, want advanced to now", got.LastChecked)
	}
}

func TestRunSchedulerOnceSkipsUnknownBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".channel-agent")
	if err := SaveRegistry(root, Registry{}); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
	now := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	cfg := TriggerConfig{Triggers: []Trigger{{
		Name:        "orphan",
		Binding:     "does-not-exist",
		Cron:        "0 9 * * *",
		Timezone:    "UTC",
		Message:     "x",
		CatchUp:     true,
		Enabled:     true,
		LastChecked: now.Add(-2 * time.Minute).Format(time.RFC3339),
	}}}
	if err := SaveTriggers(root, cfg); err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	fired, err := RunSchedulerOnce(context.Background(), root, now)
	if err != nil {
		t.Fatalf("RunSchedulerOnce should not error on missing binding: %v", err)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0", fired)
	}
}

func TestTriggerConfigAddRejectsDuplicateName(t *testing.T) {
	cfg := TriggerConfig{}
	if err := cfg.Add(Trigger{Name: "x"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := cfg.Add(Trigger{Name: "x"}); err == nil {
		t.Fatal("expected error adding duplicate trigger name")
	}
}

func TestTriggerConfigRemove(t *testing.T) {
	cfg := TriggerConfig{Triggers: []Trigger{{Name: "a"}, {Name: "b"}}}
	if !cfg.Remove("a") {
		t.Fatal("expected Remove to report found")
	}
	if _, ok := cfg.Get("a"); ok {
		t.Fatal("expected \"a\" gone after Remove")
	}
	if cfg.Remove("nope") {
		t.Fatal("expected Remove to report not-found for unknown name")
	}
}
