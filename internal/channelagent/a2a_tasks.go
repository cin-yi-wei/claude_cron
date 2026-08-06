package channelagent

import (
	"errors"
	"os"
	"path/filepath"
)

type TaskState string

const (
	TaskSubmitted TaskState = "submitted"
	// TaskDispatching 是「已取得派送權、沙盒正在建立」。它與 submitted 分開
	// 的唯一理由是關掉重複派送：Start 要到 EnsureWorkspace + Start + Inject
	// 全部成功（最長 90 秒的開機窗口）才寫 TaskWorking，而 A2A cycle 每 10
	// 秒跑一次 DrainQueue —— 沒有這個中間狀態，handler 派送的任務幾乎必然被
	// DrainQueue 再派一次，同一段委派工作跑兩遍。
	TaskDispatching TaskState = "dispatching"
	TaskWorking     TaskState = "working"
	TaskCompleted   TaskState = "completed"
	TaskFailed      TaskState = "failed"
	TaskCanceled    TaskState = "canceled"
)

// A2ATask is one delegated task, keyed by the A2A contextId. Its sandbox is a
// dedicated tmux session + git worktree.
type A2ATask struct {
	ContextID string    `json:"context_id"`
	TaskID    string    `json:"task_id"`
	Agent     string    `json:"agent"`
	CallerID  string    `json:"caller_id"`
	Session   string    `json:"session"`
	Worktree  string    `json:"worktree"`
	Branch    string    `json:"branch"`
	State     TaskState `json:"state"`
	StartedAt string    `json:"started_at"`
	// DispatchedAt 是取得派送權的時刻。StartedAt 不能用：一個排隊數小時後才
	// 被 DrainQueue 撿走的任務，它的 StartedAt 是提交時刻，用它算「派送中卡
	// 住多久」會立刻誤判。
	DispatchedAt string `json:"dispatched_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
	// Prompt is the caller's original request text. It must be persisted so a
	// task queued at capacity can still be started later by DrainQueue.
	Prompt string `json:"prompt,omitempty"`
	// Detail carries the outcome: the sandbox's reply on success, or the error
	// reason on failure. Never the input — that is Prompt. It can also carry a
	// non-fatal warning while the task is still Working (e.g. a folder-trust
	// seed failure) — a transient note the next terminal-state write (success
	// or failure) overwrites; it exists so a task stuck mid-boot shows a
	// reason via status instead of only the serve journal.
	Detail string `json:"detail,omitempty"`
	// Level 是這個任務的有效授權等級,dispatch 當下算出並寫進沙盒政策檔。
	// 空值的 row 不可以起沙盒(SandboxExecutor.Start 會拒絕)。
	Level GrantLevel `json:"level,omitempty"`
	// VanishedStrikes 累計「這個 session 連續被判定不存在」的次數，只有
	// SweepTimeouts 的存活偵測會動它。單一次 tmux 呼叫失敗（fork EAGAIN、
	// 執行檔暫時找不到、serve 關機時 sweep 自己的 ctx 被取消）不足以宣告一
	// 個健康任務死亡；連續累積到 VanishedConfirmStrikes 才真的判定 vanished
	// 並轉 TaskFailed。任何一次判定結果是「活著」就清零——不是遞減，因為
	// 這是「連續」而不是「累計」次數。
	VanishedStrikes int `json:"vanished_strikes,omitempty"`
	// SessionStopPending 是「這一列被存活偵測或撤銷偵測判定終態的當下，它的
	// session 還沒被確認停掉」的耐久標記。只有這兩條路徑會把它設成 true；
	// dispatch-stall 那條既有的一次性 stopTarget 路徑（有自己審過的、窄範
	// 圍才成立的自我化解論證）完全不動它，行為不變。鎖忙的那一刻——最典型
	// 的是一個合法的 DeliverFollowUp 正在投遞，恰好是「這個 session 其實
	// 還活著」的假陽性場景——不能假設之後不會再有機會停掉它，也不能假設
	// 不需要再試：只要這個欄位還是 true，SweepTimeouts 每一輪都會重新嘗
	// 試，成功了才清掉；不論成功與否都完全不動 Session/Worktree（鑑識保留
	// 不受影響，round 10 review, Important）。
	SessionStopPending bool `json:"session_stop_pending,omitempty"`
	// LastMessageID 是最後一次注入這個沙盒的 SourceMessage.MessageID。
	// pendingResultFile 用它比對結果檔的來源 —— session 名是 contextId 的
	// 確定性函式，failed 沙盒又依 forensics 規則保留，沒有這個比對，殘留在
	// outbox/pending 的舊結果檔會把重用同一 contextId 的新任務立刻判為完成。
	LastMessageID string `json:"last_message_id,omitempty"`
}

type TaskStore struct {
	Tasks []A2ATask `json:"tasks"`
}

func TasksPath(root string) string { return filepath.Join(root, "tasks.json") }

func LoadTasks(root string) (TaskStore, error) {
	var s TaskStore
	if err := ReadJSON(TasksPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TaskStore{}, nil
		}
		return TaskStore{}, err
	}
	return s, nil
}

func SaveTasks(root string, s TaskStore) error {
	return AtomicWriteJSON(TasksPath(root), s)
}

func (s *TaskStore) Upsert(t A2ATask) {
	for i := range s.Tasks {
		if s.Tasks[i].ContextID == t.ContextID {
			s.Tasks[i] = t
			return
		}
	}
	s.Tasks = append(s.Tasks, t)
}

func (s *TaskStore) ByContext(contextID string) (A2ATask, bool) {
	for _, t := range s.Tasks {
		if t.ContextID == contextID {
			return t, true
		}
	}
	return A2ATask{}, false
}

// ActiveCount counts tasks that have not yet finished (submitted or working).
// This is NOT the same as "occupying a sandbox slot" — a submitted task is
// still waiting for one. Use RunningCount for capacity gating.
func (s TaskStore) ActiveCount() int {
	n := 0
	for _, t := range s.Tasks {
		if t.State == TaskSubmitted || t.State == TaskWorking {
			n++
		}
	}
	return n
}

// RunningCount 計入 working 與 dispatching：一個 dispatching 的 row 已經在
// 建 worktree、起 tmux session 了，它就是佔著一個槽。漏算它正是 40 個並發
// 請求全部算出「有容量」的原因（I2）。submitted 仍然不算 —— 它還在排隊，
// 把它算進去會讓「一堆排隊、什麼都沒在跑」永久讀成客滿。
func (s TaskStore) RunningCount() int {
	n := 0
	for _, t := range s.Tasks {
		if t.State == TaskWorking || t.State == TaskDispatching {
			n++
		}
	}
	return n
}

// CanTransition enforces the state machine: terminal states are final, and a
// task must pass through TaskDispatching (claim the right to dispatch) before
// it can ever become TaskWorking — submitted -> working directly is no
// longer legal, which is what closes C2 (double dispatch).
func CanTransition(from, to TaskState) bool {
	switch from {
	case TaskSubmitted:
		return to == TaskDispatching || to == TaskFailed || to == TaskCanceled
	case TaskDispatching:
		return to == TaskWorking || to == TaskFailed || to == TaskCanceled
	case TaskWorking:
		return to == TaskCompleted || to == TaskFailed || to == TaskCanceled
	default:
		return false
	}
}

// SessionNameFor builds the sandbox session name. Never collides with cc-.
func SessionNameFor(agent, contextID string) string {
	return "aa-" + agent + "-" + sanitize(contextID)
}
