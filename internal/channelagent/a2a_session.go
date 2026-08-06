package channelagent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
)

// SessionManager isolates every side effect that touches git or tmux, so tests
// can substitute FakeSessionManager and never spawn a real claude session.
type SessionManager interface {
	EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error
	Start(ctx context.Context, session, cwd, registryRoot string) error
	Stop(ctx context.Context, session string) error
	Inject(ctx context.Context, root string, msg SourceMessage) error
	// RemoveWorkspace tears down a sandbox checkout. Reclaiming only the tmux
	// session leaves ~80MB per task on disk, and contextId is caller-chosen, so
	// without this one approved caller can grow the disk without bound.
	RemoveWorkspace(ctx context.Context, projectDir, worktree string) error
	// TrustFolder 預先把 worktree 標成已信任,讓沙盒開機時不會跳資料夾信任
	// 對話框。走介面而不是直接呼叫 EnsureFolderTrusted 是強制要求:後者寫的
	// 是 ~/.claude.json,那是這台機器上所有 claude 行程共用的活檔,一個直接
	// 呼叫它的單元測試會改寫 operator 的線上設定。
	TrustFolder(ctx context.Context, worktree string) error
	// Alive 回報這個 tmux session 是否還在。沙盒死掉（機器重開、session 被砍）
	// 沒有任何其他偵測管道 —— 任務會停在 working 兩小時，然後被判成 canceled
	// 而不是 failed，forensics 保留規則因此套錯邊。
	Alive(ctx context.Context, session string) (bool, error)
}

// TmuxSessionManager is the production implementation, delegating to the same
// helpers the cc- supervisor uses.
type TmuxSessionManager struct{}

func (TmuxSessionManager) EnsureWorkspace(ctx context.Context, projectDir, branch, worktree string) error {
	return EnsureWorktree(ctx, projectDir, branch, worktree)
}

func (TmuxSessionManager) Start(ctx context.Context, session, cwd, registryRoot string) error {
	// SessionManager 只服務 aa- 沙盒(cc- 走 supervisor.go 的自己那條路),
	// 所以這裡一律用不含 SessionStart hook 的沙盒 settings。
	return StartTmuxClaudeSandbox(ctx, session, cwd, registryRoot)
}

func (TmuxSessionManager) TrustFolder(_ context.Context, worktree string) error {
	abs, err := filepath.Abs(worktree)
	if err != nil {
		abs = worktree
	}
	return EnsureFolderTrusted(ClaudeConfigPath(), abs)
}

func (TmuxSessionManager) Stop(ctx context.Context, session string) error {
	return StopTmuxSession(ctx, session)
}

func (TmuxSessionManager) Alive(ctx context.Context, session string) (bool, error) {
	return TmuxSessionAlive(ctx, session)
}

// TmuxSessionAlive 用 `tmux has-session -t` 判斷，但只有「tmux 真的跑起來、
// 回報了明確的離開碼」才解讀成「不存在」——這種情況下不管是「沒有這個
// session」還是「tmux server 沒起來」，對一個應該有 session 在跑的沙盒來說
// 結論相同：它沒在跑。任何讓我們「根本沒問到答案」的失敗都必須回傳非 nil
// 的 error，不可以跟上面那種「問到了、答案是沒有」混在一起——這兩者對呼叫
// 方的意義完全相反：前者是「先當它還活著，之後再查」，後者才是「真的死
// 了，可以動手」。round 9 review 抓到的 Critical：fork EAGAIN（這台機器有
// OOM 史）、tmux 執行檔暫時找不到、以及 sweep 傳入的 ctx 在 serve 關機時被
// 取消，這三種「問不到」的情況，先前全部被這個函式吞成 (false, nil)，跟
// 「真的沒有這個 session」無法區分——sweep 的「這次判不出來就先當它還活
// 著」guard 因此在正式環境裡永遠打不到。
func TmuxSessionAlive(ctx context.Context, session string) (bool, error) {
	err := runExternalCommand(ctx, "tmux", "has-session", "-t", session)
	if err == nil {
		return true, nil
	}
	// ctx 被取消/逾時：這不是 tmux 回報的答案，是我們自己沒等到——最典型
	// 的例子是 serve 關機時 sweep 用的 supCtx 被取消，這時每一個還在跑的
	// row 都不該被判定死亡。優先於下面的離開碼判斷，因為 context 取消時
	// cmd.Run() 回的錯誤型別不保證是 *exec.ExitError。
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	// 執行本身失敗（fork EAGAIN、tmux 執行檔找不到……）：同樣是「問不到」，
	// 不是「問到了，沒有」。把錯誤原樣往上傳。
	return false, err
}

func (TmuxSessionManager) RemoveWorkspace(ctx context.Context, projectDir, worktree string) error {
	return RemoveWorktree(ctx, projectDir, worktree)
}

func (TmuxSessionManager) Inject(ctx context.Context, root string, msg SourceMessage) error {
	created, err := IngestMessages(ctx, root, []SourceMessage{msg})
	if err != nil {
		return err
	}
	if created == 0 {
		// IngestMessages dedups on platform:channel:messageID and reports a
		// duplicate by returning created=0 with a nil error: nothing landed
		// in the inbox, but the call otherwise looks like it succeeded.
		// Silently returning nil here is exactly how a caller
		// (SandboxExecutor.Start) came to believe a message was queued when
		// it had actually been dropped. Treat it as an error so the
		// caller's failure handling fires instead of lying about success.
		return fmt.Errorf("inject: message %s:%s:%s was deduped, nothing queued", msg.Platform, msg.ChannelID, msg.MessageID)
	}
	return nil
}

// FakeSessionManager records calls for assertions. FailOn makes one method
// return an error: "workspace", "start", "stop", "inject", "remove", or
// "trust".
type FakeSessionManager struct {
	Workspaces []string
	Started    []string
	Stopped    []string
	Injected   []SourceMessage
	Removed    []string
	Trusted    []string
	FailOn     string
	// OnRemove, if set, fires once on the first RemoveWorkspace call, then
	// clears itself. Tests use it to inject a state change into tasks.json
	// from inside SweepTimeouts' teardown window — the gap between its step 1
	// (candidate identification) and step 3 (clearing fields for confirmed
	// matches) — to simulate a caller resubmitting the same contextId while
	// teardown for the previous, terminal task is in flight (task-8 review
	// round 3, finding 1).
	OnRemove func()
	// OnStart, if set, fires on every Start call after the session has been
	// recorded. Tests use it to observe the on-disk state that must already be
	// in place by the time a real tmux session would come up — the sandbox
	// policy in particular.
	OnStart func(session string)
	// EnsureWorkspaceHold and EnsureWorkspaceEntered let a test hold a
	// dispatch open at a precise, deterministic point instead of racing on
	// real goroutine scheduling timing (task 6 review round 2, minor 4):
	// EnsureWorkspace records its call, closes EnsureWorkspaceEntered (if
	// set) so a waiting test knows the dispatch is now genuinely in flight,
	// then blocks on EnsureWorkspaceHold (if set) until it is closed. This
	// mirrors production's up-to-90s worktree-add + tmux-boot window without
	// an actual sleep.
	EnsureWorkspaceHold    chan struct{}
	EnsureWorkspaceEntered chan struct{}
	// AliveSessions scripts the Alive method: nil (the zero value) means every
	// session reads as alive, so existing tests that never mention liveness
	// are unaffected. When set, only the sessions explicitly present with
	// value true are alive — everything else (absent key, or explicit false)
	// reads as dead, letting a test simulate a vanished session precisely.
	AliveSessions map[string]bool
	// mu 序列化每個方法對上面那些切片的存取。task 6 之前所有測試都是單一
	// goroutine 呼叫 fake,不需要鎖;task 6 的併發測試 (N 條 goroutine 各自
	// message/send、或 handler 與 DrainQueue 並發) 會從多條 goroutine 同時
	// 呼叫同一個 fake 實例的 EnsureWorkspace/Start/Inject 等方法,對同一個
	// slice append 而不加鎖在 -race 下必炸。既有測試都在並發結束後才讀取欄位
	// (見各方法後段的斷言),所以這個鎖不影響它們。
	mu sync.Mutex
}

func (f *FakeSessionManager) EnsureWorkspace(_ context.Context, _, _, worktree string) error {
	f.mu.Lock()
	if f.FailOn == "workspace" {
		f.mu.Unlock()
		return errors.New("fake workspace failure")
	}
	f.Workspaces = append(f.Workspaces, worktree)
	entered := f.EnsureWorkspaceEntered
	hold := f.EnsureWorkspaceHold
	f.mu.Unlock()
	// entered/hold 必須在放掉 f.mu 之後才處理:一旦持有 hold 阻塞,若還握著
	// mu,任何併發呼叫 fake 其他方法(包括同一次派送稍後可能用到的方法)都會
	// 卡死,不是單純變慢而已。
	if entered != nil {
		close(entered)
	}
	if hold != nil {
		<-hold
	}
	return nil
}

func (f *FakeSessionManager) Start(_ context.Context, session, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOn == "start" {
		return errors.New("fake start failure")
	}
	f.Started = append(f.Started, session)
	if f.OnStart != nil {
		f.OnStart(session)
	}
	return nil
}

func (f *FakeSessionManager) Stop(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOn == "stop" {
		return errors.New("fake stop failure")
	}
	f.Stopped = append(f.Stopped, session)
	return nil
}

func (f *FakeSessionManager) RemoveWorkspace(_ context.Context, _, worktree string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.OnRemove != nil {
		hook := f.OnRemove
		f.OnRemove = nil
		hook()
	}
	if f.FailOn == "remove" {
		return errors.New("fake remove failure")
	}
	f.Removed = append(f.Removed, worktree)
	return nil
}

func (f *FakeSessionManager) Inject(_ context.Context, _ string, msg SourceMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOn == "inject" {
		return errors.New("fake inject failure")
	}
	f.Injected = append(f.Injected, msg)
	return nil
}

func (f *FakeSessionManager) TrustFolder(_ context.Context, worktree string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOn == "trust" {
		return errors.New("fake trust failure")
	}
	// 只記錄呼叫,絕不碰真實的 ~/.claude.json。
	f.Trusted = append(f.Trusted, worktree)
	return nil
}

func (f *FakeSessionManager) Alive(_ context.Context, session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOn == "alive" {
		return false, errors.New("fake alive failure")
	}
	if f.AliveSessions == nil {
		return true, nil // 未腳本化時視為活著，既有測試不受影響
	}
	return f.AliveSessions[session], nil
}
