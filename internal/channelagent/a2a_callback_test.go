package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 假解析器：SSRF 防護的測試絕不做真實 DNS 查詢。
func fixedResolver(ips ...string) func(string) ([]net.IP, error) {
	return func(string) ([]net.IP, error) {
		out := make([]net.IP, 0, len(ips))
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func TestValidateCallbackURLRejectsUnsafeDestinations(t *testing.T) {
	for _, c := range []struct {
		name, url string
		resolve   func(string) ([]net.IP, error)
	}{
		{"plain http", "http://example.com/hook", fixedResolver("93.184.216.34")},
		{"loopback", "https://example.com/hook", fixedResolver("127.0.0.1")},
		{"ipv6 loopback", "https://example.com/hook", fixedResolver("::1")},
		{"rfc1918 10/8", "https://example.com/hook", fixedResolver("10.1.2.3")},
		{"rfc1918 172.16/12", "https://example.com/hook", fixedResolver("172.20.0.5")},
		{"rfc1918 192.168/16", "https://example.com/hook", fixedResolver("192.168.1.1")},
		{"link local", "https://example.com/hook", fixedResolver("169.254.169.254")},
		{"ipv6 ula", "https://example.com/hook", fixedResolver("fc00::1")},
		{"ipv6 link local", "https://example.com/hook", fixedResolver("fe80::1")},
		{"one bad among good", "https://example.com/hook", fixedResolver("93.184.216.34", "127.0.0.1")},
		{"dot local host", "https://box.local/hook", fixedResolver("93.184.216.34")},
		{"dot internal host", "https://api.internal/hook", fixedResolver("93.184.216.34")},
		{"localhost", "https://localhost/hook", fixedResolver("93.184.216.34")},
		{"no host", "https:///hook", fixedResolver("93.184.216.34")},
		// round-12-review Minor 3/4 的探針：以下四項曾經全部被放行。
		{"cgnat / tailscale 100.64/10", "https://example.com/hook", fixedResolver("100.64.0.1")},
		{"ietf protocol assignments 192.0.0/24", "https://example.com/hook", fixedResolver("192.0.0.1")},
		{"benchmark 198.18/15", "https://example.com/hook", fixedResolver("198.18.0.1")},
		{"reserved 240/4", "https://example.com/hook", fixedResolver("240.0.0.1")},
		{"nat64-mapped loopback", "https://example.com/hook", fixedResolver("64:ff9b::7f00:1")},
		{"trailing-dot localhost evades suffix blacklist", "https://localhost./hook", fixedResolver("93.184.216.34")},
		{"trailing-dot internal host evades suffix blacklist", "https://api.internal./hook", fixedResolver("93.184.216.34")},
	} {
		if _, err := ValidateCallbackURL(c.url, c.resolve); err == nil {
			t.Errorf("%s: %q must be rejected", c.name, c.url)
		}
	}
	if _, err := ValidateCallbackURL("https://example.com/hook", fixedResolver("93.184.216.34")); err != nil {
		t.Fatalf("a public https destination must be accepted: %v", err)
	}
}

// 任務狀態機與 callback 的成敗完全解耦。
func TestCallbackFailureNeverBlocksTheTask(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetGrantLevel("peer-a", GrantDevelop)
	callers.SetCallback("peer-a", "https://example.com/hook", "tok")
	_ = SaveCallers(root, callers)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a",
		State: TaskCompleted, Detail: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewCallbackDispatcher(ctx, root)
	d.resolve = fixedResolver("93.184.216.34")
	// 撥號一律失敗：目的地不可達。
	d.dial = func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
	d.retryDelays = []time.Duration{time.Millisecond}

	if n := EnqueueTerminalCallbacks(root, d); n != 1 {
		t.Fatalf("enqueued %d, want 1", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := LoadTasks(root)
		tk, _ := got.ByContext("c1")
		if tk.CallbackState == "failed" {
			if tk.State != TaskCompleted || tk.Detail != "done" {
				t.Fatalf("the task was mutated by a callback failure: %#v", tk)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("callback never reached a terminal callback_state")
}

func TestCallbackPostsTaskSnapshotAndToken(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]any
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotToken = r.Header.Get("X-A2A-Callback-Token")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetCallback("peer-a", "https://hook.example.com/x", "tok-1")
	_ = SaveCallers(root, callers)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a", Level: GrantDevelop,
		State: TaskCompleted, Detail: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewCallbackDispatcher(ctx, root)
	d.resolve = fixedResolver("93.184.216.34")
	// 已檢查過的 IP 換成 httptest 的 loopback 位址：驗的是「連的是預先檢查
	// 過的位址、而不是重新解析」這個行為，不是真的去連公網。
	d.dial = func(dctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(dctx, network, net.JoinHostPort(host, port))
	}
	d.scheme = "http" // httptest.NewServer 是 http；TLS 不是這條測試的標的

	EnqueueTerminalCallbacks(root, d)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := gotBody != nil
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotBody == nil {
		t.Fatal("callback was never delivered")
	}
	if gotBody["event"] != "task.terminal" || gotBody["contextId"] != "c1" || gotBody["state"] != "completed" {
		t.Fatalf("body = %#v", gotBody)
	}
	if gotToken != "tok-1" {
		t.Fatalf("token header = %q", gotToken)
	}
}

// 目的地永遠不接受請求提供 —— 否則這台主機就成了 SSRF 跳板。
func TestMessageSendRejectsCallbackFieldsInParams(t *testing.T) {
	s, _ := newTestA2AServer(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","callbackUrl":"https://evil.example"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","webhookUrl":"https://evil.example"}}`,
	} {
		rec := postRPC(t, s.Handler(), "secret-1", body)
		var resp RPCResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error == nil || resp.Error.Code != RPCInvalidParams {
			t.Fatalf("a request-supplied callback destination must reject the whole request, got %#v", resp.Error)
		}
	}
}

// round-12-review Minor 4：encoding/json 的欄位比對本來就不分大小寫，逐字比
// 對的黑名單可以被大小寫變體繞過去（呼叫方會以為自己成功設定了目的地）。
func TestMessageSendRejectsCallbackFieldsCaseInsensitively(t *testing.T) {
	s, _ := newTestA2AServer(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","CallbackUrl":"https://evil.example"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","callbackurl":"https://evil.example"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","WEBHOOK_URL":"https://evil.example"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"agent":"codereview","contextId":"c1","text":"hi","CallbackToken":"x"}}`,
	} {
		rec := postRPC(t, s.Handler(), "secret-1", body)
		var resp RPCResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error == nil || resp.Error.Code != RPCInvalidParams {
			t.Fatalf("a case-varied callback key must still reject the whole request, got %#v", resp.Error)
		}
	}
}

// round-12-review 探針確認：一個 302 只換來一次伺服器 hit（不跟隨轉址），且
// 3xx 不算成功——回呼最終落在 failed，不是 sent。
func TestCallbackDoesNotFollowRedirects(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Location", "https://evil.example/stolen")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetCallback("peer-a", "https://hook.example.com/x", "tok")
	_ = SaveCallers(root, callers)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a",
		State: TaskCompleted, Detail: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewCallbackDispatcher(ctx, root)
	d.resolve = fixedResolver("93.184.216.34")
	d.dial = func(dctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(dctx, network, net.JoinHostPort(host, port))
	}
	d.scheme = "http"
	d.retryDelays = []time.Duration{time.Millisecond}

	EnqueueTerminalCallbacks(root, d)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := LoadTasks(root)
		tk, _ := got.ByContext("c1")
		if tk.CallbackState == "failed" {
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Fatalf("server hits = %d, want 1 (a redirect must not be followed)", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("callback never reached a terminal callback_state")
}

// 佇列滿了就直接標 dropped、絕不阻塞：不啟動 run() 消費 channel，直接把它
// 填到 callbackQueueSize，證明滿了之後多出來的候選被立即丟棄而不是等待。
func TestCallbackDropsWhenQueueIsFull(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetCallback("peer-a", "https://hook.example.com/x", "tok")
	_ = SaveCallers(root, callers)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a",
		State: TaskCompleted, Detail: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	d := &CallbackDispatcher{
		root:     root,
		ch:       make(chan callbackJob, callbackQueueSize),
		inflight: map[string]bool{},
		resolve:  fixedResolver("93.184.216.34"),
	}
	for i := 0; i < callbackQueueSize; i++ {
		d.ch <- callbackJob{contextID: fmt.Sprintf("filler-%d", i)}
	}

	if n := EnqueueTerminalCallbacks(root, d); n != 0 {
		t.Fatalf("enqueued %d, want 0 (the queue was already full)", n)
	}
	got, _ := LoadTasks(root)
	tk, _ := got.ByContext("c1")
	if tk.CallbackState != "dropped" {
		t.Fatalf("callback_state = %q, want dropped", tk.CallbackState)
	}
}

// 重試次數有界：撥號永遠失敗，attempts 必須恰好等於
// len(retryDelays)+1（第一次嘗試 + 每個 delay 各重試一次），而不是無限重試。
func TestCallbackFailsAfterExhaustingRetries(t *testing.T) {
	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetCallback("peer-a", "https://hook.example.com/x", "tok")
	_ = SaveCallers(root, callers)

	var s TaskStore
	s.Upsert(A2ATask{
		ContextID: "c1", TaskID: "t1", Agent: "a", CallerID: "peer-a",
		State: TaskCompleted, Detail: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = SaveTasks(root, s)

	var attempts int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewCallbackDispatcher(ctx, root)
	d.resolve = fixedResolver("93.184.216.34")
	d.dial = func(context.Context, string, string) (net.Conn, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, net.ErrClosed
	}
	d.retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

	EnqueueTerminalCallbacks(root, d)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := LoadTasks(root)
		tk, _ := got.ByContext("c1")
		if tk.CallbackState == "failed" {
			if got := atomic.LoadInt32(&attempts); got != int32(len(d.retryDelays)+1) {
				t.Fatalf("attempts = %d, want %d", got, len(d.retryDelays)+1)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("callback never reached failed")
}

// round-12-review Important 1 的迴歸防線：舊版本每次送遞各建一個新
// http.Transport，零值 IdleConnTimeout 永不過期，goroutine 隨送遞次數累積
// （探針：30 次回呼 → +90 goroutine，兩秒 + 五次強制 GC 後仍是 +90）。這裡
// 走真正的 HTTP round trip（httptest 本機伺服器，不是真實網路），送 20 次
// 完成回呼，確認 goroutine 數量在送遞完成、連線關閉之後回到接近基準值，
// 不會隨著次數線性累積。
func TestCallbackDeliveryDoesNotLeakGoroutines(t *testing.T) {
	const n = 20
	var delivered int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&delivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	root := t.TempDir()
	var callers CallerStore
	_ = callers.Register("peer-a", "s")
	callers.Approve("peer-a", []string{"read"})
	callers.SetCallback("peer-a", "https://hook.example.com/x", "tok")
	_ = SaveCallers(root, callers)

	var s TaskStore
	for i := 0; i < n; i++ {
		s.Upsert(A2ATask{
			ContextID: fmt.Sprintf("c%d", i), TaskID: fmt.Sprintf("t%d", i), Agent: "a", CallerID: "peer-a",
			State: TaskCompleted, Detail: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	_ = SaveTasks(root, s)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewCallbackDispatcher(ctx, root)
	d.resolve = fixedResolver("93.184.216.34")
	d.dial = func(dctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(dctx, network, net.JoinHostPort(host, port))
	}
	d.scheme = "http"

	EnqueueTerminalCallbacks(root, d)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&delivered) < n {
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&delivered); got != n {
		t.Fatalf("delivered %d callbacks, want %d", got, n)
	}

	// 讓已經用完的連線有機會被關閉、goroutine 有機會退出;仿照 review 探針
	// 的手法連續強制幾次 GC。
	var after int
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
		after = runtime.NumGoroutine()
	}
	// dispatcher 自己的 run() goroutine 是預期多出來的一條;容許一點排程雜
	// 訊,但增量絕不可以隨 n 縮放 —— 這正是舊版本洩漏的樣子。
	if delta := after - baseline; delta > 5 {
		t.Fatalf("goroutine count grew by %d after %d deliveries (baseline=%d, after=%d) — want it to stay flat, not scale with delivery count", delta, n, baseline, after)
	}
}
