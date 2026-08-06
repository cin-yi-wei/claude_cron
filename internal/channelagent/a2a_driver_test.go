package channelagent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingInjector stands in for TmuxInjector: it records what it was asked to
// deliver instead of typing into a real pane.
type recordingInjector struct {
	mu       sync.Mutex
	Injected []InputJob
}

func (r *recordingInjector) Inject(_ context.Context, job InputJob, outputPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Injected = append(r.Injected, job)
	return nil
}

func (r *recordingInjector) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Injected)
}

// The defect this plan exists to fix: a staged job must actually be delivered.
// Asserting that Inject was CALLED is not enough — that is what let the missing
// delivery slip through thirteen reviews.
func TestSandboxDriverDeliversStagedJob(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "codereview", Session: SessionNameFor("codereview", "c1"), State: TaskWorking}
	sandbox := SandboxRoot(root, task.Session)
	if err := Init(sandbox); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := IngestMessages(context.Background(), sandbox, []SourceMessage{{
		Platform: "a2a", ChannelID: "c1", MessageID: "m1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Content: "review this",
	}}); err != nil {
		t.Fatalf("stage job: %v", err)
	}

	inj := &recordingInjector{}
	d := NewSandboxDriver(root, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, inj)
	defer d.StopAll()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && inj.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if inj.count() == 0 {
		t.Fatal("driver never delivered the staged job to the injector")
	}
	if got := inj.Injected[0].Source.Content; got != "review this" {
		t.Fatalf("delivered content = %q, want %q", got, "review this")
	}
}

func TestSandboxDriverIsIdempotentPerSession(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "a", Session: SessionNameFor("a", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 5; i++ {
		d.Ensure(ctx, task, &recordingInjector{})
	}
	defer d.StopAll()

	if got := d.Running(); len(got) != 1 {
		t.Fatalf("Running() = %#v, want exactly one driver for the session", got)
	}
}

func TestSandboxDriverStopEndsTheLoop(t *testing.T) {
	root := t.TempDir()
	task := A2ATask{ContextID: "c1", Agent: "a", Session: SessionNameFor("a", "c1"), State: TaskWorking}
	_ = Init(SandboxRoot(root, task.Session))

	d := NewSandboxDriver(root, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Ensure(ctx, task, &recordingInjector{})
	d.Stop(task.Session)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(d.Running()) != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.Running(); len(got) != 0 {
		t.Fatalf("Running() = %#v after Stop, want empty", got)
	}
}
