package channelagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// TaskExecutor dispatches an accepted task into a sandbox. Phase 2 ships only
// StubExecutor; Phase 3 adds the real one. Keeping this an interface is what
// lets every test run without tmux.
type TaskExecutor interface {
	Start(ctx context.Context, task A2ATask, prompt string) error
}

// FollowUpDeliverer is an OPTIONAL capability of a TaskExecutor: delivering a
// message into a task that is ALREADY TaskDispatching or TaskWorking,
// without repeating any part of a fresh dispatch (no EnsureWorkspace, no
// TrustFolder, no policy write, no Sessions.Start — the sandbox already
// exists or is already coming up). handleRPC's message/send follow-up path
// type-asserts for this rather than putting it on TaskExecutor itself, so
// executors that don't model a real sandbox (StubExecutor, test doubles like
// failingExecutor) aren't forced to implement it. A follow-up against one of
// those is still fully safe either way — the row is never re-claimed,
// regressed or re-Started, regardless of whether this interface is
// satisfied — it just isn't actually delivered anywhere real, which matches
// what those doubles model in the first place. SandboxExecutor implements
// it (a2a_executor.go).
type FollowUpDeliverer interface {
	DeliverFollowUp(ctx context.Context, task A2ATask, prompt string) error
}

// StubExecutor records calls and does nothing else.
type StubExecutor struct {
	Calls      int
	LastTask   A2ATask
	LastPrompt string
}

func (s *StubExecutor) Start(_ context.Context, task A2ATask, prompt string) error {
	s.Calls++
	s.LastTask = task
	s.LastPrompt = prompt
	return nil
}

// a2aContextIDRe bounds the caller-controlled contextId. SessionNameFor feeds
// it through sanitize (watcher.go), which strips every non-alphanumeric
// character. If this let through dashes, underscores, dots, spaces, etc.,
// two different contextIds (e.g. "c-1" and "c_1") could sanitize down to the
// same session name and collide once real sandboxes exist. Restricting the
// charset to plain alphanumerics makes sanitize a no-op on any accepted
// contextId, so distinct valid contextIds can never collide downstream.
// 1-128 chars is a conservative bound on a caller-supplied token.
var a2aContextIDRe = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

// errContextHijack signals, from inside a WithTasks callback, that the
// submitted contextId is actively owned by a different caller. Returning it
// makes WithTasks discard the attempted upsert; handleRPC then distinguishes
// it from a genuine store error to decide which RPC failure and audit entry
// to write.
var errContextHijack = errors.New("a2a: contextId is owned by another caller")

// errContextAgentSwitch 表示這個 contextId 已經綁在另一個 agent 上。
// SessionNameFor 與 SandboxWorktree 都含 agent 名，而 Upsert 以 contextId 為
// key 整列覆寫，所以換 agent 再送一次會讓舊的 aa-<oldagent>-<ctx>（活著的
// tmux session + ~80MB worktree）不再被任何 row 參照 —— 沒有任何程式碼掃
// sandboxes/，RunningCount 也數不到它，8 併發上限對它完全無效。
var errContextAgentSwitch = errors.New("a2a: contextId is bound to another agent")

// errDispatchFailAlreadyRecorded 從派送失敗那條路的 WithTasks callback 回
// 傳，代表磁碟上那一列已經是終態（executor 的 markFailed 已經記下更完整的
// 身分與理由），或者這一列已經不是這次失敗要負責的那次派送嘗試（同一個
// contextId 在這個 handler 呼叫 Executor.Start 之後、fallback 真正落地之前
// 被合法重送並認領出一個新的 dispatching/working row——round 2026-08-06
// final review, Important 1）。兩種情況都回傳 error 讓 WithTasks 整個放棄
// 寫入，而不是用 handler 手上那份呼叫前的過期快照覆蓋掉它。
var errDispatchFailAlreadyRecorded = errors.New("a2a: dispatch failure already recorded on the row")

// MessageSendParams is the params body of the message/send method.
type MessageSendParams struct {
	Agent     string `json:"agent"`
	ContextID string `json:"contextId"`
	Text      string `json:"text"`
	TaskID    string `json:"taskId"`
	Level     string `json:"level"`
}

// A2AServer serves the Agent Card and the JSON-RPC endpoint. It MUST be mounted
// on a port separate from the admin API, which can create shell-capable
// bindings and must never be externally reachable.
type A2AServer struct {
	Root     string
	BaseURL  string
	Executor TaskExecutor
	// DispatchContext scopes sandbox creation. It must NOT be the request's
	// context: a client disconnect would cancel git worktree add or the tmux
	// start midway, leaving a half-built sandbox that the forensics rule then
	// keeps forever. Nil means context.Background().
	DispatchContext context.Context
}

// a2aDispatchTimeout 是單一次沙盒派送（EnsureWorkspace + Sessions.Start +
// Inject）的上限。變數而不是常數，只為了讓測試能把窗口縮到毫秒級（比照
// adapters.go 的 injectSubmitDelay），production 不得改寫它。
//
// 取值的兩個邊界：下限要蓋得住文件記載的開機時間 —— tmux 開機在
// worktree.go 有 sessionBootDelay = 90 秒的上限，再加上一次冷的
// `git worktree add`；上限必須小於 DispatchStaleAfter（5 分鐘），這樣一個真
// 的卡死的建置會先被自己的 deadline 解開、由 markFailed 留下明確理由，而不
// 是拖到 sweep 的「派送中崩潰」猜測路徑才被處理。
//
// 為什麼非有界不可：dispatch 過去跑在 context.Background() 上，
// `git worktree add` 與 tmux 就緒等待因此完全沒有上限（兩者都經
// adapters.go 的 runExternalCommand → exec.CommandContext，逾時與否完全取決
// 於傳進來的 ctx）。SandboxExecutor.Start 整段建置持有該 session 的共享鎖，
// 於是一個卡死的建置會永遠握著它，sweep 第 2 步的 TryLock 永遠拿不到 ——
// 那個沙盒的 worktree / sandbox root 就永遠回收不了。
var a2aDispatchTimeout = 4 * time.Minute

// dispatchCtx returns the context used for the (slow, detached) sandbox
// dispatch, as opposed to r.Context(), which the rest of handleRPC keeps
// using since parsing/auth/store access genuinely should abort if the
// caller goes away. 回傳的 cancel 必須在派送結束後呼叫（釋放 timer）。
//
// 逾時觸發時，卡在 exec 裡的 git/tmux 呼叫會拿到 ctx.Err()，Start 一路把它
// 當成一般的派送失敗處理：markFailed 寫下理由與完整身分、Start 回傳 error、
// defer 的 unlock 放開這個 session 的 build/共享鎖。於是那一列變成一個
// sweep 之後處理得了的終態 row，而不是一個永遠握著鎖、永遠回收不了的沙盒。
func (s *A2AServer) dispatchCtx() (context.Context, context.CancelFunc) {
	parent := s.DispatchContext
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, a2aDispatchTimeout)
}

func (s *A2AServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent.json", s.handleCard)
	mux.HandleFunc("/a2a", s.handleRPC)
	return mux
}

func (s *A2AServer) handleCard(w http.ResponseWriter, r *http.Request) {
	agents, err := LoadAgents(s.Root)
	if err != nil {
		http.Error(w, "agent store unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(BuildAgentCard(s.BaseURL, agents))
}

func writeRPC(w http.ResponseWriter, resp RPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// unauthorizedAuditThrottle 讓對憑證的暴力嘗試不會把 a2a-audit.jsonl 灌爆：
// 以來源 IP 為 key，每秒最多一筆。上限 1024 個 key，滿了就整批清空 —— 一個
// 攻擊者可以用偽造來源撐爆 map，整批清空比 LRU 簡單且效果相同（限流的目的
// 是護住 log，不是精確計量）。
type unauthorizedAuditThrottle struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

var unauthorizedAudits = &unauthorizedAuditThrottle{seen: map[string]time.Time{}}

func (t *unauthorizedAuditThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.seen) > 1024 {
		t.seen = map[string]time.Time{}
	}
	if last, ok := t.seen[key]; ok && now.Sub(last) < time.Second {
		return false
	}
	t.seen[key] = now
	return true
}

// sourceHost 取請求的來源 host（去掉 port）。
func sourceHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// auditBadRequest 記錄一個已認證但格式／目標有問題的請求。與 unauthorized 分
// 開：呼叫方是誰已經知道了，這是「他們送了什麼壞東西」。
func (s *A2AServer) auditBadRequest(r *http.Request, callerID, agent, contextID, reason string) {
	_ = AppendAudit(s.Root, AuditEntry{
		At:         time.Now().UTC().Format(time.RFC3339),
		CallerID:   callerID,
		Agent:      agent,
		ContextID:  contextID,
		Summary:    reason,
		Outcome:    "bad_request",
		RemoteAddr: sourceHost(r),
	})
}

func (s *A2AServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPC(w, RPCFail(nil, RPCParseError, "unreadable body"))
		return
	}
	req, rerr := ParseRPC(body)
	if rerr != nil {
		writeRPC(w, RPCFail(nil, rerr.Code, rerr.Message))
		return
	}

	callers, err := LoadCallers(s.Root)
	if err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "caller store unavailable"))
		return
	}
	caller, ok := callers.Authenticate(bearer(r))
	if !ok {
		// 認證失敗完全不留稽核，等於一個以「需要誰要求了什麼的持久紀錄」為
		// 存在理由的對外監聽器，卻對暴力嘗試視而不見（I8）。以來源 IP 限流，
		// 記憑證指紋（HMAC-SHA256，per-install 金鑰，前 8 hex）而非憑證本
		// 身——絕不記憑證。
		host := sourceHost(r)
		if unauthorizedAudits.allow(host, time.Now()) {
			_ = AppendAudit(s.Root, AuditEntry{
				At:           time.Now().UTC().Format(time.RFC3339),
				Outcome:      "unauthorized",
				CredentialFP: credentialFingerprint(s.Root, bearer(r)),
				RemoteAddr:   host,
			})
		}
		writeRPC(w, RPCFail(req.ID, RPCUnauthorized, "unknown or unapproved caller"))
		return
	}

	switch req.Method {
	case "message/send":
		s.handleMessageSend(w, r, req, caller)
	case "tasks/get":
		s.handleTasksGet(w, r, req, caller)
	default:
		s.auditBadRequest(r, caller.CallerID, "", "", "unsupported method "+req.Method)
		writeRPC(w, RPCFail(req.ID, RPCMethodNotFound, "unsupported method "+req.Method))
	}
}

// handleMessageSend 是 message/send 的完整處理：驗證 params、找 agent、算有效
// 授權等級、以 WithTasks 原子性地判斷「新派送 / 排隊 / 對已存在的 dispatch
// 送 follow-up」、再實際呼叫 Executor。
func (s *A2AServer) handleMessageSend(w http.ResponseWriter, r *http.Request, req RPCRequest, caller Caller) {
	var p MessageSendParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.auditBadRequest(r, caller.CallerID, "", "", "malformed params")
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "malformed params"))
		return
	}
	// 目的地由 operator 記在 caller 記錄裡。請求裡出現任何 callback 欄位就
	// 拒絕整個請求（不是忽略）—— 忽略會讓呼叫方以為自己設定成功了。
	//
	// 逐個 key 精確比對在 round-12-review Minor 4 被戳破：這裡的黑名單比對
	// 本身是大小寫敏感的字串比較，攻擊者送 "CallbackUrl" 或
	// "callbackurl"（跟清單裡的 "callbackUrl" 只差大小寫）就逐字比對不中，
	// 放它過去——回應看起來「成功」，呼叫方會誤以為自己真的設定了回呼目
	// 的地（沒有 SSRF 風險，因為沒有任何程式碼真的去讀這幾個欄位，但這個
	// 檢查存在的理由正是「告知呼叫方目的地被忽略」，悄悄放行就等於違反了
	// 這個檢查自己的目的）。改成把每個 key 轉小寫再比對，蓋掉任何大小寫變
	// 體。
	forbiddenCallbackKeys := map[string]bool{
		"callbackurl":    true,
		"callback_url":   true,
		"webhookurl":     true,
		"webhook_url":    true,
		"callbacktoken":  true,
		"callback_token": true,
	}
	var rawParams map[string]json.RawMessage
	_ = json.Unmarshal(req.Params, &rawParams)
	for k := range rawParams {
		if forbiddenCallbackKeys[strings.ToLower(k)] {
			s.auditBadRequest(r, caller.CallerID, p.Agent, p.ContextID, "request supplied a callback destination")
			writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "callback destinations are configured by the operator, not per request"))
			return
		}
	}
	if p.Agent == "" || p.ContextID == "" {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "agent and contextId are required"))
		return
	}
	if !a2aContextIDRe.MatchString(p.ContextID) {
		s.auditBadRequest(r, caller.CallerID, p.Agent, p.ContextID, "contextId must be 1-128 alphanumeric characters")
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "contextId must be 1-128 alphanumeric characters"))
		return
	}
	// p.TaskID 未驗證、未設長度上限：不可達路徑或 session 名，但可讓呼叫方在
	// task store 裡塞 ~1 MiB blob，而 task store 是每 10 秒整檔讀寫一次的。
	if len(p.TaskID) > 128 {
		s.auditBadRequest(r, caller.CallerID, p.Agent, p.ContextID, "taskId exceeds 128 characters")
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "taskId must be at most 128 characters"))
		return
	}

	agents, err := LoadAgents(s.Root)
	if err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "agent store unavailable"))
		return
	}
	agent, ok := agents.Get(p.Agent)
	if !ok || !agent.Enabled {
		s.auditBadRequest(r, caller.CallerID, p.Agent, p.ContextID, "unknown agent "+p.Agent)
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "unknown agent "+p.Agent))
		return
	}

	// The grant list is the whole policy: every capability the agent needs must
	// have been granted to this caller. No runtime prompt. An agent that
	// declares zero capabilities must fail closed, not open — it must state
	// what it needs in order to be callable at all.
	if len(agent.Capabilities) == 0 {
		// Distinct outcome from an ordinary grant denial: this means the agent
		// itself is misconfigured (nothing to grant), not that the caller is
		// under-privileged.
		_ = AppendAudit(s.Root, AuditEntry{
			At:        time.Now().UTC().Format(time.RFC3339),
			CallerID:  caller.CallerID,
			Agent:     p.Agent,
			ContextID: p.ContextID,
			Summary:   p.Text,
			Outcome:   "forbidden_no_capabilities",
		})
		writeRPC(w, RPCFail(req.ID, RPCForbidden, "agent "+agent.Name+" declares no capabilities and cannot be called"))
		return
	}
	for _, need := range agent.Capabilities {
		if !caller.Allows(need) {
			_ = AppendAudit(s.Root, AuditEntry{
				At:        time.Now().UTC().Format(time.RFC3339),
				CallerID:  caller.CallerID,
				Agent:     p.Agent,
				ContextID: p.ContextID,
				Summary:   p.Text,
				Outcome:   "forbidden",
			})
			writeRPC(w, RPCFail(req.ID, RPCForbidden, "caller lacks capability "+need))
			return
		}
	}

	// 有效等級 = min(請求的 level, caller 的授權上限)。請求未給則取 caller 的。
	// 請求高於授權 → RPCForbidden + 一筆稽核;請求的字串不是三個已知等級之一
	// → RPCInvalidParams(不是靜默降級,那會讓呼叫方以為自己拿到了 full)。
	callerLevel := caller.EffectiveGrantLevel()
	requested := callerLevel
	if p.Level != "" {
		requested = GrantLevel(p.Level)
		if !ValidGrantLevel(requested) {
			writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "level must be readonly, develop or full"))
			return
		}
	}
	effective := MinGrantLevel(requested, callerLevel)
	if effective != requested {
		_ = AppendAudit(s.Root, AuditEntry{
			At:        time.Now().UTC().Format(time.RFC3339),
			CallerID:  caller.CallerID,
			Agent:     p.Agent,
			ContextID: p.ContextID,
			Summary:   p.Text,
			Outcome:   "forbidden_level",
		})
		writeRPC(w, RPCFail(req.ID, RPCForbidden, "requested level exceeds this caller's grant"))
		return
	}

	task := A2ATask{
		ContextID: p.ContextID,
		TaskID:    p.TaskID,
		Agent:     agent.Name,
		CallerID:  caller.CallerID,
		Session:   SessionNameFor(agent.Name, p.ContextID),
		State:     TaskSubmitted,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Prompt:    p.Text,
		Level:     effective,
	}

	// contextId is caller-controlled. Without an ownership check, a second
	// caller could reuse another caller's contextId and overwrite its task row
	// (CallerID, Session, State), making the original task unbookkeepable. A
	// caller may only reuse a contextId that is unclaimed or already theirs.
	//
	// Ownership applies regardless of state, and so does the agent-match
	// check right below it: Session and worktree names are functions of
	// (agent, contextId) (SessionNameFor, SandboxWorktree), and Upsert keys
	// on contextId alone, so a caller resubmitting the same contextId against
	// a DIFFERENT agent would overwrite the row and orphan the old
	// aa-<oldagent>-<ctx> sandbox (a live tmux session + its worktree) with
	// nothing left pointing at it (D1). This is checked in every state,
	// including terminal ones, because the whole point is that the same
	// caller MAY legitimately resubmit a finished contextId — just not
	// against a different agent.
	//
	// The load, ownership/agent check, live-row check, capacity check and
	// upsert all happen inside one WithTasks call so a concurrent request can
	// never interleave between the check and the write. That is what makes
	// the capacity claim atomic (I2): hasCapacity is read and the claim
	// (submitted -> dispatching) is written in the very same locked section,
	// so this row counts against RunningCount from the instant any other
	// request could possibly observe it — a check in one locked section and
	// a mark in another would reopen the same race in a new shape.
	//
	// A send against a contextId whose row is ALREADY TaskDispatching or
	// TaskWorking is a follow-up, not a new dispatch — some other call (this
	// same caller's earlier request, or DrainQueue) already owns that row's
	// dispatch and may still be inside EnsureWorkspace/Sessions.Start. Two
	// distinct bugs follow from treating it as a fresh dispatch instead
	// (review round 2, important 1 and 2): (a) re-claiming and re-Starting it
	// races the in-flight EnsureWorkspace/Sessions.Start on the same
	// worktree/session, and if the loser's Start then fails (a git ref lock,
	// an inject error), markFailed marks the row failed while the winner's
	// session is still live; (b) when capacity happens to be full at that
	// moment, Upsert-ing a fresh TaskSubmitted row over a live TaskWorking
	// one regresses its state and wipes its Worktree/Branch — RunningCount
	// silently drops, a 9th sandbox can start over cap, and the live
	// worktree loses its only reference (SweepTimeouts skips rows with an
	// empty Worktree). A follow-up must therefore never be claimed, never
	// take a capacity slot, and never overwrite the row's identity — it only
	// updates Prompt (so a query reflects what was last said) and leaves
	// State/Worktree/Branch/Session/StartedAt/DispatchedAt exactly as they
	// are. Actually delivering the follow-up into the sandbox's inbox
	// (Inject touches the filesystem) happens OUTSIDE this lock, in the
	// isFollowUp branch below — WithTasks forbids that here.
	//
	// A row that is TaskSubmitted (still queued, not yet claimed by anyone)
	// or terminal (finished) is NOT a live in-flight dispatch, so it falls
	// through to the ordinary new-dispatch path unchanged: a terminal
	// contextId may be legitimately reused by its same caller/agent (see the
	// ownership/agent checks above), and a still-queued one simply gets its
	// queued row replaced by this request's own (possibly now-claimable) one.
	var hasCapacity bool
	var isFollowUp bool
	var followUpTask A2ATask
	err = WithTasks(s.Root, func(tasks *TaskStore) error {
		if existing, ok := tasks.ByContext(p.ContextID); ok {
			if existing.CallerID != caller.CallerID {
				return errContextHijack
			}
			if existing.Agent != "" && existing.Agent != task.Agent {
				return errContextAgentSwitch
			}
			if existing.State == TaskDispatching || existing.State == TaskWorking {
				merged := existing
				merged.Prompt = task.Prompt
				tasks.Upsert(merged)
				isFollowUp = true
				followUpTask = merged
				return nil
			}
		}
		// 容量在 upsert 之「前」算，翻成 dispatching 在同一個 critical
		// section 內完成：於是這一列立刻開始計入 RunningCount，下一個並發
		// 請求算出的就是真話。這同時修掉 I2。
		hasCapacity = HasCapacity(*tasks)
		if hasCapacity {
			task.State = TaskDispatching
			task.DispatchedAt = time.Now().UTC().Format(time.RFC3339)
			// 這一刻起這一列就是「這次派送嘗試」——見 A2ATask.DispatchAttempt
			// 的說明,派送失敗那條回呼路徑(下面)靠它分辨磁碟上這一列是不是
			// 還是自己要負責的那一次嘗試。
			task.DispatchAttempt = nextDispatchAttempt()
		}
		tasks.Upsert(task)
		return nil
	})
	if err != nil {
		if errors.Is(err, errContextHijack) {
			// The most operationally important audit path: this looks like a
			// deliberate attempt to interfere with another caller's task, so
			// the entry must record both who was rejected (caller.CallerID)
			// and which contextId they tried to take over.
			_ = AppendAudit(s.Root, AuditEntry{
				At:        time.Now().UTC().Format(time.RFC3339),
				CallerID:  caller.CallerID,
				Agent:     p.Agent,
				ContextID: p.ContextID,
				Summary:   p.Text,
				Outcome:   "forbidden_hijack",
			})
			writeRPC(w, RPCFail(req.ID, RPCForbidden, "contextId is owned by another caller"))
			return
		}
		if errors.Is(err, errContextAgentSwitch) {
			_ = AppendAudit(s.Root, AuditEntry{
				At:        time.Now().UTC().Format(time.RFC3339),
				CallerID:  caller.CallerID,
				Agent:     p.Agent,
				ContextID: p.ContextID,
				Summary:   p.Text,
				Outcome:   "forbidden_agent_switch",
			})
			writeRPC(w, RPCFail(req.ID, RPCForbidden, "contextId is already bound to a different agent"))
			return
		}
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "cannot persist task"))
		return
	}

	if isFollowUp {
		// Outside the lock: best-effort delivery into the already-live
		// sandbox's inbox. This never calls Executor.Start and never touches
		// task.State — that lifecycle belongs entirely to whichever call is
		// actually running this row's dispatch.
		if fd, ok := s.Executor.(FollowUpDeliverer); ok {
			dctx, cancel := s.dispatchCtx()
			derr := fd.DeliverFollowUp(dctx, followUpTask, p.Text)
			cancel()
			if derr != nil {
				// 交付失敗不能讓任務狀態退回、也不能觸發重新派送 —— 沙盒本身
				// 還活著,只是這一句沒送到,跟「派送失敗」是完全不同性質的
				// 錯誤(那套邏輯的前提是沙盒根本沒起來)。log 之外還要記到
				// task row 上：final review 2026-08-06, Important 3 —— 修之
				// 前這裡只 log，呼叫方拿到的是成功回應，兩小時硬逾時才會發現
				// 真相。用 appendDetail 疊加（不覆寫既有 Detail），固定安全
				// 字串（不帶 derr 內容——那可能夾著 host 路徑），這樣 tasks/get
				// 立刻就能查到，呼叫方可以決定要不要重送。不改 State：沙盒仍
				// 活著，這不是終態轉換。
				log.Printf("a2a: follow-up delivery failed for contextId %s (agent=%s): %v", followUpTask.ContextID, followUpTask.Agent, derr)
				if serr := WithTasks(s.Root, func(tasks *TaskStore) error {
					cur, ok := tasks.ByContext(followUpTask.ContextID)
					if !ok || cur.TaskID != followUpTask.TaskID || isTerminal(cur.State) {
						return errA2AStoreUnchanged
					}
					cur.Detail, cur.DetailSafe = appendDetail(cur.Detail, cur.DetailSafe,
						"a follow-up message failed to deliver into the sandbox; it may not have been seen", true)
					tasks.Upsert(cur)
					return nil
				}); serr != nil && !errors.Is(serr, errA2AStoreUnchanged) {
					log.Printf("a2a: failed to record follow-up delivery failure for %s/%s: %v", followUpTask.Agent, followUpTask.ContextID, serr)
				}
			}
		}
		_ = AppendAudit(s.Root, AuditEntry{
			At:        time.Now().UTC().Format(time.RFC3339),
			CallerID:  caller.CallerID,
			Agent:     agent.Name,
			ContextID: followUpTask.ContextID,
			TaskID:    followUpTask.TaskID,
			Summary:   p.Text,
			Outcome:   "follow_up",
		})
		writeRPC(w, RPCOK(req.ID, map[string]any{
			"contextId": followUpTask.ContextID,
			"taskId":    followUpTask.TaskID,
			"state":     string(followUpTask.State),
		}))
		return
	}

	if !hasCapacity {
		// Queued, not rejected: it stays in TaskSubmitted for DrainQueue.
		// Distinct outcome from "accepted": no sandbox actually started.
		_ = AppendAudit(s.Root, AuditEntry{
			At:        time.Now().UTC().Format(time.RFC3339),
			CallerID:  caller.CallerID,
			Agent:     agent.Name,
			ContextID: task.ContextID,
			TaskID:    task.TaskID,
			Summary:   p.Text,
			Outcome:   "queued",
		})
		writeRPC(w, RPCOK(req.ID, map[string]any{
			"contextId": task.ContextID,
			"taskId":    task.TaskID,
			"state":     string(TaskSubmitted),
			"queued":    true,
		}))
		return
	}

	dctx, cancelDispatch := s.dispatchCtx()
	err = s.Executor.Start(dctx, task, p.Text)
	cancelDispatch()
	if err != nil {
		// Never echo the underlying error to an internet-facing caller: once
		// the real executor lands, this detail will carry worktree paths, git
		// output and tmux state — exactly the private project information
		// this design exists to keep off the wire. Log it server-side instead,
		// and mark the task failed so it stops occupying capacity forever.
		log.Printf("a2a: dispatch failed for task %s (agent=%s contextId=%s): %v", task.TaskID, task.Agent, task.ContextID, err)
		// task 是這個 handler 在呼叫 Executor.Start **之前**取得的快照：
		// Worktree/Branch 都還是空的（handleRPC 只填得出 Session，它是
		// contextId 的確定性函式）。executor 在建立過程中已經把真正落到磁碟
		// 上的 Worktree/Branch persist 進這一列了，失敗時 markFailed 也把它
		// 們保住。把這份過期快照整列 Upsert 回去，等於用空字串蓋掉那些欄位
		// ——一個活著的 tmux session、它的 worktree、sandbox root 與政策檔就
		// 再也沒有任何 row 指向它們：SweepTimeouts 的候選清單完全靠掃
		// tasks.Tasks 的 Worktree/Session 產生，回收不了、也不計入併發上限
		// （round 14 review, Critical 1）。
		//
		// 所以這裡在鎖內重讀那一列再決定：
		//   - 已經是終態：executor 的 markFailed（或 sweep/operator）早就記
		//     下了這一列的下場，而且那份紀錄帶著完整、正確的身分。完全不
		//     動它——覆寫只會用更差的資訊取代更好的。
		//   - 身分已經換人：這裡的 task 是這個 handler 呼叫 Executor.Start
		//     **之前**取得的快照。它回來、走到這裡之間沒有任何鎖保護——同一
		//     個 contextId 可能已經合法重送並被認領成一個新的
		//     dispatching/working row（markFailed 先幫上一次嘗試定案成終
		//     態，呼叫方看到終態合法重送，新一輪認領又剛好搶在這個舊
		//     handler 回來之前完成）。只比對 isTerminal 抓不到這個窗口：新
		//     row 通常還不是終態。這裡改成跟 markFailed（a2a_executor.go）
		//     與訊息追問失敗那條路（上面 :544）同樣的做法，比對
		//     TaskID/DispatchAttempt——不吻合就代表這一列已經是別次派送嘗
		//     試，不可以被這次遲到的失敗覆寫（round 2026-08-06 final review,
		//     Important 1；探針證明過會誤殺一個活著的 t2-live row）。
		//   - 還不是終態、身分也還是這次嘗試：只可能是 Start 在 markFailed
		//     之外的路徑失敗（例如 load agents），這時才由這裡補一筆
		//     failed，且是疊在磁碟上那一列之上（保留它的
		//     Worktree/Branch/Session），不是拿舊快照覆蓋。
		if serr := WithTasks(s.Root, func(tasks *TaskStore) error {
			cur, ok := tasks.ByContext(task.ContextID)
			if !ok {
				// 這一列根本還沒落地（persist 之前就失敗）：沒有任何磁碟上的
				// 東西需要保護，照這次請求自己的身分寫一筆失敗紀錄。
				cur = task
			} else if isTerminal(cur.State) || cur.TaskID != task.TaskID || cur.DispatchAttempt != task.DispatchAttempt {
				return errDispatchFailAlreadyRecorded
			}
			cur.State = TaskFailed
			cur.Detail = err.Error()
			// DetailSafe=false：err 是 Executor.Start 的回傳值，跟上面註解說的
			// 一樣可能夾帶 worktree 路徑、git 輸出、tmux 狀態。這一列的 Detail
			// 之後可以被 tasks/get 查到（taskSnapshotPayload），這裡明確標成
			// 不安全，讓那個查詢路徑用固定字串取代，不把它原文交給遠端呼叫方。
			cur.DetailSafe = false
			// CompletedAt 過去這裡從沒設過：PruneTasks 的保留期判斷靠
			// !CompletedAt.IsZero()，沒有它這一列永遠排不進終止排名、永遠不
			// 會因為年紀被修剪（round 2026-08-06 final review, Minor 2）。
			cur.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			tasks.Upsert(cur)
			return nil
		}); serr != nil && !errors.Is(serr, errDispatchFailAlreadyRecorded) {
			log.Printf("a2a: failed to persist failed task state for %s/%s: %v", task.Agent, task.ContextID, serr)
		}
		// This branch mutates task state (TaskFailed) and must not do so without
		// a trail. Distinct from both "accepted" and the "forbidden_*" family:
		// the caller was authorized and the request was well-formed — the
		// failure was ours, not theirs. Summary stays the caller's request
		// text, same as every other entry; the executor's raw error (which can
		// carry worktree paths and internal detail) goes only to log.Printf
		// above and task.Detail, never into the audit trail.
		_ = AppendAudit(s.Root, AuditEntry{
			At:        time.Now().UTC().Format(time.RFC3339),
			CallerID:  caller.CallerID,
			Agent:     agent.Name,
			ContextID: task.ContextID,
			TaskID:    task.TaskID,
			Summary:   p.Text,
			Outcome:   "dispatch_failed",
		})
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "dispatch failed"))
		return
	}

	_ = AppendAudit(s.Root, AuditEntry{
		At:        time.Now().UTC().Format(time.RFC3339),
		CallerID:  caller.CallerID,
		Agent:     agent.Name,
		ContextID: task.ContextID,
		TaskID:    task.TaskID,
		Summary:   p.Text,
		Outcome:   "accepted",
	})

	writeRPC(w, RPCOK(req.ID, map[string]any{
		"contextId": task.ContextID,
		"taskId":    task.TaskID,
		"state":     string(task.State),
	}))
}

// TaskGetParams 是 tasks/get 的 params。contextId 必填。
type TaskGetParams struct {
	ContextID string `json:"contextId"`
	TaskID    string `json:"taskId"`
}

// errTaskNotVisible 是「查無此 row」與「這一列屬於別人」共用的訊息。兩者必須
// 完全一致，否則呼叫方可以用錯誤訊息的差異列舉別人的 contextId。
const errTaskNotVisible = "no task for that contextId"

// detailWithheldMessage 取代任何 DetailSafe==false 的 Detail。它本身必須是
// 固定字串——不重複底層錯誤裡的任何字詞、不帶路徑——這樣任何未來忘記把
// 新錯誤路徑標成 safe=false 的人，看到回應也不會誤以為「原文正常回傳」是
// 預期行為（審計時一眼可辨）。
const detailWithheldMessage = "internal error (detail withheld; see operator log)"

// taskSnapshotPayload 是 tasks/get 的回應形狀，也是完成回呼的 body 基底
// （Task 12 在它之上加一個 "event" 欄位）。刻意不含 session / worktree ——
// 那是私有專案資訊，host 路徑與內部簿記沒有理由跨過 HTTP 邊界；state / detail
// 才是這個功能存在的理由。
//
// detail 是「本來」該是沙盒自撰文字才能截斷放行的欄位（規格第六節開放問題
// 8：沒有它就沒有交付），但 Detail 這個欄位本身同時也被 markFailed／派送失
// 敗這幾條路徑拿去裝 err.Error()——那些字串可能包著 host 上的絕對路徑、git
// 輸出、tmux 狀態，跟這裡要放行的「沙盒說了什麼」是完全不同的東西，卻共用
// 同一個欄位（round-11-review Critical）。
//
// 用 A2ATask.DetailSafe 在「寫入的當下」標記來源，取代事後對 Detail 內容做
// 字串比對（例如找有沒有像路徑的子字串）：這欄位一路被 appendDetail／sweep
// 的「;」接續反覆疊加前面留下的線索，寫入時就知道這一段是誰寫的、事後從最
// 終字串裡永遠分不出哪一段來自哪裡——內容比對這條路在這裡註定不可靠，標記
// 來源不會。DetailSafe 為 false（包含從未被明確標成 true 的預設值——fail
// closed）時，一律換成下面固定的 detailWithheldMessage；operator 仍能在
// tasks.json 上看到完整原文，只是不讓它跨過這條 HTTP 邊界。
//
// 放行的 Detail 一樣截斷至 maxDetailBytes；Upsert 已經在寫入時做過這次截
// 斷，這裡再截一次是防禦性的——就算未來某條路徑繞過 Upsert 直接改了
// Detail，回應仍然有界。
func taskSnapshotPayload(t A2ATask) map[string]any {
	detail := t.Detail
	if detail != "" && !t.DetailSafe {
		detail = detailWithheldMessage
	}
	return map[string]any{
		"contextId":   t.ContextID,
		"taskId":      t.TaskID,
		"state":       string(t.State),
		"level":       string(t.Level),
		"branch":      t.Branch,
		"startedAt":   t.StartedAt,
		"completedAt": t.CompletedAt,
		"detail":      truncateBytes(detail, maxDetailBytes),
	}
}

// handleTasksGet 是 tasks/get 的完整處理：只讀，不經過 WithTasks（不做任何
// 變更，LoadTasks 就夠了），只讓呼叫方看到自己（CallerID 相符）的 row，且
// 「查無此列」與「這列是別人的」回完全相同的錯誤，不讓呼叫方靠錯誤訊息的
// 差異列舉別人的 contextId。
func (s *A2AServer) handleTasksGet(w http.ResponseWriter, r *http.Request, req RPCRequest, caller Caller) {
	var p TaskGetParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.auditBadRequest(r, caller.CallerID, "", "", "malformed tasks/get params")
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "malformed params"))
		return
	}
	if !a2aContextIDRe.MatchString(p.ContextID) {
		s.auditBadRequest(r, caller.CallerID, "", p.ContextID, "invalid contextId on tasks/get")
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, "contextId must be 1-128 alphanumeric characters"))
		return
	}
	tasks, err := LoadTasks(s.Root)
	if err != nil {
		writeRPC(w, RPCFail(req.ID, RPCInternalError, "task store unavailable"))
		return
	}
	t, ok := tasks.ByContext(p.ContextID)
	// 擁有權不符與查無此 row 回完全相同的錯誤（不洩漏存在性）。
	if !ok || t.CallerID != caller.CallerID {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, errTaskNotVisible))
		return
	}
	if p.TaskID != "" && p.TaskID != t.TaskID {
		writeRPC(w, RPCFail(req.ID, RPCInvalidParams, errTaskNotVisible))
		return
	}
	writeRPC(w, RPCOK(req.ID, taskSnapshotPayload(t)))
}
