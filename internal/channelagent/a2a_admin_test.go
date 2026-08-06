package channelagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newA2AAdmin(t *testing.T) (AdminHandler, string) {
	t.Helper()
	root := t.TempDir()
	if err := AtomicWriteJSON(ConfigPath(root), map[string]any{
		"admin": map[string]any{"listen": "127.0.0.1:8787"},
		"a2a":   map[string]any{"enabled": true, "listen": "127.0.0.1:8790"},
	}); err != nil {
		t.Fatal(err)
	}
	return AdminHandler{Root: root, A2ASessions: &FakeSessionManager{}}, root
}

func adminReq(t *testing.T, h AdminHandler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAdminA2AAgentCRUD(t *testing.T) {
	h, root := newA2AAdmin(t)

	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"pm","project_dir":"/p/pm","description":"pm agent","capabilities":["plan"],"enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	got, _ := LoadAgents(root)
	if len(got.Agents) != 1 || got.Agents[0].Name != "pm" {
		t.Fatalf("agents = %#v", got.Agents)
	}

	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/pm/disable", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	got, _ = LoadAgents(root)
	if got.Agents[0].Enabled {
		t.Fatal("agent still enabled after /disable")
	}

	rec = adminReq(t, h, http.MethodDelete, "/api/a2a/agents/pm", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	got, _ = LoadAgents(root)
	if len(got.Agents) != 0 {
		t.Fatalf("agents = %#v after delete", got.Agents)
	}
}

// credential 只在 POST /api/a2a/callers 的回應裡出現一次；任何 GET 都不得回傳它。
func TestAdminA2ACallerCredentialIsNeverListed(t *testing.T) {
	h, _ := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers", `{"caller_id":"peer-a"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	cred, _ := created["credential"].(string)
	if len(cred) < 20 {
		t.Fatalf("generated credential looks too short: %q", cred)
	}

	rec = adminReq(t, h, http.MethodGet, "/api/a2a/callers", "")
	body := rec.Body.String()
	if strings.Contains(body, cred) {
		t.Fatal("GET /api/a2a/callers leaked the credential")
	}
	if !strings.Contains(body, `"has_credential":true`) {
		t.Fatalf("listing must report has_credential instead: %s", body)
	}
}

// 撤銷必須對已排隊與執行中的工作生效，不只對新請求生效。
func TestAdminA2ARevokeTerminatesInFlightWork(t *testing.T) {
	h, root := newA2AAdmin(t)
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetGrantLevel("peer-a", GrantDevelop)
	_ = SaveCallers(root, callers)

	session := SessionNameFor("a", "c1")
	_ = WriteSandboxPolicy(root, SandboxPolicy{
		Session: session, ContextID: "c1", Agent: "a", CallerID: "peer-a",
		Level: GrantDevelop, Worktree: "/p/aa-a-c1", SandboxRoot: SandboxRoot(root, session),
	})
	var tasks TaskStore
	tasks.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantDevelop,
		Session: session, Worktree: "/p/aa-a-c1", State: TaskWorking,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	tasks.Upsert(A2ATask{
		ContextID: "c2", TaskID: "t2", Agent: "a", CallerID: "peer-a", Level: GrantDevelop,
		State: TaskSubmitted, StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, tasks)

	stopper := &recordingStopper{}
	h.A2AStopper = stopper
	fake := &FakeSessionManager{}
	h.A2ASessions = fake

	rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers/peer-a/revoke", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}

	got, _ := LoadTasks(root)
	for _, id := range []string{"c1", "c2"} {
		tk, _ := got.ByContext(id)
		if tk.State != TaskCanceled || !strings.Contains(tk.Detail, "revoked") {
			t.Fatalf("%s = %q / %q, want canceled", id, tk.State, tk.Detail)
		}
	}
	// 政策檔立刻變 revoked：在 session 真的死掉之前，in-flight 的工具呼叫就
	// 已經開始被 gate 拒絕。
	pol, err := LoadSandboxPolicy(root, session)
	if err != nil || pol.Level != GrantRevoked {
		t.Fatalf("policy = %#v err = %v", pol, err)
	}
	if len(stopper.stopped) != 1 || stopper.stopped[0] != session {
		t.Fatalf("driver stops = %#v", stopper.stopped)
	}
	if len(fake.Stopped) != 1 || fake.Stopped[0] != session {
		t.Fatalf("tmux stops = %#v", fake.Stopped)
	}
	entries, _ := ReadAudit(root)
	if len(entries) == 0 || entries[len(entries)-1].Outcome != "revoked" {
		t.Fatalf("audit tail = %#v", entries)
	}
}

// callback 目的地在設定當下就要驗一次。
func TestAdminA2ASetCallbackRejectsUnsafeDestination(t *testing.T) {
	h, _ := newA2AAdmin(t)
	adminReq(t, h, http.MethodPost, "/api/a2a/callers", `{"caller_id":"peer-a"}`)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers/peer-a/callback",
		`{"url":"http://10.0.0.1/hook"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an http/private destination must be refused at configuration time, got %d", rec.Code)
	}
}

// cfg.A2A.Enabled == false 時 /api/a2a/* 一律 404。
func TestAdminA2AIs404WhenDisabled(t *testing.T) {
	root := t.TempDir()
	_ = AtomicWriteJSON(ConfigPath(root), map[string]any{"a2a": map[string]any{"enabled": false}})
	h := AdminHandler{Root: root}
	for _, p := range []string{"/api/a2a/agents", "/api/a2a/callers", "/api/a2a/tasks", "/api/a2a/audit"} {
		if rec := adminReq(t, h, http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 while a2a is disabled", p, rec.Code)
		}
	}
}
