package channelagent

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// adminAgentDTO / adminCallerDTO：任何 GET 都不得回傳 credential 或
// callback_token，改回 has_credential / has_callback。
//
// Filtered / FilterReason（2026-08-06 followup review）：只有 listA2AAgents
// 會填,createA2AAgent 解碼請求體時這兩個欄位永遠是零值——客戶端送了也沒有
// 任何程式碼會讀它們。標出「這筆 entry 存在於 agents.json,但被 LoadAgents
// 的驗證過濾擋在 dispatch 之外」的情況：名稱不合法,或 channel_id 撞到一個
// binding。沒有這兩個欄位，操作者只能看見 LoadAgents 濾過的清單——被排除
// 的 agent 直接從畫面上消失，而且往往是在它建立之後、透過完全跟它無關的
// 另一個操作（例如新建一個 binding）才被排除的，操作者根本沒做任何看起來
// 相關的事，也就無從得知該去修哪一個 agent。
type adminAgentDTO struct {
	Name         string   `json:"name"`
	ProjectDir   string   `json:"project_dir"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	ChannelID    string   `json:"channel_id,omitempty"`
	Enabled      bool     `json:"enabled"`
	Filtered     bool     `json:"filtered,omitempty"`
	FilterReason string   `json:"filter_reason,omitempty"`
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
		if err := CancelTask(h.Root, name); err != nil {
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

// listA2AAgents 用 LoadAgentsFiltered 而不是 LoadAgents：後者是 dispatch 唯一
// 該看到的版本,語意不能動;這裡額外要的是「連被濾掉的也顯示,附上原因」,
// 所以兩者一起列出,用 Filtered/FilterReason 分清楚哪個是哪個
// （2026-08-06 followup review——見 adminAgentDTO 上方的說明）。CLI（原封不動
// 轉印這份 JSON）跟著自動拿到這兩個欄位；UI 是唯一還需要另外改的地方
// （Agents.svelte）。
func (h AdminHandler) listA2AAgents(w http.ResponseWriter) {
	agents, filtered, err := LoadAgentsFiltered(h.Root)
	if err != nil {
		http.Error(w, "agent store unavailable", http.StatusInternalServerError)
		return
	}
	out := make([]adminAgentDTO, 0, len(agents.Agents)+len(filtered))
	for _, a := range agents.Agents {
		out = append(out, adminAgentDTO{
			Name: a.Name, ProjectDir: a.ProjectDir, Description: a.Description,
			Capabilities: a.Capabilities, ChannelID: a.ChannelID, Enabled: a.Enabled,
		})
	}
	for _, f := range filtered {
		out = append(out, adminAgentDTO{
			Name: f.Agent.Name, ProjectDir: f.Agent.ProjectDir, Description: f.Agent.Description,
			Capabilities: f.Agent.Capabilities, ChannelID: f.Agent.ChannelID, Enabled: f.Agent.Enabled,
			Filtered: true, FilterReason: f.Reason,
		})
	}
	writeJSONResponse(w, out)
}

// bindingChannelClaim 回傳（若有）root 底下哪個 binding 正佔用著這個
// channel_id。createA2AAgent 與 updateA2AAgent 都用它在存檔前擋掉「把
// agent 的 channel_id 指到一個 cc- binding 的頻道」這個操作——LoadAgents
// 的驗證過濾（見 a2a_agents.go）會在下一次載入時把撞到的 agent 整個濾
// 掉：不只從 dispatch 消失，GET /api/a2a/agents 這份清單也看不到它，UI
// 再也碰不到，只能靠 CLI 直接改 agents.json 救回來。這正是 a2a_agents.go
// 那條過濾規則原本要防的事（人類訊息被 ingest 進錯的 session）——擋在
// 操作者眼前用 400 拒絕，遠比讓它悄悄存進去、事後才在別的路徑上消失安全。
// bindings.json 只讀不寫，讀取失敗（幾乎不會發生）視為沒有撞名，交由既有
// 的 admin 操作繼續，不因為讀一份不相關的檔案失敗而擋住 agent 管理。
func bindingChannelClaim(root, channelID string) (string, bool) {
	if channelID == "" {
		return "", false
	}
	reg, err := LoadRegistry(root)
	if err != nil {
		return "", false
	}
	for _, b := range reg.Bindings {
		if b.ChannelID == channelID {
			return b.Name, true
		}
	}
	return "", false
}

func (h AdminHandler) createA2AAgent(w http.ResponseWriter, r *http.Request) {
	var in adminAgentDTO
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if name, clash := bindingChannelClaim(h.Root, in.ChannelID); clash {
		http.Error(w, fmt.Sprintf("channel_id %s already belongs to binding %q; an agent's channel must not collide with a binding's, or LoadAgents will silently drop this agent", in.ChannelID, name), http.StatusBadRequest)
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
	if name, ok := strings.CutSuffix(rest, "/disable"); ok {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		n, err := DisableAgent(h.Root, name)
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
			agents.replace(a)
			return nil
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
	if name, ok := strings.CutSuffix(rest, "/update"); ok {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		h.updateA2AAgent(w, r, name)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	// 刪除前先停用：否則還在跑的沙盒會失去它的 agent 記錄，sweep 也就查不到
	// ProjectDir 可以拿來回收 worktree。
	if _, err := DisableAgent(h.Root, rest); err != nil {
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

// adminAgentUpdateDTO 是 PATCH 語意的 update 請求體：每個欄位都是 pointer，
// 「這個 key 完全沒出現在 JSON 裡」與「出現、值是空字串/空陣列」必須分得出
// 來——前者代表「不要動這個欄位」，後者代表「操作者明確要把它清空」。用
// 一般的 value 型別（像 create 用的 adminAgentDTO）做不到這件事：CLI
// 只改一個欄位時，其餘沒帶的欄位會被 JSON 預設值（""、nil）整批覆寫過去，
// 這正是 Gap 1 報告裡點名要避免的「改一個欄位卻清空其他欄位」。
//
// Name 平時只用來偵測「這個請求想改名」並拒絕它——name 是 agent 的身分，
// SessionNameFor／SandboxWorktree 都拿它派生 session 名與 worktree 路徑，
// 改名會讓正在跑的沙盒（如果有）失去自己的記錄，且沒有任何程式碼會去搬移
// 一個活著的 tmux session 或 worktree 去對上新名字。要換名字得刪除重建。
//
// 唯一例外（2026-08-06 followup review）：URL 路徑裡現有的那個名字本身就不
// 合法（例如含空白）時，允許把它改成一個合法的新名字。這種 entry 從一開始
// 就通不過 a2aNameRe，LoadAgents 永遠把它濾掉——dispatch 從沒看過它，
// SessionNameFor 從沒替它算出任何 session 名或 worktree 路徑,所以沒有沙盒
// 可能因此孤兒化。這是名字不合法這一類 entry 唯一真正的修復手段（見
// a2a_agents.go replace 上方的說明：其他欄位可以直接透過 replace 改,名字
// 本身不行）。新名字仍要通過一般的格式與唯一性檢查,見 updateA2AAgent。
//
// Enabled 刻意不在這裡：enable/disable 各自有專用路由，/update 完全不碰
// 這個欄位，維持單一權責。
type adminAgentUpdateDTO struct {
	Name         *string   `json:"name"`
	ProjectDir   *string   `json:"project_dir"`
	Description  *string   `json:"description"`
	Capabilities *[]string `json:"capabilities"`
	ChannelID    *string   `json:"channel_id"`
}

// updateA2AAgent 改一個既有 agent 的 project_dir / description / capabilities /
// channel_id。跟 enable 用同一個 Get → 改本地副本 → replace 手法（見上面
// a2aAgentAction 的 /enable 分支），一樣包在單次 WithAgents 呼叫裡，讀-改-寫
// 對併發請求是原子的。
//
// 這條路由正是修掉 Gap 1 第二個問題的辦法：a2a_server.go 對零 capabilities 的
// agent 是永久 fail-closed（TestZeroCapabilityAgentDeniedByDefault），過去只
// 能刪除重建才能補上 capabilities，把任何跟這個 agent 綁在一起的東西都丟掉。
// 現在操作者可以直接送一次 {"capabilities":[...]} 補救。
//
// renaming 這條分支是名字不合法這一類 entry 唯一能修好名字本身的路：只有
// 「URL 裡現有的名字過不了 a2aNameRe」時才打開,新名字必須合法、且不能跟
// 既有的任何 agent 撞名——兩個檢查都跟 Add 一樣,但故意不呼叫 Add（見
// adminAgentUpdateDTO 與 a2a_agents.go replace 的說明）。renaming 時不能用
// replace：replace 用新的 a.Name 去找同名的舊列,但舊列還是掛在舊名字下,
// 找不到就什麼都不做,是一個看起來成功、實際上沒存到任何東西的假陽性。這裡
// 改成明確的 Remove(舊名) + append(新 entry)。
func (h AdminHandler) updateA2AAgent(w http.ResponseWriter, r *http.Request, name string) {
	var in adminAgentUpdateDTO
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	renaming := false
	if in.Name != nil && *in.Name != name {
		if a2aNameRe.MatchString(name) {
			http.Error(w, "agent name is immutable (it derives session and worktree names); delete and recreate to rename", http.StatusBadRequest)
			return
		}
		if !a2aNameRe.MatchString(*in.Name) {
			http.Error(w, fmt.Sprintf("invalid new agent name %q: use lowercase letters, digits, dashes", *in.Name), http.StatusBadRequest)
			return
		}
		renaming = true
	}
	if in.ChannelID != nil {
		if bname, clash := bindingChannelClaim(h.Root, *in.ChannelID); clash {
			http.Error(w, fmt.Sprintf("channel_id %s already belongs to binding %q; an agent's channel must not collide with a binding's, or LoadAgents will silently drop this agent", *in.ChannelID, bname), http.StatusBadRequest)
			return
		}
	}
	missing := false
	renameConflict := false
	if err := WithAgents(h.Root, func(agents *AgentStore) error {
		a, ok := agents.Get(name)
		if !ok {
			missing = true
			return errA2AStoreUnchanged
		}
		if renaming {
			if _, exists := agents.Get(*in.Name); exists {
				renameConflict = true
				return errA2AStoreUnchanged
			}
		}
		if in.ProjectDir != nil {
			a.ProjectDir = *in.ProjectDir
		}
		if in.Description != nil {
			a.Description = *in.Description
		}
		if in.Capabilities != nil {
			a.Capabilities = *in.Capabilities
		}
		if in.ChannelID != nil {
			a.ChannelID = *in.ChannelID
		}
		if renaming {
			agents.Remove(name)
			a.Name = *in.Name
			agents.Agents = append(agents.Agents, a)
		} else {
			agents.replace(a)
		}
		return nil
	}); err != nil {
		if missing {
			http.Error(w, "unknown agent", http.StatusNotFound)
			return
		}
		if renameConflict {
			http.Error(w, fmt.Sprintf("agent %q already exists", *in.Name), http.StatusBadRequest)
			return
		}
		http.Error(w, "cannot save agents", http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, map[string]string{"status": "updated"})
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
	if id, ok := strings.CutSuffix(rest, "/revoke"); ok {
		n, err := RevokeCaller(h.Root, id)
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
