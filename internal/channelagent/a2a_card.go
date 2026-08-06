package channelagent

// AgentCardSkill advertises one agent. The ID is the agent name; a caller names
// it when submitting a task.
type AgentCardSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Tags 對應 A2A 0.2.0 規格裡 AgentSkill.tags——那是必填欄位，即使一個
	// agent 沒有宣告任何 capabilities，也必須是空陣列而不是整個欄位消失。
	// 故意不加 omitempty：那會在 Capabilities 是 nil 時把這個必填鍵整個丟
	// 掉，讓這個 skill 對一支照規格驗證的用戶端而言是格式不合法的
	// AgentSkill，連 discovery 階段都過不了（2026-08-06 follow-up, Gap 2）。
	// BuildAgentCard 因此在 Capabilities 是 nil 時明確填一個非 nil 的空
	// slice，而不是直接把 nil 指定過來。
	Tags []string `json:"tags"`
}

// AgentCapabilities 對應規格裡 AgentCard.capabilities——這裡老實回報三項
// 都不支援，不是隨便塞一個空物件充數：沒有 SSE streaming
// （message/send 一律同步回一次 JSON-RPC response，見 a2a_server.go），沒有
// push notification（沒有任何程式碼會主動打出站的 webhook——callers.json 的
// CallbackURL 是 operator 設定、a2a_callback.go 才會用到的東西，跟這裡指的
// 「A2A 標準 push notification 機制」不是同一件事），也沒有把狀態變更歷史
// 開放查詢（tasks/get 只回目前這一列的快照，見 a2a_rpc.go 的
// taskSnapshotPayload）。
type AgentCapabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

// a2aCardVersion 是這台伺服器自己的版本號，對應規格裡 AgentCard.version——
// 跟 ProtocolVersion 是兩個完全不同的東西：version 說的是「這個 agent（這個
// 部署）本身是第幾版」，由提供者自訂格式，不代表任何協定相容性宣告。這裡
// 目前沒有真正的建置版本追蹤機制，先用一個固定字串佔位；不宣稱、也不影響
// 下面 ProtocolVersion 的誠實聲明。
const a2aCardVersion = "0"

// AgentCard is the public discovery document served at /.well-known/agent.json.
// Only opted-in agents appear: binding names would leak project and client info.
//
// 誠實聲明（2026-08-06 follow-up, Gap 2）：這**不是**一份符合官方
// Agent2Agent（A2A）線路協定的卡片。ProtocolVersion 故意留空，不宣稱任何一
// 個真實規格版號——原本這裡寫的是 "0.2.0"，但比對過官方 0.2.0 規格的 JSON
// Schema（a2aproject/A2A repo, tag v0.2.0, specification/json/a2a.json）之
// 後,發現至少三個地方不符:
//
//  1. AgentCard 0.2.0 根本沒有 "protocolVersion" 這個欄位（那是 0.3.0 才加
//     的）；此外它缺了 0.2.0 真正必填的 capabilities / defaultInputModes /
//     defaultOutputModes / version（下面已經補上這四個欄位，這部分是便宜、
//     不影響 dispatch 行為的改動，值得做,也已經做了）。
//  2. message/send 的 params/result 是自訂的扁平結構
//     （MessageSendParams{Agent,ContextID,Text,TaskID,Level}），不是規格要
//     求的巢狀 Message{kind,messageId,parts,role}；tasks/get 回的
//     taskSnapshotPayload 也不是規格要求的結構化 Task{id,contextId,kind,
//     status}。把這兩個形狀改成規格要求的樣子不是「不改行為的小改動」——
//     會動到既有呼叫方已經在用的回應格式。
//  3. 這裡是一個 URL 背後同時服務多個 agent（靠請求裡的 "agent" 欄位選
//     路），跟規格「一張卡對一個 agent、一個 URL」的模型本質上不同,不是
//     欄位改名就能對齊的差異。
//
// (2) 跟 (3) 合起來代表要做到真正的線路相容不是一個便宜的改動——會牽動既
// 有回應格式，也牽動這台伺服器目前「一個 endpoint 服務多個 agent」的架構。
// 既然做不到便宜的真相容,誠實的做法就是不再宣稱任何版號,而不是留著一個
// 看起來合理但其實不成立的宣稱：一支照規格寫的通用 A2A client 讀了這張卡
// 不該以為可以直接互通、寫完整合程式碼才在真正呼叫時失敗——那比誠實承認
// 「不相容」更糟。
//
// 有一項偏差是刻意保留、不打算修的：TaskState 裡的 "dispatching"
// （沙盒還在建置、尚未進入 working）沒有標準對應，也沒有計畫拿掉——它是
// a2a_tasks.go CanTransition 狀態機的一部分，見那邊的說明。除此之外的
// TaskState 值（submitted/working/completed/canceled/failed）跟規格字串
// 一致；method 名稱 message/send、tasks/get 也跟規格一致。這兩件事都寫進
// 下面的 Description，讓真正要整合的人在寫程式碼之前就看到，而不是要翻原
// 始碼才知道。
type AgentCard struct {
	// 故意留空——理由見上面型別的完整說明。省略之後,任何要求這個欄位存在
	// 的 client SDK（例如針對 0.3.0 寫的）會在 discovery 階段就判定這張卡
	// 不合法而拒絕,這是比「深入呼叫到一半才失敗」更早、更乾脆的失敗點。
	ProtocolVersion    string            `json:"protocolVersion,omitempty"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	URL                string            `json:"url"`
	Version            string            `json:"version"`
	Capabilities       AgentCapabilities `json:"capabilities"`
	DefaultInputModes  []string          `json:"defaultInputModes"`
	DefaultOutputModes []string          `json:"defaultOutputModes"`
	Skills             []AgentCardSkill  `json:"skills"`
}

func BuildAgentCard(baseURL string, s AgentStore) AgentCard {
	card := AgentCard{
		ProtocolVersion: "",
		Name:            "claude_cron",
		Description: "Delegated task execution in isolated sandboxes. " +
			"This server does NOT conform to the official Agent2Agent (A2A) wire " +
			"protocol: message/send and tasks/get use a bespoke flat JSON-RPC " +
			"dialect (no Message.parts/kind, no structured Task.status), and one " +
			"URL multiplexes many agents via a request-level \"agent\" selector " +
			"instead of the spec's one-agent-per-card model. Most TaskState " +
			"values match the spec's strings, but \"dispatching\" (a sandbox " +
			"still building, before \"working\") is a deliberate addition with " +
			"no standard equivalent. Do not point an off-the-shelf A2A client at " +
			"this server and expect it to interoperate.",
		URL:     baseURL,
		Version: a2aCardVersion,
		Capabilities: AgentCapabilities{
			Streaming:              false,
			PushNotifications:      false,
			StateTransitionHistory: false,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []AgentCardSkill{},
	}
	for _, a := range s.Enabled() {
		tags := a.Capabilities
		if tags == nil {
			tags = []string{}
		}
		card.Skills = append(card.Skills, AgentCardSkill{
			ID:          a.Name,
			Name:        a.Name,
			Description: a.Description,
			Tags:        tags,
		})
	}
	return card
}
