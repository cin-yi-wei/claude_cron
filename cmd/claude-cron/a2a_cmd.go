package main

import (
	"bytes"
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
// 其餘是位置參數。--root 同時接受 `--root <dir>` 與 `--root=<dir>`，比照
// runBusyCommand（main.go:745）。
//
// 一律走 admin API，沒有第二種寫入路徑。CLI 是另一個行程，直接寫
// tasks.json / agents.json / callers.json 會打破 a2a_store.go:10 那句
// 「Only serve writes tasks.json, so an in-process mutex is sufficient」——
// 那個 in-process mutex 是整個 A2A 併發正確性的基礎，Task 13 的 admin API
// 存在正是為了讓這些檔案不再被手動編輯或被第二個行程搶寫。這裡曾經有一個
// `--offline` 直寫模式，但它的唯一安全前提（「serve 沒在跑」）只能靠探測
// /api/healthz 這種有 TOCTOU 的方式檢查，且沒有跨行程鎖，會與 serve 的
// LoadCallers/SaveCallers 交錯而悄悄丟掉彼此的寫入（review 抓到的真實
// repro）；已整段移除，改成一律要求可連通的 admin API。
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
		case strings.HasPrefix(rest[i], "--root="):
			root = strings.TrimPrefix(rest[i], "--root=")
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
	// false）。關掉的時候 admin API 的 /api/a2a/* 一律回 404
	// （a2a_admin.go serveA2A），所以在這裡就先擋下並說明原因，而不是讓
	// 每個子命令各自去踩一次 404。
	if !cfg.A2A.Enabled {
		fmt.Fprintln(stderr, "a2a is disabled (a2a.enabled=false in config.json); refusing to manage agents, callers, tasks, or audit until it is turned on")
		return 1
	}
	if cfg.Admin.Listen == "" {
		fmt.Fprintln(stderr, "admin.listen is not configured; the a2a CLI has no other way to reach agents.json/callers.json/tasks.json and refuses to write them directly")
		return 1
	}
	// 注意：不要求 len(pos) >= 2 —— `a2a audit` 是唯一一個沒有第二個位置參數
	// （動詞）的頂層命令；缺動詞的其餘命令交給 runA2AOnline 的 switch
	// default 去印 usage。

	c := a2aClient{base: "http://" + cfg.Admin.Listen, token: cfg.Admin.Token}
	return runA2AOnline(c, pos, opts, flags, stdout, stderr)
}

const a2aUsage = `用法：
  claude-cron a2a agent add <name> --project=<dir> [--description=…] [--capabilities=a,b] [--channel=<id>] [--enabled]
  claude-cron a2a agent update <name> [--project=<dir>] [--description=…] [--capabilities=a,b] [--channel=<id>] [--name=<new-name>]
      （只改有帶的欄位；enabled 走 enable/disable，不走這裡。--name 一般會被拒絕
       ——name 是身分，要換名字得刪除重建——唯一例外是 <name> 本身格式不合法
       （含空白等）時，可以把它改成一個合法的新名字：這種 entry 從沒通過驗證、
       從沒派送過任何工作，改名不會孤兒化任何沙盒）
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
共用旗標：--root=<dir>（或 --root <dir>）。所有動作都經由 admin API 執行，需要
serve 正在跑且 admin.listen 已設定。`

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

// a2aGroupVerbArg 把位置參數拆成 group、verb、arg，三者都容許缺席（`a2a
// audit` 只有 group，沒有 verb 也沒有 arg）。呼叫端用 group+" "+verb 對應
// switch case；verb 缺席時就是空字串，剛好對得上 `case "audit ":` 那個刻意
// 留了尾端空白的 case label（這個空白只出現在內部的 switch key，從不寫進任何
// 使用者可見的輸出）。
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
	case "agent update":
		// 只送真的有出現的 --flag：admin API 的 /update 用 pointer 語意分辨
		// 「沒帶這個 key」跟「帶了空字串/空陣列」，才能只改一個欄位而不清空
		// 其餘欄位。--name 平時仍會被伺服器端拒絕（agent 的身分，一般得刪除
		// 重建），只有 <name> 本身格式不合法時才會被接受，見上面 a2aUsage。
		body := map[string]any{}
		if v, ok := opts["name"]; ok {
			body["name"] = v
		}
		if v, ok := opts["project"]; ok {
			body["project_dir"] = v
		}
		if v, ok := opts["description"]; ok {
			body["description"] = v
		}
		if v, ok := opts["capabilities"]; ok {
			body["capabilities"] = splitCSV(v)
		}
		if v, ok := opts["channel"]; ok {
			body["channel_id"] = v
		}
		out, err = c.do(http.MethodPost, "/api/a2a/agents/"+arg+"/update", body)
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
