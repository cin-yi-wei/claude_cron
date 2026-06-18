package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Admin backend: an HTTP API over the registry.
//   - phase 0: read-only (GET bindings/status/healthz)
//   - phase 1: bearer-token auth
//   - phase 2: write (bind/unbind/restart), registry-locked
//   - phase 3: observability (failed-job details)
//   - phase 4: a minimal web UI at /
//
// Bind loopback unless a token is set; the API can create/delete shell-capable
// sessions, so non-loopback without auth is refused at startup.

type adminBindingDTO struct {
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Mode        string `json:"mode"`
	ChannelID   string `json:"channel_id"`
	Branch      string `json:"branch"`
	TmuxSession string `json:"tmux_session"`
	Plane       string `json:"plane"`
	Paused      bool   `json:"paused"`
	// Transport is the ACTUAL ingestion path (gateway/webhook under demux, else the
	// legacy poll/push mode). Mode is kept for reference but is fallback-only.
	Transport string `json:"transport"`
}

type adminStatusDTO struct {
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	Mode         string   `json:"mode"`
	TmuxSession  string   `json:"tmux_session"`
	SessionAlive bool     `json:"session_alive"`
	Pending      int      `json:"pending"`
	Processing   int      `json:"processing"`
	Failed       int      `json:"failed"`
	FailedJobs   []string `json:"failed_jobs,omitempty"`
}

// AdminHandler is the admin API as an http.Handler (testable without a
// listener). Deps enables write endpoints; nil Deps → writes return 503.
type AdminHandler struct {
	Root         string
	Token        string
	SessionAlive func(session string) bool
	Deps         *ControlDeps
	GuildID      string
	// RestartServe, when set, is called after a config change to apply it (config
	// is read at serve startup). In production it triggers `systemctl --user
	// restart` asynchronously; nil = no auto-restart (a manual restart is needed).
	RestartServe func()
}

func (h AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// The UI page is static (no data) and must load without a bearer token —
	// a browser navigating to the URL cannot send one. It then prompts for the
	// token and sends it on the API calls, which ARE gated below.
	if path == "/" || path == "/index.html" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(adminIndexHTML))
		return
	}
	// Health check is unauthenticated (monitoring/uptime probes can't carry the
	// bearer token); it exposes no data. Both /healthz and /api/healthz work.
	if path == "/healthz" || path == "/api/healthz" {
		writeJSONResponse(w, map[string]string{"status": "ok"})
		return
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case path == "/api/config":
		switch r.Method {
		case http.MethodGet:
			h.getConfig(w)
		case http.MethodPut:
			h.putConfig(w, r)
		default:
			methodNotAllowed(w)
		}
	case path == "/api/bindings":
		switch r.Method {
		case http.MethodGet:
			h.listBindings(w)
		case http.MethodPost:
			h.createBinding(w, r)
		default:
			methodNotAllowed(w)
		}
	case strings.HasPrefix(path, "/api/bindings/"):
		rest := strings.TrimPrefix(path, "/api/bindings/")
		if name, ok := strings.CutSuffix(rest, "/restart"); ok {
			h.restartBinding(w, r, name)
			return
		}
		if name, ok := strings.CutSuffix(rest, "/pause"); ok {
			h.pauseResume(w, r, name, "pause")
			return
		}
		if name, ok := strings.CutSuffix(rest, "/resume"); ok {
			h.pauseResume(w, r, name, "resume")
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.bindingStatus(w, rest)
		case http.MethodDelete:
			h.deleteBinding(w, r, rest)
		default:
			methodNotAllowed(w)
		}
	default:
		http.NotFound(w, r)
	}
}

func (h AdminHandler) authorized(r *http.Request) bool {
	if h.Token == "" {
		return true // loopback-only dev mode (enforced at startup)
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtleEqual(got, h.Token)
}

func (h AdminHandler) listBindings(w http.ResponseWriter) {
	reg, err := LoadRegistry(h.Root)
	if err != nil {
		http.Error(w, "registry error", http.StatusInternalServerError)
		return
	}
	cfg, _ := LoadConfig(h.Root) // for the actual transport (demux-aware); zero cfg is fine
	out := make([]adminBindingDTO, 0, len(reg.Bindings))
	for _, b := range reg.Bindings {
		out = append(out, adminBindingDTO{
			Name: b.Name, Platform: b.PlatformOf(), Mode: b.ModeOf(),
			ChannelID: b.ChannelID, Branch: b.Branch, TmuxSession: b.TmuxSession,
			Plane: b.PlaneOf(), Paused: b.Paused, Transport: cfg.Transport(b),
		})
	}
	writeJSONResponse(w, out)
}

func (h AdminHandler) bindingStatus(w http.ResponseWriter, name string) {
	if !validBindingName(name) {
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
		FailedJobs: listJSONNames(pathIn(b.Root, "inbox", "failed"), 20),
	})
}

type adminBindRequest struct {
	Name       string `json:"name"`
	ProjectDir string `json:"project_dir"`
	Branch     string `json:"branch"`
	Platform   string `json:"platform"`
	Mode       string `json:"mode"`
	ChatID     string `json:"chat_id"`
}

func (h AdminHandler) createBinding(w http.ResponseWriter, r *http.Request) {
	if h.Deps == nil {
		http.Error(w, "writes disabled", http.StatusServiceUnavailable)
		return
	}
	var req adminBindRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cmd := Command{
		Name: "bind",
		Args: []string{req.Name, req.ProjectDir, req.Branch},
		Opts: map[string]string{},
	}
	if req.Platform != "" {
		cmd.Opts["platform"] = req.Platform
	}
	if req.Mode != "" {
		cmd.Opts["mode"] = req.Mode
	}
	if req.ChatID != "" {
		cmd.Opts["chat-id"] = req.ChatID
	}
	h.runWrite(w, cmd, http.StatusCreated)
}

func (h AdminHandler) deleteBinding(w http.ResponseWriter, r *http.Request, name string) {
	if h.Deps == nil {
		http.Error(w, "writes disabled", http.StatusServiceUnavailable)
		return
	}
	cmd := Command{Name: "unbind", Args: []string{name}, Flags: map[string]bool{}}
	if r.URL.Query().Get("delete_channel") == "true" {
		cmd.Flags["delete-channel"] = true
	}
	h.runWrite(w, cmd, http.StatusOK)
}

// runWrite serializes a registry-mutating command behind the registry lock so
// it cannot clobber the supervisor (which reloads the registry each cycle).
func (h AdminHandler) runWrite(w http.ResponseWriter, cmd Command, okStatus int) {
	lock, err := AcquireLock(pathIn(h.Root, "locks", "registry.lock"))
	if err != nil {
		http.Error(w, "lock error", http.StatusInternalServerError)
		return
	}
	defer lock.Release()

	reg, err := LoadRegistry(h.Root)
	if err != nil {
		http.Error(w, "registry error", http.StatusInternalServerError)
		return
	}
	// Admin is god-view: act as whichever plane owns the target binding so manage
	// ops (unbind/pause/resume) work across planes (e.g. a telegram-plane binding).
	// For a new bind, use the requested platform's plane (default discord).
	reply, changed, herr := HandleCommand(context.Background(), *h.Deps, &reg, cmd, h.planeForCommand(cmd, reg))
	if herr != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": herr.Error()})
		return
	}
	if changed {
		if serr := SaveRegistry(h.Root, reg); serr != nil {
			http.Error(w, "save error", http.StatusInternalServerError)
			return
		}
	}
	writeJSONStatus(w, okStatus, map[string]string{"result": reply})
}

// planeForCommand picks the control plane to run cmd as: an existing target
// binding's own plane (so cross-plane manage works), else the platform requested
// for a new bind (default discord).
func (h AdminHandler) planeForCommand(cmd Command, reg Registry) ControlPlane {
	if len(cmd.Args) >= 1 {
		if b, ok := reg.Get(cmd.Args[0]); ok {
			return ControlPlane{Name: b.PlaneOf(), Platform: b.PlatformOf()}
		}
	}
	name := PlatformDiscord
	if p, err := normalizePlatform(cmd.opt("platform")); err == nil {
		name = p
	}
	return ControlPlane{Name: name, Platform: name}
}

// pauseResume handles POST /api/bindings/<name>/{pause,resume} via the shared
// command path (plane-aware), so it works for any plane's binding.
func (h AdminHandler) pauseResume(w http.ResponseWriter, r *http.Request, name, action string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.Deps == nil {
		http.Error(w, "writes disabled", http.StatusServiceUnavailable)
		return
	}
	h.runWrite(w, Command{Name: action, Args: []string{name}, Flags: map[string]bool{}}, http.StatusOK)
}

func (h AdminHandler) restartBinding(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.Deps == nil {
		http.Error(w, "writes disabled", http.StatusServiceUnavailable)
		return
	}
	if !validBindingName(name) {
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
	ctx := context.Background()
	_ = h.Deps.StopSession(ctx, b.TmuxSession)
	if err := h.Deps.StartSession(ctx, b.TmuxSession, b.Worktree); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSONResponse(w, map[string]string{"result": "restarted " + name})
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func validBindingName(name string) bool {
	return name != "" && !strings.Contains(name, "/") && ValidName(name)
}

// listJSONNames returns up to max .json filenames in dir (for surfacing failed
// job ids). Missing dir → empty.
func listJSONNames(dir string, max int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, e.Name())
		if len(out) >= max {
			break
		}
	}
	return out
}

func writeJSONResponse(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// subtleEqual is a constant-time string compare for the admin token.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// RunAdminServer serves the admin API on addr until ctx is cancelled. token,
// when empty, is allowed only for loopback addresses. deps enables writes.
func RunAdminServer(ctx context.Context, root, addr, token string, deps *ControlDeps, guildID string, restartServe func()) error {
	if token == "" && !isLoopbackAddr(addr) {
		return fmt.Errorf("admin: refusing to listen on non-loopback %q without a token", addr)
	}
	h := AdminHandler{
		Root:  root,
		Token: token,
		SessionAlive: func(session string) bool {
			return runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil
		},
		Deps:         deps,
		GuildID:      guildID,
		RestartServe: restartServe,
	}
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

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
