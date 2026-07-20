package channelagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seedTriggerBinding(t *testing.T, root string) (bindingRoot string) {
	t.Helper()
	bindingRoot = pathIn(root, "bindings", "worker1")
	seedBinding(t, root, Binding{Name: "worker1", ChannelID: "chan1", Root: bindingRoot})
	return bindingRoot
}

func TestAdminListTriggersEmpty(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/triggers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got []Trigger
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil {
		t.Fatal("expected [] not null for empty list")
	}
	if len(got) != 0 {
		t.Fatalf("triggers = %d, want 0", len(got))
	}
}

func TestAdminCreateTriggerThenList(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	seedTriggerBinding(t, root)
	h := AdminHandler{Root: root}

	body, _ := json.Marshal(adminTriggerRequest{
		Name: "daily", Binding: "worker1", Cron: "0 9 * * *",
		Timezone: "Asia/Taipei", Message: "早安檢查",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/triggers", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/triggers", nil))
	var got []Trigger
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "daily" || !got[0].Enabled || !got[0].CatchUp {
		t.Fatalf("triggers = %#v", got)
	}
}

func TestAdminCreateTriggerRejectsUnknownBinding(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}
	body, _ := json.Marshal(adminTriggerRequest{
		Name: "daily", Binding: "ghost", Cron: "0 9 * * *", Message: "x",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/triggers", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminCreateTriggerRejectsBadCron(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	seedTriggerBinding(t, root)
	h := AdminHandler{Root: root}
	body, _ := json.Marshal(adminTriggerRequest{
		Name: "daily", Binding: "worker1", Cron: "not a cron", Message: "x",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/triggers", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminDeleteTrigger(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := SaveTriggers(root, TriggerConfig{Triggers: []Trigger{{Name: "gone", Binding: "worker1", Cron: "* * * * *", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/triggers/gone", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := LoadTriggers(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Get("gone"); ok {
		t.Fatal("trigger still present after delete")
	}
}

func TestAdminDeleteTriggerNotFound(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/triggers/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAdminEnableDisableTrigger(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := SaveTriggers(root, TriggerConfig{Triggers: []Trigger{{Name: "t1", Binding: "worker1", Cron: "* * * * *", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/triggers/t1/disable", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	cfg, _ := LoadTriggers(root)
	got, _ := cfg.Get("t1")
	if got.Enabled {
		t.Fatal("expected disabled")
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/triggers/t1/enable", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	cfg2, _ := LoadTriggers(root)
	got2, _ := cfg2.Get("t1")
	if !got2.Enabled {
		t.Fatal("expected enabled")
	}
}

func TestAdminTestTriggerFiresIntoInbox(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	bindingRoot := seedTriggerBinding(t, root)
	if err := SaveTriggers(root, TriggerConfig{Triggers: []Trigger{{
		Name: "manual", Binding: "worker1", Cron: "0 9 * * *", Message: "手動測試", Enabled: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/triggers/manual/test", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := countJSONFiles(t, pathIn(bindingRoot, "inbox", "pending")); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
}

func TestAdminTriggersRequireAuth(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler{Root: root, Token: "secret"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/triggers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
