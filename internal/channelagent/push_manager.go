package channelagent

import (
	"context"
	"net/http"
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
	mu       sync.Mutex
	running  map[string]*pushHandle
	parent   context.Context
	servers  map[string]*webhookServer // by listen addr, shared across bindings
	webhooks map[string]webhookRoute   // by binding name
	controlS *ControlGatewaySource     // persistent control-channel gateway source
}

// ControlSource returns the persistent gateway-backed control source, creating
// it once with the given poll backstop. Survives across supervisor cycles so
// its buffer and gateway goroutine persist.
func (m *PushManager) ControlSource(poll DiscordSource) *ControlGatewaySource {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controlS == nil {
		m.controlS = &ControlGatewaySource{Poll: poll}
	}
	return m.controlS
}

type pushHandle struct {
	cancel context.CancelFunc
}

type webhookRoute struct {
	addr string
	path string
}

// NewPushManager returns a manager whose ingesters are children of parent;
// cancelling parent (or calling StopAll) stops them all.
func NewPushManager(parent context.Context) *PushManager {
	return &PushManager{
		running:  map[string]*pushHandle{},
		parent:   parent,
		servers:  map[string]*webhookServer{},
		webhooks: map[string]webhookRoute{},
	}
}

// EnsureWebhook registers a webhook route for name on the shared server at addr
// (started lazily, once per addr), so many webhook bindings share one port. On
// first registration for name, onFirstStart runs once (e.g. setWebhook); its
// error is reported but does not unwind the route. No-op if already registered.
func (m *PushManager) EnsureWebhook(name, addr, path string, h http.Handler, onFirstStart func() error) {
	m.mu.Lock()
	if _, ok := m.webhooks[name]; ok {
		m.mu.Unlock()
		return
	}
	srv, ok := m.servers[addr]
	if !ok {
		srv = newWebhookServer(addr)
		m.servers[addr] = srv
	}
	srv.register(m.parent, path, h)
	m.webhooks[name] = webhookRoute{addr: addr, path: path}
	m.mu.Unlock()

	if onFirstStart != nil {
		_ = onFirstStart()
	}
}

// WebhookRegistered reports whether name has a webhook route.
func (m *PushManager) WebhookRegistered(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.webhooks[name]
	return ok
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

// Reconcile stops any running goroutine ingester and unregisters any webhook
// route whose name is not in active.
func (m *PushManager) Reconcile(active map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, h := range m.running {
		if !active[name] {
			h.cancel()
			delete(m.running, name)
		}
	}
	for name, route := range m.webhooks {
		if !active[name] {
			if srv, ok := m.servers[route.addr]; ok {
				srv.unregister(route.path)
			}
			delete(m.webhooks, name)
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

// StopAll cancels every running ingester and shuts down shared webhook servers.
func (m *PushManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, h := range m.running {
		h.cancel()
		delete(m.running, name)
	}
	for addr, srv := range m.servers {
		srv.stop()
		delete(m.servers, addr)
	}
	m.webhooks = map[string]webhookRoute{}
}
