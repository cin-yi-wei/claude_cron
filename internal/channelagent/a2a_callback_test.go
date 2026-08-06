package channelagent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
