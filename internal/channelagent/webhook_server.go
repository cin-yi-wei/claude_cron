package channelagent

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// webhookServer is a single HTTP server shared by all webhook-based push
// bindings on one listen address. Routes are added/removed dynamically (a
// plain http.ServeMux cannot unregister), so many Telegram bindings share one
// port, keyed by path — fixing the per-binding port-conflict.
type webhookServer struct {
	addr string

	mu      sync.Mutex
	routes  map[string]http.Handler
	srv     *http.Server
	started bool
}

func newWebhookServer(addr string) *webhookServer {
	return &webhookServer{addr: addr, routes: map[string]http.Handler{}}
}

func (s *webhookServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h, ok := s.routes[r.URL.Path]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

// register adds (or replaces) the handler at path and starts the server on
// first use, under ctx (cancelling ctx shuts it down).
func (s *webhookServer) register(ctx context.Context, path string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = h
	if s.started {
		return
	}
	s.started = true
	s.srv = &http.Server{Addr: s.addr, Handler: http.HandlerFunc(s.serveHTTP)}
	go func() { _ = s.srv.ListenAndServe() }()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
}

func (s *webhookServer) unregister(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.routes, path)
}

// stop shuts the server down if it was started.
func (s *webhookServer) stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
}

func (s *webhookServer) hasRoute(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.routes[path]
	return ok
}
