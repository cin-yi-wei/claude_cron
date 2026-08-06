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

// dispatchCtx returns the context used for the (slow, detached) sandbox
// dispatch, as opposed to r.Context(), which the rest of handleRPC keeps
// using since parsing/auth/store access genuinely should abort if the
// caller goes away.
func (s *A2AServer) dispatchCtx() context.Context {
	if s.DispatchContext != nil {
		return s.DispatchContext
	}
	return context.Background()
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
			if derr := fd.DeliverFollowUp(s.dispatchCtx(), followUpTask, p.Text); derr != nil {
				// 交付失敗不能讓任務狀態退回、也不能觸發重新派送 —— 沙盒本身
				// 還活著,只是這一句沒送到,跟「派送失敗」是完全不同性質的
				// 錯誤(那套邏輯的前提是沙盒根本沒起來)。只記 log,讓呼叫方
				// 之後可以再送一次。
				log.Printf("a2a: follow-up delivery failed for contextId %s (agent=%s): %v", followUpTask.ContextID, followUpTask.Agent, derr)
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

	if err := s.Executor.Start(s.dispatchCtx(), task, p.Text); err != nil {
		// Never echo the underlying error to an internet-facing caller: once
		// the real executor lands, this detail will carry worktree paths, git
		// output and tmux state — exactly the private project information
		// this design exists to keep off the wire. Log it server-side instead,
		// and mark the task failed so it stops occupying capacity forever.
		log.Printf("a2a: dispatch failed for task %s (agent=%s contextId=%s): %v", task.TaskID, task.Agent, task.ContextID, err)
		task.State = TaskFailed
		task.Detail = err.Error()
		if serr := WithTasks(s.Root, func(tasks *TaskStore) error {
			tasks.Upsert(task)
			return nil
		}); serr != nil {
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

// taskSnapshotPayload 是 tasks/get 的回應形狀，也是完成回呼的 body 基底
// （Task 12 在它之上加一個 "event" 欄位）。刻意不含 session / worktree ——
// 那是私有專案資訊，host 路徑與內部簿記沒有理由跨過 HTTP 邊界；state / detail
// 才是這個功能存在的理由。
//
// detail 是沙盒自撰文字，截斷至 maxDetailBytes。把它交出去是對「沙盒文字不
// 流出 HTTP」的刻意放寬，因為沒有它就沒有交付（規格第六節開放問題 8）。
// Upsert 已經在寫入時把 Detail 截到 maxDetailBytes，這裡再截一次是防禦性的
// ——就算未來某條路徑繞過 Upsert 直接改了 Detail，回應仍然有界。
func taskSnapshotPayload(t A2ATask) map[string]any {
	return map[string]any{
		"contextId":   t.ContextID,
		"taskId":      t.TaskID,
		"state":       string(t.State),
		"level":       string(t.Level),
		"branch":      t.Branch,
		"startedAt":   t.StartedAt,
		"completedAt": t.CompletedAt,
		"detail":      truncateBytes(t.Detail, maxDetailBytes),
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
