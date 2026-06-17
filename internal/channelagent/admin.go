package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Admin backend, phase 0: a read-only HTTP API over the registry. It reuses the
// registry + queue-count helpers, adds no write paths, and is meant to bind
// loopback only (auth arrives in a later phase before any write endpoint).

type adminBindingDTO struct {
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Mode        string `json:"mode"`
	ChannelID   string `json:"channel_id"`
	Branch      string `json:"branch"`
	TmuxSession string `json:"tmux_session"`
}

type adminStatusDTO struct {
	Name         string `json:"name"`
	Platform     string `json:"platform"`
	Mode         string `json:"mode"`
	TmuxSession  string `json:"tmux_session"`
	SessionAlive bool   `json:"session_alive"`
	Pending      int    `json:"pending"`
	Processing   int    `json:"processing"`
	Failed       int    `json:"failed"`
}

// AdminHandler is the read-only API as an http.Handler (testable without a
// listener). root is the runtime root; sessionAlive reports whether a tmux
// session is up (injectable for tests).
type AdminHandler struct {
	Root         string
	SessionAlive func(session string) bool
}

func (h AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case r.URL.Path == "/api/healthz":
		writeJSONResponse(w, map[string]string{"status": "ok"})
	case r.URL.Path == "/api/bindings":
		h.listBindings(w)
	case strings.HasPrefix(r.URL.Path, "/api/bindings/"):
		name := strings.TrimPrefix(r.URL.Path, "/api/bindings/")
		h.bindingStatus(w, name)
	default:
		http.NotFound(w, r)
	}
}

func (h AdminHandler) listBindings(w http.ResponseWriter) {
	reg, err := LoadRegistry(h.Root)
	if err != nil {
		http.Error(w, "registry error", http.StatusInternalServerError)
		return
	}
	out := make([]adminBindingDTO, 0, len(reg.Bindings))
	for _, b := range reg.Bindings {
		out = append(out, adminBindingDTO{
			Name: b.Name, Platform: b.PlatformOf(), Mode: b.ModeOf(),
			ChannelID: b.ChannelID, Branch: b.Branch, TmuxSession: b.TmuxSession,
		})
	}
	writeJSONResponse(w, out)
}

func (h AdminHandler) bindingStatus(w http.ResponseWriter, name string) {
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	reg, err := LoadRegistry(h.Root)
	if err != nil {
		http.Error(w, "registry error", http.StatusInternalServerError)
		return
	}
	b, ok := reg.Get(name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	alive := false
	if h.SessionAlive != nil {
		alive = h.SessionAlive(b.TmuxSession)
	}
	writeJSONResponse(w, adminStatusDTO{
		Name: b.Name, Platform: b.PlatformOf(), Mode: b.ModeOf(),
		TmuxSession: b.TmuxSession, SessionAlive: alive,
		Pending:    countJSON(pathIn(b.Root, "inbox", "pending")),
		Processing: countJSON(pathIn(b.Root, "inbox", "processing")),
		Failed:     countJSON(pathIn(b.Root, "inbox", "failed")),
	})
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// RunAdminServer serves the read-only API on addr until ctx is cancelled.
// addr should be loopback (e.g. 127.0.0.1:8787) until auth exists.
func RunAdminServer(ctx context.Context, root, addr string) error {
	h := AdminHandler{Root: root, SessionAlive: func(session string) bool {
		return runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil
	}}
	srv := &http.Server{Addr: addr, Handler: h}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errc:
		return err
	}
}
