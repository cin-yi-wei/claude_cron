package channelagent

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// a2aRuntime 讓 admin handler 拿得到 serve 行程裡的 driver 與 session manager
// （撤銷必須停掉真的 goroutine 與真的 tmux session）。由 main.go 在
// `if cfg.A2A.Enabled` 內呼叫 SetA2ARuntime 設定；沒設定時兩者為 nil，
// terminateTasks 會跳過對應步驟。用 package var 而不是改 RunAdminServer 的
// 簽章，是為了讓這段接線完全留在 A2A 的 kill switch 底下。
var a2aRuntime = struct {
	mu      sync.RWMutex
	ss      SessionManager
	stopper SandboxStopper
}{}

func SetA2ARuntime(sm SessionManager, stopper SandboxStopper) {
	a2aRuntime.mu.Lock()
	a2aRuntime.ss, a2aRuntime.stopper = sm, stopper
	a2aRuntime.mu.Unlock()
}

func a2aRuntimeFor(h AdminHandler) (SessionManager, SandboxStopper) {
	if h.A2ASessions != nil || h.A2AStopper != nil {
		return h.A2ASessions, h.A2AStopper // 測試注入優先
	}
	a2aRuntime.mu.RLock()
	defer a2aRuntime.mu.RUnlock()
	return a2aRuntime.ss, a2aRuntime.stopper
}

// adminAgentDTO / adminCallerDTO：任何 GET 都不得回傳 credential 或
// callback_token，改回 has_credential / has_callback。
type adminAgentDTO struct {
	Name         string   `json:"name"`
	ProjectDir   string   `json:"project_dir"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	ChannelID    string   `json:"channel_id,omitempty"`
	Enabled      bool     `json:"enabled"`
}

type adminCallerDTO struct {
	CallerID            string   `json:"caller_id"`
	Status              string   `json:"status"`
	GrantedCapabilities []string `json:"granted_capabilities"`
	GrantLevel          string   `json:"grant_level"`
	HasCredential       bool     `json:"has_credential"`
	HasCallback         bool     `json:"has_callback"`
}

type adminA2ATaskDTO struct {
	ContextID     string `json:"context_id"`
	TaskID        string `json:"task_id"`
	Agent         string `json:"agent"`
	CallerID      string `json:"caller_id"`
	State         string `json:"state"`
	Level         string `json:"level"`
	Branch        string `json:"branch"`
	StartedAt     string `json:"started_at"`
	CompletedAt   string `json:"completed_at,omitempty"`
	CallbackState string `json:"callback_state,omitempty"`
}

// errA2AStoreUnchanged 從 WithCallers / WithAgents 的 callback 回傳，代表
// 「什麼都沒改，不要寫檔」（最常見的是找不到那個 id）。呼叫端用自己的
// missing 旗標分辨它與真正的存檔失敗，回對應的 HTTP 狀態。
var errA2AStoreUnchanged = errors.New("a2a: store left unchanged")

// serveA2A 處理 /api/a2a/*。rest 是 "/api/a2a/" 之後的部分。
// cfg.A2A.Enabled == false 時一律 404：關掉 kill switch 就等於這些路由不存在。
func (h AdminHandler) serveA2A(w http.ResponseWriter, r *http.Request, rest string) {
	cfg, err := LoadConfig(h.Root)
	if err != nil || !cfg.A2A.Enabled {
		http.NotFound(w, r)
		return
	}
	switch {
	case rest == "agents":
		switch r.Method {
		case http.MethodGet:
			h.listA2AAgents(w)
		case http.MethodPost:
			h.createA2AAgent(w, r)
		default:
			methodNotAllowed(w)
		}
	case strings.HasPrefix(rest, "agents/"):
		h.a2aAgentAction(w, r, strings.TrimPrefix(rest, "agents/"))
	case rest == "callers":
		switch r.Method {
		case http.MethodGet:
			h.listA2ACallers(w)
		case http.MethodPost:
			h.registerA2ACaller(w, r)
		default:
			methodNotAllowed(w)
		}
	case strings.HasPrefix(rest, "callers/"):
		h.a2aCallerAction(w, r, strings.TrimPrefix(rest, "callers/"))
	case rest == "tasks":
		h.listA2ATasks(w)
	case strings.HasPrefix(rest, "tasks/"):
		name, ok := strings.CutSuffix(strings.TrimPrefix(rest, "tasks/"), "/cancel")
		if !ok || r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		sm, stopper := a2aRuntimeFor(h)
		if err := CancelTask(r.Context(), h.Root, name, sm, stopper); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONResponse(w, map[string]string{"status": "canceled"})
	case rest == "audit":
		entries, err := ReadAudit(h.Root)
		if err != nil {
			http.Error(w, "audit unavailable", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, tailAudit(entries, a2aLimit(r, 200)))
	case rest == "gate-log":
		entries, err := ReadGateLog(h.Root, r.URL.Query().Get("session"), a2aLimit(r, 200))
		if err != nil {
			http.Error(w, "gate log unavailable", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, entries)
	default:
		http.NotFound(w, r)
	}
}

func a2aLimit(r *http.Request, def int) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			return n
		}
	}
	return def
}

func tailAudit(entries []AuditEntry, limit int) []AuditEntry {
	if limit > 0 && len(entries) > limit {
		return entries[len(entries)-limit:]
	}
	return entries
}

func (h AdminHandler) listA2AAgents(w http.ResponseWriter) {
	agents, err := LoadAgents(h.Root)
	if err != nil {
		http.Error(w, "agent store unavailable", http.StatusInternalServerError)
		return
	}
	out := make([]adminAgentDTO, 0, len(agents.Agents))
	for _, a := range agents.Agents {
		out = append(out, adminAgentDTO{a.Name, a.ProjectDir, a.Description, a.Capabilities, a.ChannelID, a.Enabled})
	}
	writeJSONResponse(w, out)
}

func (h AdminHandler) createA2AAgent(w http.ResponseWriter, r *http.Request) {
	var in adminAgentDTO
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// 讀 → 改 → 寫全部包在 WithAgents 的鎖內：兩個併發建立請求各自讀到同一
	// 份快照、各自 append、後寫的整檔覆蓋前寫的，回了 201 的 agent 就這樣消
	// 失（round 14 review, Critical 2）。
	var addErr error
	if err := WithAgents(h.Root, func(agents *AgentStore) error {
		addErr = agents.Add(Agent{
			Name: in.Name, ProjectDir: in.ProjectDir, Description: in.Description,
			Capabilities: in.Capabilities, ChannelID: in.ChannelID, Enabled: in.Enabled,
		})
		return addErr
	}); err != nil {
		if addErr != nil {
			http.Error(w, addErr.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "cannot save agents", http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"name": in.Name})
}

func (h AdminHandler) a2aAgentAction(w http.ResponseWriter, r *http.Request, rest string) {
	sm, stopper := a2aRuntimeFor(h)
	if name, ok := strings.CutSuffix(rest, "/disable"); ok {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		n, err := DisableAgent(r.Context(), h.Root, name, sm, stopper)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONResponse(w, map[string]any{"status": "disabled", "canceled": n})
		return
	}
	if name, ok := strings.CutSuffix(rest, "/enable"); ok {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		missing := false
		if err := WithAgents(h.Root, func(agents *AgentStore) error {
			a, ok2 := agents.Get(name)
			if !ok2 {
				missing = true
				return errA2AStoreUnchanged
			}
			a.Enabled = true
			agents.Remove(name)
			return agents.Add(a)
		}); err != nil {
			if missing {
				http.Error(w, "unknown agent", http.StatusNotFound)
				return
			}
			http.Error(w, "cannot save agents", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, map[string]string{"status": "enabled"})
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	// 刪除前先停用：否則還在跑的沙盒會失去它的 agent 記錄，sweep 也就查不到
	// ProjectDir 可以拿來回收 worktree。
	if _, err := DisableAgent(r.Context(), h.Root, rest, sm, stopper); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	missing := false
	if err := WithAgents(h.Root, func(agents *AgentStore) error {
		if !agents.Remove(rest) {
			missing = true
			return errA2AStoreUnchanged
		}
		return nil
	}); err != nil {
		if missing {
			http.Error(w, "unknown agent", http.StatusNotFound)
			return
		}
		http.Error(w, "cannot save agents", http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, map[string]string{"status": "removed"})
}

func (h AdminHandler) listA2ACallers(w http.ResponseWriter) {
	callers, err := LoadCallers(h.Root)
	if err != nil {
		http.Error(w, "caller store unavailable", http.StatusInternalServerError)
		return
	}
	out := make([]adminCallerDTO, 0, len(callers.Callers))
	for _, c := range callers.Callers {
		out = append(out, adminCallerDTO{
			CallerID: c.CallerID, Status: string(c.Status),
			GrantedCapabilities: c.GrantedCapabilities,
			GrantLevel:          string(c.EffectiveGrantLevel()),
			HasCredential:       c.Credential != "",
			HasCallback:         c.CallbackURL != "",
		})
	}
	writeJSONResponse(w, out)
}

// registerA2ACaller 註冊一個呼叫方。未給 credential 就產生 32-byte base64url，
// 且**只在這個回應裡出現一次** —— 之後任何 GET 都拿不到它。
func (h AdminHandler) registerA2ACaller(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CallerID   string `json:"caller_id"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.CallerID == "" {
		http.Error(w, "caller_id is required", http.StatusBadRequest)
		return
	}
	if in.Credential == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			http.Error(w, "cannot generate credential", http.StatusInternalServerError)
			return
		}
		in.Credential = base64.RawURLEncoding.EncodeToString(buf)
	}
	// 同上：整段 read-modify-write 在 WithCallers 的鎖內完成，否則併發註冊會
	// 互相覆寫（round 14 review, Critical 2）。
	var regErr error
	if err := WithCallers(h.Root, func(callers *CallerStore) error {
		regErr = callers.Register(in.CallerID, in.Credential)
		return regErr
	}); err != nil {
		if regErr != nil {
			http.Error(w, regErr.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "cannot save callers", http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{
		"caller_id":  in.CallerID,
		"credential": in.Credential, // 只出現這一次
	})
}

func (h AdminHandler) a2aCallerAction(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sm, stopper := a2aRuntimeFor(h)
	if id, ok := strings.CutSuffix(rest, "/revoke"); ok {
		n, err := RevokeCaller(r.Context(), h.Root, id, sm, stopper)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONResponse(w, map[string]any{"status": "revoked", "canceled": n})
		return
	}
	var in struct {
		Capabilities []string `json:"capabilities"`
		Level        string   `json:"level"`
		URL          string   `json:"url"`
		Token        string   `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	// 參數驗證（包含 ValidateCallbackURL 的 DNS 解析——絕不可以在
	// callersMu 之內做的慢動作）全部排在進鎖之前，鎖內只剩純記憶體的
	// read-modify-write。
	switch {
	case strings.HasSuffix(rest, "/approve"):
		if in.Level != "" && !ValidGrantLevel(GrantLevel(in.Level)) {
			http.Error(w, "level must be readonly, develop or full", http.StatusBadRequest)
			return
		}
	case strings.HasSuffix(rest, "/level"):
		if !ValidGrantLevel(GrantLevel(in.Level)) {
			http.Error(w, "level must be readonly, develop or full", http.StatusBadRequest)
			return
		}
	case strings.HasSuffix(rest, "/callback"):
		// 目的地在設定當下與觸發當下各驗一次。
		if in.URL != "" {
			if _, verr := ValidateCallbackURL(in.URL, defaultCallbackResolver); verr != nil {
				http.Error(w, verr.Error(), http.StatusBadRequest)
				return
			}
		}
	default:
		http.NotFound(w, r)
		return
	}

	missing := false
	if err := WithCallers(h.Root, func(callers *CallerStore) error {
		switch {
		case strings.HasSuffix(rest, "/approve"):
			id := strings.TrimSuffix(rest, "/approve")
			if !callers.Approve(id, in.Capabilities) {
				missing = true
				return errA2AStoreUnchanged
			}
			if in.Level != "" {
				callers.SetGrantLevel(id, GrantLevel(in.Level))
			}
		case strings.HasSuffix(rest, "/level"):
			id := strings.TrimSuffix(rest, "/level")
			if !callers.SetGrantLevel(id, GrantLevel(in.Level)) {
				missing = true
				return errA2AStoreUnchanged
			}
		case strings.HasSuffix(rest, "/callback"):
			id := strings.TrimSuffix(rest, "/callback")
			if !callers.SetCallback(id, in.URL, in.Token) {
				missing = true
				return errA2AStoreUnchanged
			}
		}
		return nil
	}); err != nil {
		if missing {
			http.Error(w, "unknown caller", http.StatusNotFound)
			return
		}
		http.Error(w, "cannot save callers", http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, map[string]string{"status": "ok"})
}

func (h AdminHandler) listA2ATasks(w http.ResponseWriter) {
	tasks, err := LoadTasks(h.Root)
	if err != nil {
		http.Error(w, "task store unavailable", http.StatusInternalServerError)
		return
	}
	out := make([]adminA2ATaskDTO, 0, len(tasks.Tasks))
	for _, t := range tasks.Tasks {
		out = append(out, adminA2ATaskDTO{
			ContextID: t.ContextID, TaskID: t.TaskID, Agent: t.Agent, CallerID: t.CallerID,
			State: string(t.State), Level: string(t.Level), Branch: t.Branch,
			StartedAt: t.StartedAt, CompletedAt: t.CompletedAt, CallbackState: t.CallbackState,
		})
	}
	writeJSONResponse(w, out)
}
