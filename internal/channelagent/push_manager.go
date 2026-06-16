package channelagent

import (
	"context"
	"sync"
)

// PushManager keeps long-running push ingesters alive across the supervisor's
// per-cycle loop. RunSupervisorOnce is stateless and called once per tick, but
// a push ingester (webhook server / gateway socket) must persist; the manager
// holds that state outside the cycle.
//
// Each ingester runs in its own goroutine under a child context. Ensure starts
// one only if not already running; Reconcile stops ingesters whose binding is
// gone; StopAll tears everything down at shutdown.
type PushManager struct {
	mu      sync.Mutex
	running map[string]*pushHandle
	parent  context.Context
}

type pushHandle struct {
	cancel context.CancelFunc
}

// NewPushManager returns a manager whose ingesters are children of parent;
// cancelling parent (or calling StopAll) stops them all.
func NewPushManager(parent context.Context) *PushManager {
	return &PushManager{running: map[string]*pushHandle{}, parent: parent}
}

// Ensure starts the ingester for name if it is not already running. The
// goroutine runs until its context is cancelled (Reconcile/StopAll) or Run
// returns. onExit, if non-nil, is called with Run's error when it stops.
func (m *PushManager) Ensure(name string, ing PushIngester, onExit func(error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.running[name]; ok {
		return
	}
	ctx, cancel := context.WithCancel(m.parent)
	h := &pushHandle{cancel: cancel}
	m.running[name] = h
	go func() {
		err := ing.Run(ctx)
		m.mu.Lock()
		// Forget this entry only if it is still the one we started (a Reconcile
		// + restart may have replaced it with a new handle under the same name).
		if cur, ok := m.running[name]; ok && cur == h {
			delete(m.running, name)
		}
		m.mu.Unlock()
		if onExit != nil {
			onExit(err)
		}
	}()
}

// Reconcile stops any running ingester whose name is not in active.
func (m *PushManager) Reconcile(active map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, h := range m.running {
		if !active[name] {
			h.cancel()
			delete(m.running, name)
		}
	}
}

// Running reports whether an ingester for name is currently tracked.
func (m *PushManager) Running(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[name]
	return ok
}

// StopAll cancels every running ingester.
func (m *PushManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, h := range m.running {
		h.cancel()
		delete(m.running, name)
	}
}
