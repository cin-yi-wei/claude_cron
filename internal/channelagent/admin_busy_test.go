package channelagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminSetAndClearBusy(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bRoot := pathIn(root, "bindings", "calc")
	seedBinding(t, root, Binding{Name: "calc", ChannelID: "c1", TmuxSession: "cc-calc", Root: bRoot})
	h := AdminHandler{Root: root}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bindings/calc/busy?duration=1h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("set status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !isBusy(bRoot) {
		t.Fatal("expected busy marker to be set")
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/bindings/calc", nil))
	var st adminStatusDTO
	if err := json.Unmarshal(rec2.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.Busy {
		t.Fatal("expected status DTO to report busy=true")
	}

	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/api/bindings/calc/busy?clear=true", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body=%s", rec3.Code, rec3.Body.String())
	}
	if isBusy(bRoot) {
		t.Fatal("expected busy marker to be cleared")
	}
}

func TestAdminSetBusyRejectsBadDuration(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	seedBinding(t, root, Binding{Name: "calc", ChannelID: "c1", TmuxSession: "cc-calc", Root: pathIn(root, "bindings", "calc")})
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bindings/calc/busy?duration=notaduration", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminSetBusyNotFound(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bindings/ghost/busy?duration=1h", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
