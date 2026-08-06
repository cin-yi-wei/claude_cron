package channelagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// 這個檔案是第四輪併發審查（round 14）5 個 finding 的回歸測試。每一個測試在
// 修正之前都會失敗，而且是「因為正確的理由」失敗（斷言不成立，不是編譯錯誤
// 或逾時掛死）。

// --- Critical 1：dispatch 失敗不可以用過期快照蓋掉 executor 已經落地的身分 ---

// TestDispatchFailureKeepsSandboxIdentitySoSweepCanReclaimIt 釘住
// a2a_server.go 派送失敗那條路：handler 手上的 task 是「呼叫 Executor.Start
// 之前」的快照，Worktree/Branch 都還是空的；executor 的 markFailed 已經把真
// 正建出來的 Worktree/Branch/Session 寫進那一列。handler 若把自己的舊快照
// Upsert 回去，那一列就再也沒有任何欄位指向磁碟上真的存在的東西 —— 活著的
// tmux session、它的 worktree、sandbox root、政策檔全部變成沒有主人的孤兒，
// 任何 sweep 都掃不到（sweep 的候選清單完全靠 tasks.json 的 Worktree/Session
// 產生），而且不計入併發上限。
func TestDispatchFailureKeepsSandboxIdentitySoSweepCanReclaimIt(t *testing.T) {
	s, root := newTestA2AServer(t)
	fake := &FakeSessionManager{FailOn: "inject"}
	s.Executor = NewSandboxExecutor(root, fake)

	rec := postRPC(t, s.Handler(), "secret-1",
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"go"}}`)
	var resp RPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatalf("inject 失敗必須回報 dispatch failed，got %s", rec.Body.String())
	}

	session := SessionNameFor("codereview", "c1")
	wantWorktree := SandboxWorktree("/p/x", session)

	tasks, _ := LoadTasks(root)
	got, ok := tasks.ByContext("c1")
	if !ok {
		t.Fatal("派送失敗後 row 必須留下來")
	}
	if got.State != TaskFailed {
		t.Fatalf("state = %s, want %s", got.State, TaskFailed)
	}
	if got.Session != session || got.Worktree != wantWorktree || got.Branch != BranchFor(session) {
		t.Fatalf("派送失敗的 row 必須仍然指向 executor 真的建出來的沙盒，否則沒有任何機制找得到它：got session=%q worktree=%q branch=%q，want session=%q worktree=%q branch=%q",
			got.Session, got.Worktree, got.Branch, session, wantWorktree, BranchFor(session))
	}

	// 證明「找得到」不只是欄位好看：讓鑑識保留上限把這一列擠出來，sweep 必須
	// 真的停掉它的 session 並刪掉它的 worktree。
	seedNewerFailedSandboxes(t, root, MaxRetainedFailedSandboxes)
	if _, reclaimed, err := SweepTimeouts(context.Background(), root, fake, time.Now(), nil); err != nil || reclaimed != 1 {
		t.Fatalf("SweepTimeouts = (reclaimed %d, err %v), want reclaimed 1, err nil", reclaimed, err)
	}
	if !containsString(fake.Stopped, session) {
		t.Fatalf("sweep 必須停掉這個孤兒 session，Stopped=%#v", fake.Stopped)
	}
	if !containsString(fake.Removed, wantWorktree) {
		t.Fatalf("sweep 必須移除這個孤兒 worktree，Removed=%#v", fake.Removed)
	}
}

// seedNewerFailedSandboxes 塞 n 列「比現在新」的 failed row（都還握著
// worktree），把鑑識保留上限撐爆，好讓待測的那一列成為最舊、第一個被回收的
// candidate。CompletedAt 刻意設在未來，避免跟待測那一列同秒而讓排序結果不
// 確定。
func seedNewerFailedSandboxes(t *testing.T, root string, n int) {
	t.Helper()
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	err := WithTasks(root, func(tasks *TaskStore) error {
		for i := 0; i < n; i++ {
			ctx := fmt.Sprintf("filler%02d", i)
			session := SessionNameFor("codereview", ctx)
			tasks.Upsert(A2ATask{
				ContextID: ctx, TaskID: ctx, Agent: "codereview", CallerID: "peer-a",
				Session: session, Worktree: SandboxWorktree("/p/x", session),
				Branch: BranchFor(session), State: TaskFailed, CompletedAt: future,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed filler rows: %v", err)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- Critical 2：callers.json / agents.json 的 read-modify-write 必須序列化 ---

// TestConcurrentCallerAdminWritesNeverLoseAnUpdate 釘住那個「安全控制悄悄失
// 效」的案例：兩個 admin 請求同時對同一個 caller 做 read-modify-write（一個
// revoke、一個改授權等級），沒有序列化時後寫的那份整檔覆寫會把前一份丟掉
// —— revoke 回 200 OK，callers.json 裡那個 caller 卻還是 approved，還能繼續
// 認證。每個 caller 各派兩條真正競爭的 goroutine，用同一個 start channel 一
// 起放行（-race 下跑）。
func TestConcurrentCallerAdminWritesNeverLoseAnUpdate(t *testing.T) {
	h, root := newA2AAdmin(t)
	const n = 30

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("peer-%02d", i)
		if rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers", `{"caller_id":"`+id+`"}`); rec.Code != http.StatusCreated {
			t.Fatalf("register %s = %d %s", id, rec.Code, rec.Body.String())
		}
		if rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers/"+id+"/approve", `{"capabilities":["read"]}`); rec.Code != http.StatusOK {
			t.Fatalf("approve %s = %d %s", id, rec.Code, rec.Body.String())
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("peer-%02d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers/"+id+"/revoke", ""); rec.Code != http.StatusOK {
				t.Errorf("revoke %s = %d %s", id, rec.Code, rec.Body.String())
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if rec := adminReq(t, h, http.MethodPost, "/api/a2a/callers/"+id+"/level", `{"level":"develop"}`); rec.Code != http.StatusOK {
				t.Errorf("level %s = %d %s", id, rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	callers, err := LoadCallers(root)
	if err != nil {
		t.Fatalf("LoadCallers: %v", err)
	}
	if len(callers.Callers) != n {
		t.Fatalf("callers = %d, want %d（有註冊被覆寫掉）", len(callers.Callers), n)
	}
	for _, c := range callers.Callers {
		if c.Status != CallerRevoked {
			t.Fatalf("caller %s status = %q，want %q —— revoke 回了 200 OK 卻沒有生效，安全控制悄悄失效", c.CallerID, c.Status, CallerRevoked)
		}
		if c.GrantLevel != GrantDevelop {
			t.Fatalf("caller %s grant level = %q, want %q（更新遺失）", c.CallerID, c.GrantLevel, GrantDevelop)
		}
	}
}

// TestConcurrentAgentAdminWritesNeverLoseAnUpdate 是 agents.json 的對稱案
// 例：N 個併發建立請求全部回 201，卻只有少數幾個真的留在檔案裡。
func TestConcurrentAgentAdminWritesNeverLoseAnUpdate(t *testing.T) {
	h, root := newA2AAdmin(t)
	const n = 30

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("agent-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"name":%q,"project_dir":"/p/%s","capabilities":["read"],"enabled":true}`, name, name)
			if rec := adminReq(t, h, http.MethodPost, "/api/a2a/agents", body); rec.Code != http.StatusCreated {
				t.Errorf("create %s = %d %s", name, rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	agents, err := LoadAgents(root)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(agents.Agents) != n {
		t.Fatalf("agents = %d, want %d（併發寫入互相覆寫，回了 201 的 agent 不見了）", len(agents.Agents), n)
	}
}

// --- Important 3：同一個 session 的併發 build 必須互斥 ---

// concurrentBuildSpy 記錄同一時間有幾個 build 走在 EnsureWorkspace 裡，並讓
// 第一次呼叫卡在一個由測試控制的 channel 上 —— 用確定的訊號把窗口撐開，不
// 靠 sleep 賭時序。
type concurrentBuildSpy struct {
	*FakeSessionManager
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	seenOne  bool
	entered  chan struct{}
	hold     chan struct{}
}

func (c *concurrentBuildSpy) EnsureWorkspace(_ context.Context, _, _, _ string) error {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.maxSeen {
		c.maxSeen = c.inFlight
	}
	first := !c.seenOne
	c.seenOne = true
	c.mu.Unlock()

	if first {
		close(c.entered)
		<-c.hold
	}

	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return nil
}

func (c *concurrentBuildSpy) maxConcurrent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSeen
}

// TestConcurrentStartsOnTheSameSessionAreMutuallyExclusive 釘住
// a2a_executor.go:153：Start 只拿共享鎖，所以同一個 session 名可能有兩個
// build 同時在跑 —— 兩份 EnsureWorkspace、兩次 WriteSandboxPolicy（第二次
// 可以把一個活著的 readonly 沙盒的政策檔改寫成 level=full）、兩次 Inject。
func TestConcurrentStartsOnTheSameSessionAreMutuallyExclusive(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)

	session := SessionNameFor("codereview", "c1")
	spy := &concurrentBuildSpy{
		FakeSessionManager: &FakeSessionManager{},
		entered:            make(chan struct{}),
		hold:               make(chan struct{}),
	}
	ex := NewSandboxExecutor(root, spy)
	seedTask(t, root, A2ATask{ContextID: "c1", TaskID: "t1", Agent: "codereview", Session: session, State: TaskDispatching, Level: GrantReadOnly})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ex.Start(context.Background(), A2ATask{
			ContextID: "c1", TaskID: "t1", Agent: "codereview", Session: session,
			State: TaskDispatching, Level: GrantReadOnly,
		}, "first")
	}()

	<-spy.entered // 第一個 build 現在確定卡在建置窗口裡（production 最長 90 秒）。

	second := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ex.Start(context.Background(), A2ATask{
			ContextID: "c1", TaskID: "t2", Agent: "codereview", Session: session,
			State: TaskDispatching, Level: GrantFull,
		}, "second")
		close(second)
	}()

	select {
	case <-second:
		// 沒有互斥：第二個 build 在第一個還握著這個 session 的時候整個跑完了。
	case <-time.After(5 * time.Second):
		// 預期行為：第二個 build 被擋在外面等。
	}

	close(spy.hold)
	wg.Wait()

	if got := spy.maxConcurrent(); got != 1 {
		t.Fatalf("同一個 session 同時有 %d 個 build 在跑，必須互斥（1）", got)
	}
}

// TestMarkFailedRefusesRowsThatChangedIdentityOrAreTerminal 釘住
// a2a_executor.go:123：markFailed 完全沒有身分或終態守衛，任何一次遲到的失
// 敗都能把一個活著的、屬於別次派送的 row 翻成 failed，而它的沙盒還在跑。
func TestMarkFailedRefusesRowsThatChangedIdentityOrAreTerminal(t *testing.T) {
	root := t.TempDir()
	agents := AgentStore{}
	_ = agents.Add(Agent{Name: "codereview", ProjectDir: "/p/x", Enabled: true})
	_ = SaveAgents(root, agents)
	session := SessionNameFor("codereview", "c1")
	ex := NewSandboxExecutor(root, &FakeSessionManager{})

	// (a) 身分已經換人：磁碟上是 t2 的活 row，遲到的 t1 失敗不可以動它。
	live := A2ATask{
		ContextID: "c1", TaskID: "t2", Agent: "codereview", Session: session,
		Worktree: SandboxWorktree("/p/x", session), Branch: BranchFor(session),
		State: TaskWorking,
	}
	seedTask(t, root, live)
	ex.markFailed(A2ATask{ContextID: "c1", TaskID: "t1", Agent: "codereview", Session: session}, "late failure", false)
	tasks, _ := LoadTasks(root)
	got, _ := tasks.ByContext("c1")
	if got.State != TaskWorking || got.TaskID != "t2" {
		t.Fatalf("身分已換的 row 不可以被遲到的失敗翻掉：got %#v", got)
	}

	// (b) 已經是終態：別人（operator、sweep）決定的下場不可以被覆寫。
	done := live
	done.State = TaskCompleted
	done.Detail = "sandbox replied"
	done.DetailSafe = true
	seedTask(t, root, done)
	ex.markFailed(A2ATask{ContextID: "c1", TaskID: "t2", Agent: "codereview", Session: session}, "late failure", false)
	tasks, _ = LoadTasks(root)
	got, _ = tasks.ByContext("c1")
	if got.State != TaskCompleted || got.Detail != "sandbox replied" {
		t.Fatalf("終態 row 不可以被 markFailed 覆寫：got %#v", got)
	}
}

// --- Important 5：dispatch 必須跑在有界的 context 上 ---

// wedgedBuildSpy 模擬一個卡死的 git worktree add / tmux 就緒等待：除非
// context 逾時，否則永遠不回來。abort 是測試自己的保險絲，讓「沒有 deadline」
// 這個 bug 以斷言失敗收場，而不是把整個測試 hang 到 go test 逾時。
type wedgedBuildSpy struct {
	*FakeSessionManager
	mu          sync.Mutex
	sawDeadline bool
	entered     chan struct{}
	abort       chan struct{}
}

func (w *wedgedBuildSpy) EnsureWorkspace(ctx context.Context, _, _, _ string) error {
	_, has := ctx.Deadline()
	w.mu.Lock()
	w.sawDeadline = has
	w.mu.Unlock()
	close(w.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.abort:
		return errors.New("測試保險絲：這次建置從來沒有被任何 deadline 解開")
	}
}

func (w *wedgedBuildSpy) deadlineSeen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sawDeadline
}

// TestDispatchRunsOnABoundedContext 釘住 a2a_server.go:106：dispatch 跑在
// context.Background() 上，git worktree add 與 tmux 就緒等待都沒有上限，一個
// 卡死的 build 會永遠握著該 session 的共享鎖 —— sweep 的 TryLock 永遠拿不
// 到，那個沙盒永遠回收不了。
func TestDispatchRunsOnABoundedContext(t *testing.T) {
	restore := a2aDispatchTimeout
	a2aDispatchTimeout = 200 * time.Millisecond
	defer func() { a2aDispatchTimeout = restore }()

	s, root := newTestA2AServer(t)
	spy := &wedgedBuildSpy{
		FakeSessionManager: &FakeSessionManager{},
		entered:            make(chan struct{}),
		abort:              make(chan struct{}),
	}
	s.Executor = NewSandboxExecutor(root, spy)

	done := make(chan struct{})
	go func() {
		postRPC(t, s.Handler(), "secret-1",
			`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"go"}}`)
		close(done)
	}()

	<-spy.entered
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(spy.abort)
		<-done
		t.Fatal("卡死的 dispatch 沒有被解開：dispatch context 必須有 deadline")
	}
	if !spy.deadlineSeen() {
		t.Fatal("dispatch 傳給 SessionManager 的 context 沒有 deadline")
	}

	// 逾時必須真的放開 session 鎖，否則 sweep 的 TryLock 永遠失敗。
	session := SessionNameFor("codereview", "c1")
	unlock, ok := tryLockSandboxSessionForTeardown(session)
	if !ok {
		t.Fatal("dispatch 逾時之後 session 的共享鎖沒有被釋放，sweep 永遠拿不到拆除鎖")
	}
	unlock()

	// 而且那一列必須留在一個之後 sweep 處理得了的狀態（終態 + 完整身分）。
	tasks, _ := LoadTasks(root)
	got, okRow := tasks.ByContext("c1")
	if !okRow || got.State != TaskFailed || got.Session != session || got.Worktree == "" {
		t.Fatalf("逾時的 row 必須是終態且仍帶著完整身分讓 sweep 找得到：got %#v", got)
	}
}
