package channelagent

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Agent is an A2A-exposed identity: aa-<Name>. Unlike a Binding it has no
// channel and never executes work itself — tasks run in per-contextId
// aa-<Name>-<ctx> instances.
type Agent struct {
	Name        string `json:"name"`
	ProjectDir  string `json:"project_dir"`
	Description string `json:"description"`
	// Capabilities 是**路由標籤**,不是沙盒權限。dispatch 當下要求呼叫方持有
	// 這裡宣告的每一項(宣告零項的 agent fail-closed),但它對沙盒實際能做
	// 什麼零影響 —— 那由任務的 GrantLevel 與 a2a_gate.go 決定。
	Capabilities []string `json:"capabilities"`
	Enabled      bool     `json:"enabled"`
	// ChannelID is this agent identity's output-only Discord channel. All of the
	// agent's concurrent tasks stream there so an operator can see whether work
	// is actually progressing. It is NEVER ingested: reading it would let anyone
	// who can type in Discord drive a sandbox, bypassing A2A auth entirely.
	ChannelID string `json:"channel_id,omitempty"`
}

type AgentStore struct {
	Agents []Agent `json:"agents"`
}

// a2aNameRe mirrors the binding name rule: lowercase letters, digits, dashes.
var a2aNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func AgentsPath(root string) string { return filepath.Join(root, "agents.json") }

// LoadAgents 讀 agents.json，並在回傳前套用兩條驗證。Add 已經驗證過名字，但
// Add 沒有正式呼叫端（agents.json 目前只能手寫），所以驗證必須在這一側。
//
//  1. 名字必須符合 a2aNameRe：含 '/' 或 '..' 的名字會流進 SessionNameFor →
//     SandboxRoot / SandboxWorktree 與 tmux session 名。
//  2. ChannelID 不得與任何 binding 的 channel 相同：那會讓 dcRoute
//     （supervisor.go）把該頻道的人類訊息吃進那個 cc- session，破壞「agent
//     頻道唯讀輸出」這個不變量。bindings.json 只讀不寫。
//
// 兩者都是「跳過並 log」而不是整份載入失敗：一個手寫錯誤不該讓所有 agent 消失。
func LoadAgents(root string) (AgentStore, error) {
	s, err := LoadAgentsRaw(root)
	if err != nil {
		return AgentStore{}, err
	}
	bindingChannels := map[string]string{}
	if reg, err := LoadRegistry(root); err == nil {
		for _, b := range reg.Bindings {
			if b.ChannelID != "" {
				bindingChannels[b.ChannelID] = b.Name
			}
		}
	}
	kept := s.Agents[:0]
	for _, a := range s.Agents {
		if !a2aNameRe.MatchString(a.Name) {
			log.Printf("a2a: 跳過 agent %q：名稱不合法（只允許小寫字母、數字、連字號）", a.Name)
			continue
		}
		if name, clash := bindingChannels[a.ChannelID]; a.ChannelID != "" && clash {
			log.Printf("a2a: 跳過 agent %q：它的 channel_id 與 binding %q 相同，那會讓該頻道的人類訊息被 ingest 進 cc- session", a.Name, name)
			continue
		}
		kept = append(kept, a)
	}
	s.Agents = kept
	return s, nil
}

// LoadAgentsRaw 讀 agents.json，不套用 LoadAgents 的任何驗證過濾。只給撤銷
// 偵測用（a2a_lifecycle.go 的 revokeReasonForRunningTask）：分辨一個 agent
// 名字是「agents.json 裡真的不存在」還是「存在，只是這次驗證沒通過」——兩者
// 對正在跑的任務後果不該一樣（round 10 review, Important，D10-5）。一般的
// 新派送路徑（Start、DrainQueue）必須繼續呼叫 LoadAgents，絕不能改用這個。
func LoadAgentsRaw(root string) (AgentStore, error) {
	var s AgentStore
	if err := ReadJSON(AgentsPath(root), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AgentStore{}, nil
		}
		return AgentStore{}, err
	}
	return s, nil
}

func SaveAgents(root string, s AgentStore) error {
	return AtomicWriteJSON(AgentsPath(root), s)
}

// agentsMu 是 agents.json 的對稱防護，理由與 callersMu / tasksMu 相同：
// admin API 的建立/停用/啟用/刪除都是「整檔讀 → 改 → 整檔寫」，沒有序列化
// 時併發請求會互相覆寫，30 個回了 201 的 agent 只剩下幾個真的留在檔案裡
// （round 14 review, Critical 2）。
//
// 同樣不可重入、鎖內不得有慢動作，也絕不與 session 鎖或 tasksMu 巢狀
// （DisableAgent 因此把 terminateTasks 留在 WithAgents 之外）。載入刻意用
// LoadAgentsRaw、不套用 LoadAgents 的名稱／channel_id 驗證過濾——理由見下
// 面 WithAgents 的說明，這裡不重複。
var agentsMu sync.Mutex

// WithAgents 在鎖內載入 agents.json、交給 fn 修改、再存檔。fn 回傳 error 時
// 完全不寫檔。fn 內絕不可以再呼叫 WithAgents。
//
// 讀的是 LoadAgentsRaw，不是 LoadAgents（round 2026-08-06 final review,
// Minor 4）：後者會濾掉名字不合法、或 channel_id 撞到某個 binding 的 agent
// （見 LoadAgents 的說明），但這裡是「整檔讀 → 改 → 整檔寫」的循環——用過
// 濾後的清單當底稿，等於把過濾結果直接存檔覆蓋掉原檔,任何一次跟這些壞
// entry 完全無關的 admin 操作（例如新增另一個 agent）都會把它們從
// agents.json 裡永久抹掉，操作者從沒要求刪除它們。這同時也讓
// LoadAgentsRaw 特地留給 revokeReasonForRunningTask 用的「typo 豁免」形同
// 虛設——那個豁免的前提正是「壞掉的 entry 還留在檔案裡」，如果它已經被這
// 裡的讀-改-寫循環悄悄刪掉，豁免就沒有東西可以豁免。fn 看到的是完整、未
// 過濾的清單，跟過去用 LoadAgents 相比，唯一差別是壞 entry 現在也在 fn 可
// 以 Get/replace/Remove 的範圍內——這正是讓 operator 有辦法用 API 修好或
// 刪掉它們，而不是被過濾規則永久攔在外面、只能手改 agents.json：Enable／
// Disable／Update 三條路由改欄位一律呼叫 replace（見該函式），不再像
// Remove 再 Add 那樣、每次都對著一個名字從沒被要求改過的既有 entry 重跑
// Add 的名稱格式檢查（2026-08-06 followup review——那個重跑會讓 Update／
// Enable 回 500、DisableAgent／DELETE 回 400，剛好把這裡承諾的「修好或
// 刪掉」擋在門外，只留下「手改 agents.json」這條退路，跟本段開頭想避免的
// 事一樣）；DELETE 最終呼叫的 Remove 本身從不驗證格式，一路都能刪掉一個
// 名字不合法的 entry。一般的新派送路徑（Start、DrainQueue）完全不受影響：
// 它們繼續呼叫 LoadAgents，過濾規則對「要不要接受新派送」的效力不變。
func WithAgents(root string, fn func(*AgentStore) error) error {
	agentsMu.Lock()
	defer agentsMu.Unlock()

	s, err := LoadAgentsRaw(root)
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return SaveAgents(root, s)
}

func (s *AgentStore) Get(name string) (Agent, bool) {
	for _, a := range s.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

func (s *AgentStore) Add(a Agent) error {
	if !a2aNameRe.MatchString(a.Name) {
		return fmt.Errorf("invalid agent name %q: use lowercase letters, digits, dashes", a.Name)
	}
	if _, exists := s.Get(a.Name); exists {
		return fmt.Errorf("agent %q already exists", a.Name)
	}
	s.Agents = append(s.Agents, a)
	return nil
}

// replace 原地覆寫一個已知存在、且不改名的 entry：找到同名的那一列，直接
// 整列換掉。跟 Remove 再 Add 不同，這裡刻意不呼叫 Add——Add 的名字格式與
// 唯一性檢查是為「這是一個全新的名字」把關的，對「這個名字已經在清單裡、
// 呼叫方只是要換掉它其餘的欄位」這件事完全用不上，卻會擋下這件事：一個
// 名字不合法（例如含空白）的既有 entry，Enable／Disable／Update 三條路由
// 都是「讀到它 → 改欄位 → Remove → Add」，Add 每次都會重新驗證這個早就
// 存在、格式從沒被要求改過的名字，於是全部擋下——回報成 500（Update／
// Enable）或 400（Disable，DELETE 的第一步），不論這次操作想改的其實是
// Enabled 或 description 或 channel_id，通通打不到。見 WithAgents 上方對
// 「操作者可以修好或刪掉壞掉的 entry」這個保證的說明；replace 是讓那個保
// 證對「改欄位」這一半成立的關鍵——找不到就是呼叫方自己的邏輯錯誤（呼叫
// 前一定先 Get 確認過存在），直接讓它整段被 fn 的失敗吸收沒有意義，所以
// 用 no-op 表達：找不到就什麼都不做，讓上層自己的 Get 檢查去把關。
func (s *AgentStore) replace(a Agent) {
	for i := range s.Agents {
		if s.Agents[i].Name == a.Name {
			s.Agents[i] = a
			return
		}
	}
}

func (s *AgentStore) Remove(name string) bool {
	for i, a := range s.Agents {
		if a.Name == name {
			s.Agents = append(s.Agents[:i], s.Agents[i+1:]...)
			return true
		}
	}
	return false
}

// Enabled returns only agents that opted in to being advertised.
func (s AgentStore) Enabled() []Agent {
	var out []Agent
	for _, a := range s.Agents {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}
