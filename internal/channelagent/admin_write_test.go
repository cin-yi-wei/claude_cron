package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeControlDeps(root string) ControlDeps {
	return ControlDeps{
		Root:           root,
		GuildID:        "g1",
		CreateChannel:  func(ctx context.Context, guildID, name string) (string, error) { return "chan-" + name, nil },
		DeleteChannel:  func(ctx context.Context, channelID string) error { return nil },
		EnsureWorktree: func(ctx context.Context, projectDir, branch, worktree string) error { return nil },
		RemoveWorktree: func(ctx context.Context, projectDir, worktree string) error { return nil },
		StartSession:   func(ctx context.Context, session, cwd string) error { return nil },
		StopSession:    func(ctx context.Context, session string) error { return nil },
		InitRoot:       func(root string) error { return Init(root) },
	}
}

func TestAdminAuthRequiresToken(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root, Token: "sekret"}

	// No token → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bindings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token = %d, want 401", rec.Code)
	}
	// Correct token → 200.
	req := httptest.NewRequest(http.MethodGet, "/api/bindings", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("with-token = %d, want 200", rec2.Code)
	}
}

func TestAdminCreateAndDeleteBinding(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	deps := fakeControlDeps(root)
	h := AdminHandler{Root: root, Deps: &deps}

	// Create.
	body := `{"name":"proj","project_dir":"` + projectDir + `","branch":"main"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bindings", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body=%s", rec.Code, rec.Body.String())
	}
	reg, _ := LoadRegistry(root)
	if _, ok := reg.Get("proj"); !ok {
		t.Fatal("binding not persisted")
	}

	// Delete.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete, "/api/bindings/proj", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec2.Code)
	}
	reg2, _ := LoadRegistry(root)
	if _, ok := reg2.Get("proj"); ok {
		t.Fatal("binding not removed")
	}
}

func TestAdminWritesDisabledWithoutDeps(t *testing.T) {
	root := t.TempDir()
	_ = Init(root)
	h := AdminHandler{Root: root} // no Deps
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bindings", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("write w/o deps = %d, want 503", rec.Code)
	}
}

func TestAdminRestart(t *testing.T) {
	root := t.TempDir()
	_ = Init(root)
	deps := fakeControlDeps(root)
	var started bool
	deps.StartSession = func(ctx context.Context, session, cwd string) error { started = true; return nil }
	seedBinding(t, root, Binding{Name: "x", ChannelID: "c", TmuxSession: "cc-x", Worktree: "/tmp/x"})
	h := AdminHandler{Root: root, Deps: &deps}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bindings/x/restart", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restart = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !started {
		t.Fatal("StartSession not called")
	}
}

func TestAdminServesUI(t *testing.T) {
	// Root now redirects to the Svelte SPA at /app/.
	h := AdminHandler{Root: t.TempDir()}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/app/" {
		t.Fatalf("root redirect: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1:8787", "localhost:1", "[::1]:9"} {
		if !isLoopbackAddr(a) {
			t.Errorf("%q should be loopback", a)
		}
	}
	if isLoopbackAddr("0.0.0.0:8787") {
		t.Error("0.0.0.0 should not be loopback")
	}
}

var _ = json.Marshal
