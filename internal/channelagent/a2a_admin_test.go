package channelagent

import (
	"context"
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
	return AdminHandler{Root: root}, root
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

// Gap 1（2026-08-06 follow-up）：更新只碰使用者真的送進來的欄位，沒送的欄位
// 原樣保留——比照 approve 的「送整份覆寫」不同，這裡刻意用 pointer 語意，
// 讓 CLI「只改一個欄位」時不會把其餘欄位意外清空。
func TestAdminA2AAgentUpdatePartial(t *testing.T) {
	h, root := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"pm","project_dir":"/p/pm","description":"pm agent","capabilities":["plan"],"channel_id":"chan-1","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// 只改 description，其餘欄位（project_dir/capabilities/channel_id/enabled）
	// 完全沒出現在請求 body 裡，必須維持原值。
	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/pm/update", `{"description":"pm agent v2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	got, _ := LoadAgents(root)
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %#v", got.Agents)
	}
	a := got.Agents[0]
	if a.Name != "pm" {
		t.Fatalf("Name changed to %q, want immutable pm", a.Name)
	}
	if a.Description != "pm agent v2" {
		t.Fatalf("Description = %q, want the updated value", a.Description)
	}
	if a.ProjectDir != "/p/pm" {
		t.Fatalf("ProjectDir = %q, changed even though the request never mentioned it", a.ProjectDir)
	}
	if len(a.Capabilities) != 1 || a.Capabilities[0] != "plan" {
		t.Fatalf("Capabilities = %#v, changed even though the request never mentioned them", a.Capabilities)
	}
	if a.ChannelID != "chan-1" {
		t.Fatalf("ChannelID = %q, changed even though the request never mentioned it", a.ChannelID)
	}
	if !a.Enabled {
		t.Fatal("Enabled flipped by /update; enable state must only change via /enable or /disable")
	}
}

// 多個可變欄位一次改完：project_dir、capabilities、channel_id 全部套用。
func TestAdminA2AAgentUpdateMultipleFields(t *testing.T) {
	h, root := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"pm","project_dir":"/p/pm","description":"pm agent","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/pm/update",
		`{"project_dir":"/p/pm2","capabilities":["plan","read"],"channel_id":"chan-2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	got, _ := LoadAgents(root)
	a := got.Agents[0]
	if a.ProjectDir != "/p/pm2" {
		t.Fatalf("ProjectDir = %q", a.ProjectDir)
	}
	if len(a.Capabilities) != 2 || a.Capabilities[0] != "plan" || a.Capabilities[1] != "read" {
		t.Fatalf("Capabilities = %#v", a.Capabilities)
	}
	if a.ChannelID != "chan-2" {
		t.Fatalf("ChannelID = %q", a.ChannelID)
	}
}

// name 是 agent 的身分，SessionNameFor / SandboxWorktree 都拿它派生 session 名
// 與 worktree 路徑——改名等於讓正在跑的沙盒失去自己的記錄。/update 必須拒絕
// 任何試圖改名的請求，而不是靜靜忽略掉（忽略會讓呼叫方以為改名成功了）。
func TestAdminA2AAgentUpdateRejectsRename(t *testing.T) {
	h, root := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"pm","project_dir":"/p/pm","description":"pm agent","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/pm/update", `{"name":"pm-renamed","description":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename attempt = %d %s, want 400", rec.Code, rec.Body.String())
	}
	got, _ := LoadAgents(root)
	if len(got.Agents) != 1 || got.Agents[0].Name != "pm" {
		t.Fatalf("agents = %#v, name must be unchanged after a rejected rename", got.Agents)
	}
	if got.Agents[0].Description != "pm agent" {
		t.Fatalf("Description = %q, a rejected request must not apply any of its other fields either", got.Agents[0].Description)
	}
}

func TestAdminA2AAgentUpdateUnknownAgentIs404(t *testing.T) {
	h, _ := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents/ghost/update", `{"description":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update unknown agent = %d %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestAdminA2AAgentUpdateWrongMethodIs405(t *testing.T) {
	h, _ := newA2AAdmin(t)
	_ = adminReq(t, h, http.MethodPost, "/api/a2a/agents", `{"name":"pm","project_dir":"/p/pm","enabled":true}`)
	rec := adminReq(t, h, http.MethodGet, "/api/a2a/agents/pm/update", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET .../update = %d, want 405", rec.Code)
	}
}

// Follow-up review (2026-08-06): /update let an operator point a live agent's
// channel_id at a cc- binding's channel with no collision check. LoadAgents'
// validation filter (a2a_agents.go) then silently drops that agent on every
// future load — it vanishes from dispatch AND from GET /api/a2a/agents, and
// only the CLI (editing agents.json directly) can recover it. The fix must
// reject the collision with 400 before it is ever written to disk.
func TestAdminA2AAgentUpdateRejectsChannelCollisionWithBinding(t *testing.T) {
	h, root := newA2AAdmin(t)

	reg := Registry{Bindings: []Binding{{Name: "cc-thing", ChannelID: "12345"}}}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"probe","project_dir":"/p/probe","description":"probe agent","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/probe/update", `{"channel_id":"12345"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update to a colliding channel_id = %d %s, want 400", rec.Code, rec.Body.String())
	}

	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "probe" {
		t.Fatalf("agents = %#v; the agent must still be visible to LoadAgents — a rejected update must never make it vanish", got.Agents)
	}
	if got.Agents[0].ChannelID != "" {
		t.Fatalf("ChannelID = %q, want unchanged (empty): a rejected request must not apply any of its fields", got.Agents[0].ChannelID)
	}

	rec = adminReq(t, h, http.MethodGet, "/api/a2a/agents", "")
	var listed []adminAgentDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal agent list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("GET /api/a2a/agents returned %d agents, want 1: the agent must not disappear from the admin listing", len(listed))
	}
}

// create has the identical hole — it just cannot hide an already-working
// agent, since the agent never existed to begin with. Still worth closing:
// a created-then-immediately-invisible agent is just as confusing.
func TestAdminA2AAgentCreateRejectsChannelCollisionWithBinding(t *testing.T) {
	h, root := newA2AAdmin(t)

	reg := Registry{Bindings: []Binding{{Name: "cc-thing", ChannelID: "12345"}}}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"probe","project_dir":"/p/probe","channel_id":"12345","enabled":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a colliding channel_id = %d %s, want 400", rec.Code, rec.Body.String())
	}
	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(got.Agents) != 0 {
		t.Fatalf("agents = %#v, want none created", got.Agents)
	}
}

// Follow-up review (2026-08-06): a2a_agents.go's WithAgents doc comment
// claims the raw (unfiltered) load lets an operator "fix or delete" a
// validation-failing entry through the admin API. Before this fix none of
// the three mutating routes could actually touch a name-malformed entry:
// DisableAgent, updateA2AAgent, and the /enable branch all mutate via
// Remove-then-Add, and Add() re-validates the name format on every call —
// even though none of these requests ever changes Name — so the very entry
// this comment claims is reachable was rejected by every route that could
// have reached it (probe: DELETE -> 400 invalid agent name, /update -> 500,
// /enable -> 500, where /enable used to be a clean 404 before raw loading).
func TestAdminA2AMalformedEntryCanBeFixedAndRemoved(t *testing.T) {
	h, root := newA2AAdmin(t)
	if err := AtomicWriteJSON(AgentsPath(root), map[string]any{"agents": []map[string]any{
		{"name": "Bad Name", "project_dir": "/p/bad", "enabled": false},
	}}); err != nil {
		t.Fatal(err)
	}

	if rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents/Bad%20Name/update", `{"description":"fixed"}`); rec.Code != http.StatusOK {
		t.Fatalf("update malformed entry = %d %s, want 200", rec.Code, rec.Body.String())
	}
	raw, err := LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	if len(raw.Agents) != 1 || raw.Agents[0].Description != "fixed" {
		t.Fatalf("agents = %#v, want the description applied", raw.Agents)
	}

	if rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents/Bad%20Name/enable", ""); rec.Code != http.StatusOK {
		t.Fatalf("enable malformed entry = %d %s, want 200", rec.Code, rec.Body.String())
	}
	raw, err = LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	if len(raw.Agents) != 1 || !raw.Agents[0].Enabled {
		t.Fatalf("agents = %#v, want Enabled=true", raw.Agents)
	}

	if rec := adminReq(t, h, http.MethodDelete, "/api/a2a/agents/Bad%20Name", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete malformed entry = %d %s, want 200", rec.Code, rec.Body.String())
	}
	raw, err = LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	if len(raw.Agents) != 0 {
		t.Fatalf("agents = %#v, want the malformed entry gone", raw.Agents)
	}
}

// Follow-up review (2026-08-06): an agent can disappear from the OTHER
// direction — create the agent first, then create a binding on the same
// channel. LoadAgents drops it silently on every future load: gone from
// dispatch AND from GET /api/a2a/agents, and the only recovery
// (POST .../update {"channel_id":""}) requires already knowing the name,
// which the listing no longer shows. The fix must not reject the bind
// (bindings always win) — it must make the admin listing surface the
// excluded agent, marked distinctly, with a reason naming the colliding
// binding.
func TestAdminA2AAgentListSurfacesEntryFilteredByLateBindingChannelCollision(t *testing.T) {
	h, root := newA2AAdmin(t)

	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"probe","project_dir":"/p/probe","channel_id":"999","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	reg := Registry{Bindings: []Binding{{Name: "cc-late", ChannelID: "999"}}}
	if err := SaveRegistry(root, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(got.Agents) != 0 {
		t.Fatalf("agents = %#v, want the agent gone from dispatch — the binding must still win", got.Agents)
	}

	rec = adminReq(t, h, http.MethodGet, "/api/a2a/agents", "")
	var listed []adminAgentDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal agent list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("GET /api/a2a/agents = %#v, want the excluded agent still visible to the operator", listed)
	}
	if listed[0].Name != "probe" {
		t.Fatalf("listed[0].Name = %q, want probe", listed[0].Name)
	}
	if !listed[0].Filtered {
		t.Fatal("want probe marked Filtered=true so it reads as excluded, not healthy")
	}
	if listed[0].FilterReason == "" || !strings.Contains(listed[0].FilterReason, "cc-late") {
		t.Fatalf("FilterReason = %q, want it to name the colliding binding cc-late", listed[0].FilterReason)
	}
}

// Minor follow-up (2026-08-06): a name-malformed entry could be reached by
// /update, /enable and DELETE (TestAdminA2AMalformedEntryCanBeFixedAndRemoved
// above) but never actually repaired at the one place its real defect lives
// — its own name. Renaming out of an invalid name cannot orphan a live
// sandbox: SessionNameFor/SandboxWorktree never derived anything for a name
// that never passed a2aNameRe (LoadAgents always dropped it), so no sandbox
// could ever have started under it. This is the one rename /update allows.
func TestAdminA2AAgentUpdateAllowsRenameFromInvalidName(t *testing.T) {
	h, root := newA2AAdmin(t)
	if err := AtomicWriteJSON(AgentsPath(root), map[string]any{"agents": []map[string]any{
		{"name": "Bad Name", "project_dir": "/p/bad", "description": "d", "enabled": false},
	}}); err != nil {
		t.Fatal(err)
	}

	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents/Bad%20Name/update", `{"name":"badname"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename out of an invalid name = %d %s, want 200", rec.Code, rec.Body.String())
	}

	raw, err := LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	if len(raw.Agents) != 1 || raw.Agents[0].Name != "badname" || raw.Agents[0].ProjectDir != "/p/bad" {
		t.Fatalf("agents = %#v, want renamed to badname with its other fields preserved", raw.Agents)
	}

	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "badname" {
		t.Fatalf("agents = %#v, want the repaired agent now visible to LoadAgents (and thus to dispatch)", got.Agents)
	}
}

// A rename escape hatch that only checks "is the target new" would let an
// operator swap one broken name for another, or collide with an existing
// agent. Both must still 400.
func TestAdminA2AAgentUpdateRenameFromInvalidNameRejectsInvalidTarget(t *testing.T) {
	h, root := newA2AAdmin(t)
	if err := AtomicWriteJSON(AgentsPath(root), map[string]any{"agents": []map[string]any{
		{"name": "Bad Name", "project_dir": "/p/bad", "enabled": false},
	}}); err != nil {
		t.Fatal(err)
	}
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents/Bad%20Name/update", `{"name":"Still Bad"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename to another invalid name = %d %s, want 400", rec.Code, rec.Body.String())
	}
	raw, err := LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	if len(raw.Agents) != 1 || raw.Agents[0].Name != "Bad Name" {
		t.Fatalf("agents = %#v, name must be unchanged after a rejected rename", raw.Agents)
	}
}

func TestAdminA2AAgentUpdateRenameFromInvalidNameRejectsCollision(t *testing.T) {
	h, root := newA2AAdmin(t)
	if err := AtomicWriteJSON(AgentsPath(root), map[string]any{"agents": []map[string]any{
		{"name": "Bad Name", "project_dir": "/p/bad", "enabled": false},
		{"name": "taken", "project_dir": "/p/taken", "enabled": true},
	}}); err != nil {
		t.Fatal(err)
	}
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents/Bad%20Name/update", `{"name":"taken"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename onto an existing name = %d %s, want 400", rec.Code, rec.Body.String())
	}
	raw, err := LoadAgentsRaw(root)
	if err != nil {
		t.Fatalf("LoadAgentsRaw: %v", err)
	}
	names := map[string]bool{}
	for _, a := range raw.Agents {
		names[a.Name] = true
	}
	if !names["Bad Name"] || !names["taken"] || len(raw.Agents) != 2 {
		t.Fatalf("agents = %#v, a rejected rename must not touch either entry", raw.Agents)
	}
}

// TestAdminA2AAgentUpdateRejectsRename (above) already pins that renaming a
// VALID name stays rejected; this pins that the escape hatch cannot be used
// to rename a valid name away via the same code path that opens for invalid
// ones — the guard must key off the CURRENT name's validity, not merely
// "target differs from current".
func TestAdminA2AAgentUpdateStillRejectsRenameOfAValidName(t *testing.T) {
	h, root := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"pm","project_dir":"/p/pm","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/pm/update", `{"name":"pm2"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename of a valid name = %d %s, want 400", rec.Code, rec.Body.String())
	}
	got, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "pm" {
		t.Fatalf("agents = %#v, name must be unchanged", got.Agents)
	}
}

// Gap 1 的核心動機：一個宣告零 capabilities 的 agent 在 a2a_server.go 會被
// 永久 fail-closed（TestZeroCapabilityAgentDeniedByDefault），過去唯一的救法
// 是刪掉重建——這會丟掉任何跟它綁在一起的東西。/update 讓它可以直接被修好。
func TestAdminA2AAgentUpdateFixesZeroCapabilities(t *testing.T) {
	h, root := newA2AAdmin(t)
	rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents",
		`{"name":"pm","project_dir":"/p/pm","description":"pm agent","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	var callers CallerStore
	_ = callers.Register("peer-a", "secret-1")
	callers.Approve("peer-a", []string{"read"})
	if err := SaveCallers(root, callers); err != nil {
		t.Fatalf("SaveCallers: %v", err)
	}
	s := &A2AServer{Root: root, BaseURL: "https://example.test/a2a", Executor: &StubExecutor{}}

	rec2 := postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"pm","contextId":"c1","text":"hi"}}`)
	var respBefore RPCResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &respBefore)
	if respBefore.Error == nil || respBefore.Error.Code != RPCForbidden {
		t.Fatalf("before fix: want forbidden for a zero-capability agent, got %#v", respBefore.Error)
	}

	rec = adminReq(t, h, http.MethodPost, "/api/a2a/agents/pm/update", `{"capabilities":["read"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}

	rec2 = postRPC(t, s.Handler(), "secret-1", `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"agent":"pm","contextId":"c2","text":"hi"}}`)
	// 用全新的 struct 接第二個回應——RPCResponse.Error 帶 omitempty，若跟
	// 上面共用同一個變數，成功回應（JSON 裡沒有 "error" 這個 key）不會清掉
	// 上一次失敗留下的舊 pointer，會讓這個斷言看到假的失敗。
	var respAfter RPCResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &respAfter)
	if respAfter.Error != nil {
		t.Fatalf("after fix: want success, got %#v", respAfter.Error)
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

	// stopper/fake 不再經 AdminHandler 接線（那條線已經拆掉，見
	// terminateTasks 的說明）：revoke 本身不停任何東西，這兩個只在下面直接
	// 呼叫 SweepTimeouts 模擬「下一輪 sweep」時才用到。
	stopper := &recordingStopper{}
	fake := &FakeSessionManager{}

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
	// 撤銷本身不停 session：破壞性動作只能走 sweep 那條唯一有 TryLock + 身分
	// 重新確認的路徑（round-13-review Critical）。這裡只該留下「還沒停」的意
	// 圖，而且 HTTP 請求因此有界——SandboxDriver.Stop 會等當前那一輪
	// RunWorkerOnce 跑完，卡住的 turn 可以是二十分鐘。
	if len(stopper.stopped) != 0 || len(fake.Stopped) != 0 {
		t.Fatalf("revoke stopped things itself: driver=%#v tmux=%#v", stopper.stopped, fake.Stopped)
	}
	for _, id := range []string{"c1", "c2"} {
		tk, _ := got.ByContext(id)
		if tk.Session != "" && !tk.SessionStopPending {
			t.Fatalf("%s: SessionStopPending = false, want true so a later sweep stops it", id)
		}
	}
	// 下一輪 sweep 才真的停，而且是走那條有守衛的路徑。
	if _, _, err := SweepTimeouts(context.Background(), root, fake, time.Now(), stopper); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	// 同一列同時是 pendingStop 也是回收候選時會被停兩次；停止本身冪等，這是
	// Task 7 review 記過的已知無害重複，所以這裡只驗「有停到，而且停的都是這
	// 個 session」，不驗次數。
	assertStoppedOnly := func(what string, got []string) {
		t.Helper()
		if len(got) == 0 {
			t.Fatalf("after sweep, %s stops = %#v, want at least one", what, got)
		}
		for _, s := range got {
			if s != session {
				t.Fatalf("after sweep, %s stopped %q, want only %q", what, s, session)
			}
		}
	}
	assertStoppedOnly("driver", stopper.stopped)
	assertStoppedOnly("tmux", fake.Stopped)
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

// final review 2026-08-06, test honesty: the /api/a2a/gate-log route
// (a2a_admin.go, ReadGateLog(h.Root, session query param, a2aLimit)) had no
// test at all — only its 404-when-disabled behaviour was covered. This
// exercises the route's actual job: reading real entries back, honouring
// ?session= and ?limit=, and note in the final report that this route has
// no CLI or UI consumer as of this fix — that gap is out of scope here.
func TestAdminA2AGateLogRoute(t *testing.T) {
	h, root := newA2AAdmin(t)
	entries := []GateLogEntry{
		{At: "t1", Session: "aa-a-s1", Tool: "Bash", Outcome: "allowed"},
		{At: "t2", Session: "aa-a-s1", Tool: "Read", Outcome: "allowed"},
		{At: "t3", Session: "aa-a-s2", Tool: "Bash", Outcome: "denied_bash_rule"},
	}
	for _, e := range entries {
		if err := AppendGateLog(root, e); err != nil {
			t.Fatalf("AppendGateLog: %v", err)
		}
	}

	rec := adminReq(t, h, http.MethodGet, "/api/a2a/gate-log", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("gate-log = %d %s", rec.Code, rec.Body.String())
	}
	var all []GateLogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("gate-log (no filter) = %d entries, want 3: %#v", len(all), all)
	}

	rec = adminReq(t, h, http.MethodGet, "/api/a2a/gate-log?session=aa-a-s1", "")
	var filtered []GateLogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("gate-log?session=aa-a-s1 = %#v, want 2 entries", filtered)
	}

	rec = adminReq(t, h, http.MethodGet, "/api/a2a/gate-log?limit=1", "")
	var limited []GateLogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &limited); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(limited) != 1 || limited[0].At != "t3" {
		t.Fatalf("gate-log?limit=1 = %#v, want only the last entry (t3)", limited)
	}
}

// cfg.A2A.Enabled == false 時 /api/a2a/* 一律 404。
func TestAdminA2AIs404WhenDisabled(t *testing.T) {
	root := t.TempDir()
	_ = AtomicWriteJSON(ConfigPath(root), map[string]any{"a2a": map[string]any{"enabled": false}})
	h := AdminHandler{Root: root}
	for _, p := range []string{"/api/a2a/agents", "/api/a2a/callers", "/api/a2a/tasks", "/api/a2a/audit", "/api/a2a/gate-log"} {
		if rec := adminReq(t, h, http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 while a2a is disabled", p, rec.Code)
		}
	}
}
