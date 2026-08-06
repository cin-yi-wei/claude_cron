package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedA2ARoot 造一個指向 fake admin server 的 root。
func seedA2ARoot(t *testing.T, adminAddr string) string {
	t.Helper()
	root := t.TempDir()
	blob, _ := json.Marshal(map[string]any{
		"admin": map[string]any{"listen": adminAddr, "token": "adm-token"},
		"a2a":   map[string]any{"enabled": true, "listen": "127.0.0.1:8790"},
	})
	if err := os.WriteFile(filepath.Join(root, "config.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// CLI 是 admin API 的薄客戶端，唯一的寫入路徑。它自己寫檔會打破「只有 serve
// 寫這些檔」這個不變量（a2a_store.go:10 的 in-process mutex 就是靠它成立
// 的）。曾經有的 --offline 直寫模式已經整段移除：review 抓到它會與 serve 的
// LoadCallers/SaveCallers 交錯而悄悄丟掉彼此的寫入（沒有跨行程鎖），且它唯一
// 的安全前提只靠一次有 TOCTOU 的 /api/healthz 探測。
func TestA2ACLIGoesThroughTheAdminAPI(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"pm"}`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "add", "pm", "--project=/p/pm", "--enabled", "--root", root}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if gotPath != "POST /api/a2a/agents" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer adm-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"name":"pm"`) || !strings.Contains(gotBody, `"project_dir":"/p/pm"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if _, err := os.Stat(filepath.Join(root, "agents.json")); err == nil {
		t.Fatal("the CLI must never write agents.json directly; it has exactly one writer path, the admin API")
	}
}

// An unknown verb must fail specifically because it's an unknown verb, not
// because the implementation refuses everything indiscriminately. Proven by
// running a known verb against the very same server/root first and requiring
// it to succeed before checking the unknown one fails.
func TestA2ACLIRejectsUnknownVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/a2a/agents" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	if code := runA2ACommand([]string{"agent", "list", "--root", root}, &out, &errOut); code != 0 {
		t.Fatalf("a known verb must succeed against the same root: exit %d: %s", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runA2ACommand([]string{"agent", "frobnicate", "x", "--root", root}, &out, &errOut); code == 0 {
		t.Fatal("an unknown verb must exit non-zero")
	}
}

// cfg.A2A.Enabled is the kill switch for the whole surface. Disabled must
// refuse before making any HTTP call and before touching any file, with a
// message that says why.
func TestA2ACLIRefusesWhenA2ADisabled(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	root := t.TempDir()
	blob, _ := json.Marshal(map[string]any{
		"admin": map[string]any{"listen": strings.TrimPrefix(srv.URL, "http://"), "token": "adm-token"},
		"a2a":   map[string]any{"enabled": false},
	})
	if err := os.WriteFile(filepath.Join(root, "config.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "list", "--root", root}, &out, &errOut)
	if code == 0 {
		t.Fatal("must refuse when a2a is disabled")
	}
	if hit {
		t.Fatal("must not call the admin API when a2a is disabled")
	}
	if !strings.Contains(errOut.String(), "disabled") {
		t.Fatalf("the refusal must say why: %s", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "agents.json")); err == nil {
		t.Fatal("must not write any file when a2a is disabled")
	}
}

// `a2a audit` has no verb — only a group. A blanket `len(pos) < 2` guard
// would reject this permanently; confirm it goes straight to
// GET /api/a2a/audit instead.
func TestA2ACLIAuditNeedsNoVerb(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"audit", "--root", root}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if gotPath != "/api/a2a/audit" {
		t.Fatalf("path = %q", gotPath)
	}
}

// --enabled is documented as a bare flag (`[--enabled]`), never
// `--enabled=value`. Omitting it must default to false, and passing it must
// set true — this is the semantics an earlier offline-mode implementation
// got backwards (it read opts["enabled"] instead of flags["enabled"], so
// omitting --enabled silently defaulted to enabled=true). Pinned here on the
// one surviving (online) path.
func TestA2ACLIAgentAddEnabledIsABareFlag(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"pm"}`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	if code := runA2ACommand([]string{"agent", "add", "pm", "--project=/p/pm", "--root", root}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(gotBody, `"enabled":false`) {
		t.Fatalf("omitting --enabled must default to false: body = %s", gotBody)
	}

	if code := runA2ACommand([]string{"agent", "add", "pm2", "--project=/p/pm2", "--enabled", "--root", root}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(gotBody, `"enabled":true`) {
		t.Fatalf("bare --enabled must set true: body = %s", gotBody)
	}
}

// agent update 只該把使用者真的帶的 --flag 放進請求 body：admin API 的
// /update 用 pointer 語意分辨「沒帶」跟「帶了空值」，CLI 這一側如果對每個
// 選項都塞一個預設值（沒帶的用 ""/nil），就會把使用者沒提到的欄位一起清空
// ——這正是 Gap 1 報告點名要避免的情況。這裡只帶 --description，body 不該
// 出現 project_dir/capabilities/channel_id 任何一個 key。
func TestA2ACLIAgentUpdateOnlySendsProvidedFields(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"updated"}`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "update", "pm", "--description=new desc", "--root", root}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if gotPath != "POST /api/a2a/agents/pm/update" {
		t.Fatalf("path = %q", gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %s", gotBody)
	}
	if body["description"] != "new desc" {
		t.Fatalf("body = %s, missing description", gotBody)
	}
	for _, key := range []string{"project_dir", "capabilities", "channel_id", "name", "enabled"} {
		if _, present := body[key]; present {
			t.Fatalf("body = %s, key %q must be absent when its flag was never passed (would wipe the field server-side)", gotBody, key)
		}
	}
}

// 帶多個 --flag 時全部要出現在 body 裡，capabilities 要被拆成陣列。
func TestA2ACLIAgentUpdateMultipleFlags(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"updated"}`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{
		"agent", "update", "pm",
		"--project=/p/pm2", "--capabilities=plan,read", "--channel=chan-2", "--root", root,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %s", gotBody)
	}
	if body["project_dir"] != "/p/pm2" || body["channel_id"] != "chan-2" {
		t.Fatalf("body = %s", gotBody)
	}
	caps, ok := body["capabilities"].([]any)
	if !ok || len(caps) != 2 || caps[0] != "plan" || caps[1] != "read" {
		t.Fatalf("capabilities = %#v", body["capabilities"])
	}
}

// Follow-up review (2026-08-06): the admin API's /update now accepts a
// rename when the CURRENT name is itself invalid (the one repair for a
// name-malformed entry — see a2a_admin.go updateA2AAgent). The CLI is the
// only supported write path (no direct agents.json edits), so without a
// --name flag that server-side repair would be reachable only via raw curl.
func TestA2ACLIAgentUpdateSendsNameWhenProvided(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"updated"}`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{
		"agent", "update", "Bad Name", "--name=badname", "--root", root,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %s", gotBody)
	}
	if body["name"] != "badname" {
		t.Fatalf("body = %s, want name=badname", gotBody)
	}
}

// --root must accept both `--root <dir>` and `--root=<dir>` — runBusyCommand
// (main.go:745) already accepts both forms; a2a silently ignoring the
// latter and falling back to ./.channel-agent would target the wrong tree.
func TestA2ACLIRootAcceptsEqualsForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "list", "--root=" + root}, &out, &errOut)
	if code != 0 {
		t.Fatalf("--root=<dir> must work: exit %d: %s", code, errOut.String())
	}
}
