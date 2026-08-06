package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// callbackQueueSize：佇列滿就直接標 dropped 並丟棄。callback 絕不可以卡住
	// 任務，也不可以讓 cycle 阻塞。
	callbackQueueSize = 256
	callbackTimeout   = 10 * time.Second
)

// callbackRetryDelays：最多 3 次重試，只對傳輸錯誤與 5xx、429 重試。2xx 視為
// 成功，其他 4xx 視為永久失敗立刻放棄。
var callbackRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

type callbackJob struct {
	contextID string
	url       string
	token     string
	ips       []net.IP
	body      []byte
}

// CallbackDispatcher 用一條專屬 goroutine 消費一個容量 callbackQueueSize 的
// channel。任何時候都不得在持有 tasksMu 時發 callback。
type CallbackDispatcher struct {
	root string
	ch   chan callbackJob
	done chan struct{}

	// inflight 記下這個行程已經入列過的 contextId，讓 EnqueueTerminalCallbacks
	// 不會每個 cycle 重送同一列。serve 重啟後這個 map 是空的，於是仍是 pending
	// 的 row 會被重送一次 —— at-least-once，callee 需對 taskId 冪等
	//（規格第六節開放問題 7）。
	mu       sync.Mutex
	inflight map[string]bool

	// 以下三個欄位存在只為了讓測試可以完全不碰真實 DNS 與真實網路。
	resolve     func(host string) ([]net.IP, error)
	dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	scheme      string
	retryDelays []time.Duration
}

func NewCallbackDispatcher(ctx context.Context, root string) *CallbackDispatcher {
	d := &CallbackDispatcher{
		root:     root,
		ch:       make(chan callbackJob, callbackQueueSize),
		done:     make(chan struct{}),
		inflight: map[string]bool{},
		resolve: func(host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		},
		scheme:      "https",
		retryDelays: callbackRetryDelays,
	}
	go d.run(ctx)
	return d
}

// Wait 阻塞到 sender goroutine 真的結束（ctx 結束後）。
func (d *CallbackDispatcher) Wait() { <-d.done }

// ValidateCallbackURL 檢查目的地。設定當下與觸發當下各做一次。
//
// resolve 是注入的解析器，讓測試用假 IP 而不做真實 DNS 查詢。
func ValidateCallbackURL(raw string, resolve func(host string) ([]net.IP, error)) ([]net.IP, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("callback url is unparseable: %w", err)
	}
	if u.Scheme != "https" {
		return nil, errors.New("callback url must use https")
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("callback url has no host")
	}
	low := strings.ToLower(host)
	if low == "localhost" || strings.HasSuffix(low, ".local") || strings.HasSuffix(low, ".internal") {
		return nil, fmt.Errorf("callback host %q is internal", host)
	}
	ips, err := resolve(host)
	if err != nil {
		return nil, fmt.Errorf("resolve callback host: %w", err)
	}
	if len(ips) == 0 {
		return nil, errors.New("callback host resolved to no addresses")
	}
	// 檢查**所有**回傳 IP：只檢查第一個等於讓對方用一個混了 loopback 的
	// 多筆 A 記錄繞過去。
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("callback host resolves to a non-public address %s", ip)
		}
	}
	return ips, nil
}

// isPublicIP 涵蓋 loopback（127/8、::1）、私有（10/8、172.16/12、192.168/16、
// fc00::/7）、link-local（169.254/16、fe80::/10）、multicast 與未指定位址。
func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

// EnqueueTerminalCallbacks 是 callback 的**唯一觸發點**：A2A cycle 在 collect /
// sweep 之後掃出「terminal 且 CallbackState 尚未 sent/failed/dropped」的 row，
// 入列並標 pending。
//
// 不在 CollectResults / SweepTimeouts / markFailed 三處各接一次 —— 那三處都在
// 鎖內，而發 callback 是網路 I/O。
func EnqueueTerminalCallbacks(root string, d *CallbackDispatcher) int {
	if d == nil {
		return 0
	}
	// 鎖外先讀 callers：解析目的地是慢工。
	callers, err := LoadCallers(root)
	if err != nil {
		log.Printf("a2a callback: load callers: %v", err)
		return 0
	}
	callbackFor := map[string]Caller{}
	for _, c := range callers.Callers {
		if c.CallbackURL != "" {
			callbackFor[c.CallerID] = c
		}
	}
	if len(callbackFor) == 0 {
		return 0
	}

	var candidates []A2ATask
	_ = WithTasks(root, func(tasks *TaskStore) error {
		changed := false
		for i := range tasks.Tasks {
			t := tasks.Tasks[i]
			if !isTerminal(t.State) {
				continue
			}
			if t.CallbackState != "" && t.CallbackState != "pending" {
				continue
			}
			if _, ok := callbackFor[t.CallerID]; !ok {
				continue
			}
			if d.claim(t.ContextID) {
				t.CallbackState = "pending"
				tasks.Tasks[i] = t
				candidates = append(candidates, t)
				changed = true
			}
		}
		if !changed {
			return errNothingSwept
		}
		return nil
	})

	queued := 0
	for _, t := range candidates {
		c := callbackFor[t.CallerID]
		ips, verr := ValidateCallbackURL(c.CallbackURL, d.resolve)
		if verr != nil {
			log.Printf("a2a callback: %s 的目的地驗證失敗，放棄: %v", t.CallerID, verr)
			d.mark(t.ContextID, "failed")
			continue
		}
		payload := taskSnapshotPayload(t)
		payload["event"] = "task.terminal"
		body, merr := json.Marshal(payload)
		if merr != nil {
			d.mark(t.ContextID, "failed")
			continue
		}
		select {
		case d.ch <- callbackJob{contextID: t.ContextID, url: c.CallbackURL, token: c.CallbackToken, ips: ips, body: body}:
			queued++
		default:
			// 佇列滿：直接丟棄。絕不阻塞 cycle。
			d.mark(t.ContextID, "dropped")
		}
	}
	return queued
}

// claim 回報這個 contextId 是否由本次呼叫取得投遞權。
func (d *CallbackDispatcher) claim(contextID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[contextID] {
		return false
	}
	d.inflight[contextID] = true
	return true
}

func (d *CallbackDispatcher) run(ctx context.Context) {
	defer close(d.done)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.ch:
			d.deliver(ctx, job)
		}
	}
}

func (d *CallbackDispatcher) deliver(ctx context.Context, job callbackJob) {
	for attempt := 0; ; attempt++ {
		status, err := d.postOnce(ctx, job)
		switch {
		case err == nil && status >= 200 && status < 300:
			d.mark(job.contextID, "sent")
			return
		case err == nil && status < 500 && status != http.StatusTooManyRequests:
			// 其他 4xx 是永久失敗：重送同一份 body 不會有不同結果。
			d.mark(job.contextID, "failed")
			return
		}
		if attempt >= len(d.retryDelays) {
			d.mark(job.contextID, "failed")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.retryDelays[attempt]):
		}
	}
}

func (d *CallbackDispatcher) postOnce(ctx context.Context, job callbackJob) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.url, bytes.NewReader(job.body))
	if err != nil {
		return 0, err
	}
	if d.scheme != "" && d.scheme != "https" {
		req.URL.Scheme = d.scheme // 測試專用；正式路徑永遠是 https
	}
	req.Header.Set("Content-Type", "application/json")
	if job.token != "" {
		req.Header.Set("X-A2A-Callback-Token", job.token)
	}
	resp, err := d.client(job.ips).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// client 建一個只連「已經檢查過的 IP」的 http.Client。
//
// DNS rebinding 防護：解析一次、檢查所有回傳 IP、然後用自訂 DialContext 直接
// 連那個已檢查過的 IP，Host header 保留原主機名（req.URL 不動就會自然保留）。
// 不可以用會重新解析的 http.Get —— 那正是 rebinding 攻擊的入口。
// CheckRedirect 回 ErrUseLastResponse：不跟隨任何轉址，否則 302 到
// 169.254.169.254 就繞過了上面所有檢查。
func (d *CallbackDispatcher) client(ips []net.IP) *http.Client {
	dial := d.dial
	if dial == nil {
		base := &net.Dialer{Timeout: 5 * time.Second}
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return base.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		}
	}
	return &http.Client{
		Timeout:       callbackTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DialContext: dial},
	}
}

// mark 只寫 CallbackState。任務狀態機永遠不看這個欄位，所以這裡的失敗不會、
// 也不可以影響任務本身。
func (d *CallbackDispatcher) mark(contextID, state string) {
	_ = WithTasks(d.root, func(tasks *TaskStore) error {
		t, ok := tasks.ByContext(contextID)
		if !ok {
			return errNothingSwept
		}
		t.CallbackState = state
		tasks.Upsert(t)
		return nil
	})
	d.mu.Lock()
	delete(d.inflight, contextID)
	d.mu.Unlock()
}
