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
	// callbackResolveTimeout 限制單次 DNS 查詢的時間。net.DefaultResolver 沒
	// 加逾時預設不封頂——一台黑洞掉的 DNS 伺服器讓 Go 的重試機制（每個
	// nameserver 大約 5 秒 ×2 次）吃掉數十秒，而這段查詢目前同步發生在 A2A
	// cycle goroutine 上，會連帶拖慢同一輪的 collect/sweep/drain/prune
	//（round-12-review Important 2 的探針：3 筆各 300ms 的解析 = 903ms 的
	// cycle 停滯）。
	callbackResolveTimeout = 3 * time.Second
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

// callbackIPsContextKey 是 dialValidated 從 request context 讀出「這次呼叫
// 已經驗證過的 IP 清單」用的 key（Important 1 修正：Transport 現在是共用的
// 單一個，IP 不能再靠 client(ips) 的閉包帶,只能靠 context 逐次注入）。
type callbackIPsContextKey struct{}

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

	// transport / httpClient 是整個 dispatcher 生命週期共用的唯一一組——
	// Important 1（round-12-review）：舊版本在每次送遞時各建一個新
	// http.Transport，零值的 IdleConnTimeout 永不過期，連線與讀寫 goroutine
	// 隨送遞次數無界累積（探針：30 次回呼 → +90 goroutine，兩秒+五次強制 GC
	// 後仍是 +90）。改成一個共用 Transport，並用 DisableKeepAlives 讓每個連
	// 線用完立刻關閉，讀寫 goroutine 隨之結束，數量不會隨呼叫次數累積。
	transport  *http.Transport
	httpClient *http.Client

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
			// 逾時封頂：見 callbackResolveTimeout 的說明。
			rctx, cancel := context.WithTimeout(context.Background(), callbackResolveTimeout)
			defer cancel()
			return net.DefaultResolver.LookupIP(rctx, "ip", host)
		},
		scheme:      "https",
		retryDelays: callbackRetryDelays,
	}
	d.transport = &http.Transport{
		// DialContext 一律連「這次呼叫已經驗證過的 IP」，不重新解析 hostname
		// ——DNS-rebinding 防護的核心不變，只是把「每次送遞建一個 Transport」
		// 換成「一個共用 Transport，IP 隨 request context 逐次注入」
		//（dialValidated 從 ctx 讀 callbackIPsContextKey）。d.dial 是測試專
		// 用的整體覆寫，在呼叫時（不是建構時）動態讀取 d.dial，所以測試在
		// NewCallbackDispatcher 之後才設定 d.dial 仍然有效。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if d.dial != nil {
				return d.dial(ctx, network, addr)
			}
			return d.dialValidated(ctx, network, addr)
		},
		// 見上面 CallbackDispatcher.transport 的註解：停用 keep-alive 讓每
		// 個連線用完立刻關閉，避免讀寫 goroutine 隨送遞次數累積。
		DisableKeepAlives: true,
	}
	d.httpClient = &http.Client{
		Timeout: callbackTimeout,
		// 不跟隨任何轉址，否則 302 到 169.254.169.254 就繞過了驗證。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     d.transport,
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
	// TrimSuffix 一次尾點：一個完全合格網域名（FQDN）最多帶一個尾點
	// （"localhost."、"api.internal."），沒有它會繞過下面的黑名單比對
	// （round-12-review Minor 4）。
	low := strings.TrimSuffix(strings.ToLower(host), ".")
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

// cgnatBlock / ietfProtocolBlock / benchmarkBlock / reservedBlock /
// nat64Block 是 isPublicIP 額外要擋的範圍（round-12-review Minor 3）：
//   - 100.64.0.0/10：RFC 6598 CGNAT，同時也是 Tailscale 的預設位址範圍——
//     在這台主機上不是理論上的縫隙，是通到 operator 自己 tailnet 上的活路。
//   - 192.0.0.0/24：RFC 6890 IETF Protocol Assignments。
//   - 198.18.0.0/15：RFC 2544 Benchmark 測試範圍。
//   - 240.0.0.0/4：保留（前身 Class E）。
//   - 64:ff9b::/96：RFC 6052 NAT64 Well-Known Prefix——探針證實
//     `64:ff9b::7f00:1` 這種位址會被 NAT64 gateway 轉譯成 127.0.0.1，等於
//     用一層位址編碼繞過純 IPv4 loopback 檢查。整段前綴一律擋，不嘗試拆出
//     內嵌的 IPv4 再遞迴判斷——這裡的威脅模型是「主機安全」優先於「透過
//     NAT64 存取合法公網目的地」這種邊緣情境。
var (
	cgnatBlock        = mustCIDR("100.64.0.0/10")
	ietfProtocolBlock = mustCIDR("192.0.0.0/24")
	benchmarkBlock    = mustCIDR("198.18.0.0/15")
	reservedBlock     = mustCIDR("240.0.0.0/4")
	nat64Block        = mustCIDR("64:ff9b::/96")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isPublicIP 涵蓋 loopback（127/8、::1）、私有（10/8、172.16/12、192.168/16、
// fc00::/7）、link-local（169.254/16，含雲端 metadata 位址 169.254.169.254、
// fe80::/10）、multicast、未指定位址，加上 CGNAT/Tailscale（100.64/10）、
// IETF protocol assignments（192.0.0.0/24）、benchmark（198.18.0.0/15）、
// 保留位址（240.0.0.0/4）與 NAT64 well-known prefix（64:ff9b::/96）。
func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		cgnatBlock.Contains(ip) || ietfProtocolBlock.Contains(ip) ||
		benchmarkBlock.Contains(ip) || reservedBlock.Contains(ip) ||
		nat64Block.Contains(ip) {
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

	// 目的地驗證一次每個 caller，不是一次每個 row（round-12-review
	// Important 2）：同一個 caller 在同一輪 cycle 可能有好幾個任務同時進終
	// 態，DNS 解析是同步發生在 cycle goroutine 上的慢工，重複次數壓到「每
	// 個相異 caller 最多一次」。
	type validation struct {
		ips []net.IP
		err error
	}
	validated := map[string]validation{}

	queued := 0
	for _, t := range candidates {
		c := callbackFor[t.CallerID]
		v, ok := validated[t.CallerID]
		if !ok {
			ips, verr := ValidateCallbackURL(c.CallbackURL, d.resolve)
			v = validation{ips: ips, err: verr}
			validated[t.CallerID] = v
		}
		if v.err != nil {
			log.Printf("a2a callback: %s 的目的地驗證失敗，放棄: %v", t.CallerID, v.err)
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
		case d.ch <- callbackJob{contextID: t.ContextID, url: c.CallbackURL, token: c.CallbackToken, ips: v.ips, body: body}:
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
			// 其他 4xx（以及任何 3xx——轉址不跟隨，回應狀態碼原樣是重定向碼）
			// 是永久失敗：重送同一份 body 不會有不同結果。
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
	// 把這次呼叫驗證過的 IP 清單放進 context，讓共用 Transport 的
	// DialContext（dialValidated）連那個位址，不重新解析 hostname。
	ctx = context.WithValue(ctx, callbackIPsContextKey{}, job.ips)
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
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// dialValidated 是共用 Transport 的正式（非測試）DialContext：一律連
// ctx 裡帶著的、已經驗證過的 IP，不重新解析 hostname——這是 DNS-rebinding
// 防護的核心：解析一次、檢查所有回傳 IP，然後永遠連那個已檢查過的位址，
// Host header 保留原主機名（req.URL 不動就會自然保留）。
func (d *CallbackDispatcher) dialValidated(ctx context.Context, network, addr string) (net.Conn, error) {
	ips, _ := ctx.Value(callbackIPsContextKey{}).([]net.IP)
	if len(ips) == 0 {
		return nil, errors.New("a2a callback: dial context is missing a validated ip")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
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
