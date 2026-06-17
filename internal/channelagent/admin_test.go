package channelagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seedBinding(t *testing.T, root string, b Binding) {
	t.Helper()
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if err := reg.Add(b); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
}

func TestAdminListBindings(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "calc", ChannelID: "c1", Branch: "dev", TmuxSession: "cc-calc"})
	seedBinding(t, root, Binding{Name: "tgx", ChannelID: "999", Branch: "main", TmuxSession: "cc-tgx", Platform: PlatformTelegram, Mode: ModePush})

	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bindings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []adminBindingDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("bindings = %d, want 2", len(got))
	}
	// defaults applied for the discord/poll binding
	if got[0].Name != "calc" || got[0].Platform != "discord" || got[0].Mode != "poll" {
		t.Fatalf("calc dto = %#v", got[0])
	}
	if got[1].Platform != "telegram" || got[1].Mode != "push" {
		t.Fatalf("tgx dto = %#v", got[1])
	}
}

func TestAdminBindingStatusAndNotFound(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "calc")
	seedBinding(t, root, Binding{Name: "calc", ChannelID: "c1", TmuxSession: "cc-calc", Root: bRoot})

	h := AdminHandler{Root: root, SessionAlive: func(string) bool { return true }}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bindings/calc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var st adminStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Name != "calc" || !st.SessionAlive {
		t.Fatalf("status = %#v", st)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/bindings/nope", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("missing binding status = %d, want 404", rec2.Code)
	}
}

func TestAdminRejectsUnsupportedMethod(t *testing.T) {
	h := AdminHandler{Root: t.TempDir()}
	rec := httptest.NewRecorder()
	// PUT is not supported on /api/bindings (GET list, POST create only).
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/bindings", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
