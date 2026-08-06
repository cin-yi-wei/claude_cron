package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	agent "claude_cron/internal/channelagent"
)

// runA2ACommand 實作 `claude-cron a2a <group> <verb> …`。旗標解析風格比照
// runManageCommand（main.go:457）：--key=value 進 opts，裸 --flag 進 flags，
// 其餘是位置參數。
//
// 預設一律走 admin API。CLI 是另一個行程，直接寫 tasks.json / agents.json /
// callers.json 會打破 a2a_store.go:10 那句「Only serve writes tasks.json, so
// an in-process mutex is sufficient」—— 那個 in-process mutex 是整個 A2A 併發
// 正確性的基礎。--offline 才直接改檔，且必須先探 /api/healthz，探得到就拒絕。
func runA2ACommand(rest []string, stdout, stderr io.Writer) int {
	root := ".channel-agent"
	opts := map[string]string{}
	flags := map[string]bool{}
	var pos []string
	for i := 0; i < len(rest); i++ {
		switch {
		case rest[i] == "--root":
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--root requires a value")
				return 2
			}
			root = rest[i+1]
			i++
		case strings.HasPrefix(rest[i], "--"):
			kv := strings.TrimPrefix(rest[i], "--")
			if k, v, ok := strings.Cut(kv, "="); ok {
				opts[k] = v
			} else {
				flags[kv] = true
			}
		default:
			pos = append(pos, rest[i])
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if len(pos) < 1 {
		fmt.Fprintln(stderr, a2aUsage)
		return 2
	}

	cfg, err := agent.LoadConfig(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// 整個 A2A 表面都掛在這個 kill switch 底下（cfg.A2A.Enabled，預設
	// false）。關掉的時候，admin API 的 /api/a2a/* 一律回 404
	// （a2a_admin.go serveA2A），--offline 若照樣寫 agents.json /
	// callers.json，serve 完全不會讀它們 —— 那是「默默改一個沒人管的檔案」，
	// 不是「拒絕並說明」。兩種模式都在這裡統一擋下。
	if !cfg.A2A.Enabled {
		fmt.Fprintln(stderr, "a2a is disabled (a2a.enabled=false in config.json); refusing to manage agents, callers, tasks, or audit until it is turned on")
		return 1
	}
	// 注意：不在此統一要求 len(pos) >= 2 —— `a2a audit` 是唯一一個沒有第二個
	// 位置參數（動詞）的頂層命令，缺動詞的其餘命令交給下面兩個 run 函式的
	// switch default 去印 usage。

	base := "http://" + cfg.Admin.Listen
	if flags["offline"] {
		if adminReachable(base) {
			fmt.Fprintf(stderr, "拒絕執行：serve 正在 %s 上運行。所有 A2A 狀態的寫入必須經由 admin API 在 serve 行程內完成，否則會打破 tasks.json 的單寫者不變量。請拿掉 --offline。\n", cfg.Admin.Listen)
			return 1
		}
		return runA2AOffline(root, pos, opts, flags, stdout, stderr)
	}
	c := a2aClient{base: base, token: cfg.Admin.Token}
	return runA2AOnline(c, pos, opts, flags, stdout, stderr)
}

const a2aUsage = `用法：
  claude-cron a2a agent add <name> --project=<dir> [--description=…] [--capabilities=a,b] [--channel=<id>] [--enabled]
  claude-cron a2a agent list|remove <name>|enable <name>|disable <name>
  claude-cron a2a caller register <id> [--credential=…]
  claude-cron a2a caller list
  claude-cron a2a caller approve <id> --level=readonly|develop|full [--capabilities=a,b]
  claude-cron a2a caller revoke <id>
  claude-cron a2a caller set-level <id> --level=…
  claude-cron a2a caller set-callback <id> --url=https://… [--token=…]
  claude-cron a2a task list [--state=…]
  claude-cron a2a task cancel <contextId>
  claude-cron a2a audit [--limit=200]
共用旗標：--root=<dir> --offline`

// a2aClient 是 /api/a2a/* 的薄 HTTP 客戶端。
type a2aClient struct {
	base  string
	token string
}

func (c a2aClient) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return out, fmt.Errorf("%s %s → %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// adminReachable 探 /api/healthz（未認證端點）。
func adminReachable(base string) bool {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(base + "/api/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// a2aGroupVerbArg 把位置參數拆成 group、verb、arg，三者都容許缺席（`a2a
// audit` 只有 group，沒有 verb 也沒有 arg）。呼叫端用 group+" "+verb 對應
// switch case；verb 缺席時就是空字串，剛好對得上 `case "audit ":` 那個刻意
// 留了尾端空白的 case label。
func a2aGroupVerbArg(pos []string) (group, verb, arg string) {
	if len(pos) > 0 {
		group = pos[0]
	}
	if len(pos) > 1 {
		verb = pos[1]
	}
	if len(pos) > 2 {
		arg = pos[2]
	}
	return
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runA2AOnline(c a2aClient, pos []string, opts map[string]string, flags map[string]bool, stdout, stderr io.Writer) int {
	group, verb, arg := a2aGroupVerbArg(pos)
	var (
		out []byte
		err error
	)
	switch group + " " + verb {
	case "agent add":
		out, err = c.do(http.MethodPost, "/api/a2a/agents", map[string]any{
			"name": arg, "project_dir": opts["project"], "description": opts["description"],
			"capabilities": splitCSV(opts["capabilities"]), "channel_id": opts["channel"],
			"enabled": flags["enabled"],
		})
	case "agent list":
		out, err = c.do(http.MethodGet, "/api/a2a/agents", nil)
	case "agent remove":
		out, err = c.do(http.MethodDelete, "/api/a2a/agents/"+arg, nil)
	case "agent enable":
		out, err = c.do(http.MethodPost, "/api/a2a/agents/"+arg+"/enable", nil)
	case "agent disable":
		out, err = c.do(http.MethodPost, "/api/a2a/agents/"+arg+"/disable", nil)
	case "caller register":
		out, err = c.do(http.MethodPost, "/api/a2a/callers", map[string]any{
			"caller_id": arg, "credential": opts["credential"],
		})
		if err == nil {
			fmt.Fprintln(stdout, "憑證只會顯示這一次，請立即保存：")
		}
	case "caller list":
		out, err = c.do(http.MethodGet, "/api/a2a/callers", nil)
	case "caller approve":
		out, err = c.do(http.MethodPost, "/api/a2a/callers/"+arg+"/approve", map[string]any{
			"capabilities": splitCSV(opts["capabilities"]), "level": opts["level"],
		})
	case "caller revoke":
		out, err = c.do(http.MethodPost, "/api/a2a/callers/"+arg+"/revoke", nil)
	case "caller set-level":
		out, err = c.do(http.MethodPost, "/api/a2a/callers/"+arg+"/level", map[string]any{"level": opts["level"]})
	case "caller set-callback":
		out, err = c.do(http.MethodPost, "/api/a2a/callers/"+arg+"/callback", map[string]any{
			"url": opts["url"], "token": opts["token"],
		})
	case "task list":
		out, err = c.do(http.MethodGet, "/api/a2a/tasks", nil)
		if err == nil && opts["state"] != "" {
			out = filterTasksByState(out, opts["state"])
		}
	case "task cancel":
		out, err = c.do(http.MethodPost, "/api/a2a/tasks/"+arg+"/cancel", nil)
	case "audit ":
		limit := opts["limit"]
		if limit == "" {
			limit = "200"
		}
		out, err = c.do(http.MethodGet, "/api/a2a/audit?limit="+limit, nil)
	default:
		fmt.Fprintln(stderr, a2aUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, strings.TrimSpace(string(out)))
	return 0
}

// filterTasksByState 在客戶端過濾，讓 admin API 不必多一個查詢參數。
func filterTasksByState(blob []byte, state string) []byte {
	var rows []map[string]any
	if json.Unmarshal(blob, &rows) != nil {
		return blob
	}
	kept := rows[:0]
	for _, r := range rows {
		if r["state"] == state {
			kept = append(kept, r)
		}
	}
	out, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return blob
	}
	return out
}

// runA2AOffline 直接改檔。只在 serve 沒在跑時可用（呼叫端已經探過
// /api/healthz）。刻意只支援不需要停 session 的動作：撤銷與取消必須在 serve
// 行程內完成，因為它們要停 driver goroutine 與 tmux session。
func runA2AOffline(root string, pos []string, opts map[string]string, flags map[string]bool, stdout, stderr io.Writer) int {
	group, verb, arg := a2aGroupVerbArg(pos)
	switch group + " " + verb {
	case "agent add":
		agents, err := agent.LoadAgents(root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := agents.Add(agent.Agent{
			Name: arg, ProjectDir: opts["project"], Description: opts["description"],
			Capabilities: splitCSV(opts["capabilities"]), ChannelID: opts["channel"],
			// 比對 online 路徑：enabled 是裸旗標，預設關閉，不是
			// opts["enabled"]（那只會在寫成 --enabled=xxx 時填入，裸
			// --enabled 永遠進 flags，兩條路徑不一致會讓同一條指令離線/連線
			// 產生不同結果）。
			Enabled: flags["enabled"],
		}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := agent.SaveAgents(root, agents); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "agent %s added (offline)\n", arg)
		return 0
	case "agent list":
		agents, err := agent.LoadAgents(root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		blob, _ := json.MarshalIndent(agents.Agents, "", "  ")
		fmt.Fprintln(stdout, string(blob))
		return 0
	case "agent remove":
		agents, err := agent.LoadAgents(root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !agents.Remove(arg) {
			fmt.Fprintf(stderr, "unknown agent %q\n", arg)
			return 1
		}
		if err := agent.SaveAgents(root, agents); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "agent %s removed (offline)\n", arg)
		return 0
	case "caller register":
		callers, err := agent.LoadCallers(root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		cred := opts["credential"]
		if cred == "" {
			buf := make([]byte, 32)
			if _, rerr := rand.Read(buf); rerr != nil {
				fmt.Fprintln(stderr, rerr)
				return 1
			}
			cred = base64.RawURLEncoding.EncodeToString(buf)
		}
		if err := callers.Register(arg, cred); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := agent.SaveCallers(root, callers); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "憑證只會顯示這一次，請立即保存：\n%s\n", cred)
		return 0
	case "caller list":
		callers, err := agent.LoadCallers(root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, c := range callers.Callers {
			// 永遠不印 credential。
			fmt.Fprintf(stdout, "%s\tstatus=%s\tlevel=%s\tcaps=%s\n",
				c.CallerID, c.Status, c.EffectiveGrantLevel(), strings.Join(c.GrantedCapabilities, ","))
		}
		return 0
	case "caller approve":
		callers, err := agent.LoadCallers(root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		lvl := agent.GrantLevel(opts["level"])
		if !agent.ValidGrantLevel(lvl) {
			fmt.Fprintln(stderr, "--level must be readonly, develop or full")
			return 2
		}
		if !callers.Approve(arg, splitCSV(opts["capabilities"])) {
			fmt.Fprintf(stderr, "unknown caller %q\n", arg)
			return 1
		}
		callers.SetGrantLevel(arg, lvl)
		if err := agent.SaveCallers(root, callers); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "caller %s approved at %s (offline)\n", arg, lvl)
		return 0
	default:
		fmt.Fprintf(stderr, "%q 在 --offline 模式下不支援：此動作需要 serve 行程內才有的狀態或收尾動作（例如撤銷/取消要停掉 driver 與 tmux session），無法安全地離線完成。請移除 --offline，改走 admin API。\n", group+" "+verb)
		return 2
	}
}
