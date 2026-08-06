package channelagent

import "sync"

// sandboxSessionLocks 讓「使用某個 session 名的沙盒」（建立、追問投遞）與
// 「拆除同一個 session 名的沙盒」不可能同時進行。
//
// 為什麼光靠帳面比對不夠：sweep 第 2 步的三個破壞動作（sm.Stop、
// sm.RemoveWorkspace、os.RemoveAll(SandboxRoot(...))）是對第 1 步記下的
// 「確定性路徑」執行的。contextId 由呼叫方選、SessionNameFor 與
// SandboxWorktree 都是它的確定性函式，所以合法的同 contextId 追問會落在完全
// 相同的路徑上 —— 新起的 session 會被殺、新建的 worktree 會被 --force 刪，而
// row 完好地指向已不存在的東西，一路掛到 2 小時硬逾時。
//
// 只做身分重確認（D3(b)）仍有窗口（確認完到動手之間），只做這把鎖（D3(a)）
// 擋不住跨行程；兩者一起才完整。
//
// 為什麼是 RWMutex 而不是單純的 Mutex：「使用」這個 session 的動作不只一種
// ——SandboxExecutor.Start 在建立(可能長達 90 秒),SandboxExecutor.DeliverFollowUp
// 在既有的 dispatching/working row 上投遞追問。這兩者彼此必須可以同時存在:
// 一個追問送達的時候,同一個 session 的 Start 完全可能還卡在 EnsureWorkspace
// 裡(這正是既有測試 TestFollowUpDuringInFlightDrainQueueDispatchDoesNotDoubleDispatch
// 釘住的行為 —— 追問不等整個派送做完就必須送達)。如果兩者搶同一把單純
// mutex,追問會被迫等到整個派送結束才能送,舊有的、必須維持綠燈的測試就會
// 死鎖。所以「使用」用共享鎖(RLock):Start 與 DeliverFollowUp 可以並存;
// sweep 第 2 步的拆除動作用互斥鎖，但用 TryLock（非阻塞）而不是 Lock：它
// 只在這個瞬間沒有任何人在使用時才動手，拿不到就放棄這個 candidate、留給
// 下一次 sweep（sweep 絕不能在一個活著的建置上等待——見
// tryLockSandboxSessionForTeardown 的說明）。拿到鎖之後任何新的使用者都要
// 等它做完（做完之後，一個晚到的 DeliverFollowUp 會在鎖內重新確認 row 還
// 活著，看到 row 已經被 sweep 清空就直接拒絕投遞 —— 不會把剛刪掉的目錄重
// 建回來）。
//
// 鎖序（違反即死鎖）：lockSandboxSession 系列 → tasksMu。
// SandboxExecutor.Start、SandboxExecutor.DeliverFollowUp 與 sweep 第 2 步都
// 照這個順序；WithTasks 的 callback 內永遠不得取得 session 鎖。
//
// build 是第三個角色（round 14 review, Important 3）：共享鎖擋得住「使用 vs
// 拆除」，卻擋不住「兩個建置同時落在同一個 session 名上」——兩個 Start 都拿
// 得到 RLock，於是兩份 EnsureWorkspace、兩次 WriteSandboxPolicy（第二次能把
// 一個活著的 readonly 沙盒的政策檔改寫成 level=full）、兩次 Inject 全部疊在
// 一起。build 這把互斥鎖只有 Start 會拿，所以 DeliverFollowUp 仍然可以在一
// 個還在建置中的 session 上投遞追問（既有的
// TestFollowUpDuringInFlightDrainQueueDispatchDoesNotDoubleDispatch 釘住的
// 行為不變）。
//
// 鎖序（違反即死鎖）：build → 共享/互斥的 mu → tasksMu。拆除路徑只碰 mu，
// 不碰 build，所以不存在互相等待的環。
type sessionLock struct {
	mu    sync.RWMutex
	build sync.Mutex
	refs  int
}

var sandboxSessionLocks = struct {
	mu sync.Mutex
	m  map[string]*sessionLock
}{m: map[string]*sessionLock{}}

// acquireSessionLock 找到（或建立）session 名對應的 *sessionLock，並在
// sandboxSessionLocks.mu 底下把 refs 加一，好讓釋放時知道能不能刪掉這個
// map 項目。真正的 RLock/Lock 由呼叫方在拿到 *sessionLock 之後自己做——這一
// 步只保證同一個 session 名永遠拿到同一個 *sessionLock 物件。
func acquireSessionLock(session string) *sessionLock {
	sandboxSessionLocks.mu.Lock()
	defer sandboxSessionLocks.mu.Unlock()
	l, ok := sandboxSessionLocks.m[session]
	if !ok {
		l = &sessionLock{}
		sandboxSessionLocks.m[session] = l
	}
	l.refs++
	return l
}

// releaseSessionLock 是 acquireSessionLock 的另一半：refs 計數讓最後一個
// 釋放者刪掉 map 項目——contextId 由呼叫方選，沒有這個清理，長期執行下 map
// 會無上限成長。
func releaseSessionLock(session string, l *sessionLock) {
	sandboxSessionLocks.mu.Lock()
	defer sandboxSessionLocks.mu.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(sandboxSessionLocks.m, session)
	}
}

// lockSandboxSession 取得 session 名的共享鎖（RLock），供「使用」這個
// session 的呼叫方使用（建立中的 SandboxExecutor.Start、投遞追問的
// SandboxExecutor.DeliverFollowUp）。多個使用者可以同時持有；只有
// tryLockSandboxSessionForTeardown 的互斥鎖會被它們擋住，彼此之間不會。
func lockSandboxSession(session string) func() {
	l := acquireSessionLock(session)
	l.mu.RLock()
	return func() {
		l.mu.RUnlock()
		releaseSessionLock(session, l)
	}
}

// lockSandboxSessionForBuild 是 SandboxExecutor.Start 專用的取鎖方式：先取
// 這個 session 名的 build 互斥鎖（同一時間只准一個建置），再取共享鎖（讓拆
// 除路徑照舊被擋住、讓 DeliverFollowUp 照舊可以並存）。順序固定 build → mu，
// 全程不得反向；拆除路徑只用 TryLock 取 mu、從不碰 build，所以兩者之間沒有
// 互相等待的環。
//
// 為什麼不是把 Start 改成拿互斥的 mu：那會讓追問被迫等整個建置（最長 90
// 秒）跑完才送得出去，既有的
// TestFollowUpDuringInFlightDrainQueueDispatchDoesNotDoubleDispatch 會死鎖
// ——那個「追問不等派送做完」的行為必須維持（見上面 RWMutex 的說明）。
func lockSandboxSessionForBuild(session string) func() {
	l := acquireSessionLock(session)
	l.build.Lock()
	l.mu.RLock()
	return func() {
		l.mu.RUnlock()
		l.build.Unlock()
		releaseSessionLock(session, l)
	}
}

// tryLockSandboxSessionForTeardown 嘗試取得 session 名的互斥鎖（Lock），只
// 供 sweep 第 2 步的拆除動作使用：只有在這個瞬間沒有任何 Start/DeliverFollowUp
// 正持有共享鎖時才會成功，成功後任何新的使用者都要等它做完才能繼續（那時它
// 們會在鎖內重新確認 row 是否還活著）。
//
// 為什麼是 TryLock 而不是 Lock（阻塞版）：handleRPC 在請求的 goroutine 上呼叫
// Executor.Start，它會持有共享鎖直到整個建立完成——最長 90 秒，git worktree
// add 或 tmux 卡住時無上限。sweep 的 Lock() 不吃 ctx、也沒有逾時，若用阻塞
// 版，一個卡住的 HTTP 派送就能把 sweep 卡死，連帶卡死 DrainQueue、
// EnsureSandboxDrivers 與 serve 關機時的 driver.StopAll()。sweep 因此絕不
// 能在一個活著的建置上等待：拿不到就直接放棄這個 candidate，跟身分重確認失
// 敗時完全一樣的處理——留給下一次 sweep 重試，而不是拆掉它。
func tryLockSandboxSessionForTeardown(session string) (unlock func(), ok bool) {
	l := acquireSessionLock(session)
	if !l.mu.TryLock() {
		releaseSessionLock(session, l)
		return nil, false
	}
	return func() {
		l.mu.Unlock()
		releaseSessionLock(session, l)
	}, true
}
