package channelagent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminSPAServes(t *testing.T) {
	h := AdminHandler{Token: "x"}
	// /app/ must serve the embedded index.html unauthenticated.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/app/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/app/ status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<div id=\"app\">") && !strings.Contains(rr.Body.String(), "<script") {
		t.Fatalf("/app/ body not the SPA index: %q", rr.Body.String())
	}
	// An asset path must resolve too.
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/app/index.html", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("/app/index.html status = %d, want 200", rr2.Code)
	}
}
