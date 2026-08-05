package channelagent

import (
	"errors"
	"os"
	"path/filepath"
)

type TaskState string

const (
	TaskSubmitted TaskState = "submitted"
	TaskWorking   TaskState = "working"
	TaskCompleted TaskState = "completed"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

// A2ATask is one delegated task, keyed by the A2A contextId. Its sandbox is a
// dedicated tmux session + git worktree.
type A2ATask struct {
	ContextID   string    `json:"context_id"`
	TaskID      string    `json:"task_id"`
	Agent       string    `json:"agent"`
	CallerID    string    `json:"caller_id"`
	Session     string    `json:"session"`
	Worktree    string    `json:"worktree"`
	Branch      string    `json:"branch"`
	State       TaskState `json:"state"`
	StartedAt   string    `json:"started_at"`
	CompletedAt string    `json:"completed_at,omitempty"`
	// Prompt is the caller's original request text. It must be persisted so a
	// task queued at capacity can still be started later by DrainQueue.
	Prompt string `json:"prompt,omitempty"`
	// Detail carries the outcome: the sandbox's reply on success, or the error
	// reason on failure. Never the input — that is Prompt.
	Detail string `json:"detail,omitempty"`
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

// ActiveCount counts tasks occupying a sandbox slot (submitted or working).
func (s TaskStore) ActiveCount() int {
	n := 0
	for _, t := range s.Tasks {
		if t.State == TaskSubmitted || t.State == TaskWorking {
			n++
		}
	}
	return n
}

// CanTransition enforces the state machine: terminal states are final.
func CanTransition(from, to TaskState) bool {
	switch from {
	case TaskSubmitted:
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
