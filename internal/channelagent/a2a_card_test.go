package channelagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildAgentCardOnlyIncludesEnabledAgents(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "codereview", Description: "reviews code", Capabilities: []string{"read"}, Enabled: true},
		{Name: "secret-client-work", Description: "private", Enabled: false},
	}}
	card := BuildAgentCard("https://example.test/a2a", s)
	if len(card.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(card.Skills))
	}
	if card.Skills[0].ID != "codereview" {
		t.Fatalf("skill ID = %q", card.Skills[0].ID)
	}
	blob, _ := json.Marshal(card)
	if strings.Contains(string(blob), "secret-client-work") {
		t.Fatal("disabled agent leaked into Agent Card")
	}
}

func TestBuildAgentCardNeverLeaksBindings(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{
			Name:         "codereview",
			ProjectDir:   "/home/conray/project/fatgame-jfg-4908",
			Description:  "reviews code",
			Capabilities: []string{"read"},
			Enabled:      true,
		},
		{Name: "dataseai-secret", Description: "private", Enabled: false},
	}}
	card := BuildAgentCard("https://example.test/a2a", s)
	blob, _ := json.Marshal(card)
	blobStr := string(blob)
	forbidden := []string{
		"cc-",
		"fatgame",
		"bindings.json",
		"/home/conray/project/fatgame-jfg-4908",
		"jfg-4908",
		"dataseai-secret",
	}
	for _, f := range forbidden {
		if strings.Contains(blobStr, f) {
			t.Fatalf("Agent Card leaked %q: %s", f, blobStr)
		}
	}
}

func TestBuildAgentCardMultipleAgentsAndNilCapabilities(t *testing.T) {
	s := AgentStore{Agents: []Agent{
		{Name: "alpha", Description: "first agent", Capabilities: []string{"read", "write"}, Enabled: true},
		{Name: "beta", Description: "second agent", Capabilities: nil, Enabled: true},
		{Name: "gamma", Description: "third agent", Capabilities: []string{"exec"}, Enabled: true},
		{Name: "omitted", Description: "not enabled", Enabled: false},
	}}
	card := BuildAgentCard("https://example.test/a2a", s)
	if len(card.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d: %#v", len(card.Skills), card.Skills)
	}
	wantOrder := []string{"alpha", "beta", "gamma"}
	for i, id := range wantOrder {
		if card.Skills[i].ID != id {
			t.Fatalf("skill[%d].ID = %q, want %q (order not deterministic): %#v", i, card.Skills[i].ID, id, card.Skills)
		}
	}

	blob, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blobStr := string(blob)
	if strings.Contains(blobStr, `"tags":null`) {
		t.Fatalf("nil Capabilities rendered as null tags instead of an empty array: %s", blobStr)
	}

	var decoded struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 2026-08-06 follow-up（Gap 2）之前，這裡斷言的是相反的行為（omitempty
	// 讓 nil Capabilities 整個省略 tags 這個 key）。A2A 0.2.0 規格裡
	// AgentSkill.tags 是必填欄位——即使一個 agent 沒有宣告任何 capabilities，
	// 也必須是空陣列，不能整個 key 都不見，否則這個 skill 對一支照規格驗證
	// 的用戶端來說就是格式不合法的 AgentSkill。
	tags, hasTags := decoded.Skills[1]["tags"]
	if !hasTags {
		t.Fatalf("beta skill (nil Capabilities) must still include a \"tags\" key (AgentSkill.tags is required by A2A 0.2.0's schema even when empty), got: %#v", decoded.Skills[1])
	}
	arr, ok := tags.([]any)
	if !ok || len(arr) != 0 {
		t.Fatalf("tags = %#v, want an empty array, not omitted or null", tags)
	}
}

// Gap 2（2026-08-06 follow-up）：卡片曾經宣稱 protocolVersion "0.2.0"，但缺了
// 規格要求的欄位，而且 message/send 的 params/result 形狀是自訂的扁平結構，
// 不是規格要求的巢狀 Message{kind,messageId,parts,role} / 結構化
// Task{id,status}——一支通用 A2A client 讀了這張卡不可能真的跟這台伺服器
// 互通。既然把整條 wire 改成規格形狀不是一個便宜的改動（牽動既有呼叫方的
// 回應格式、且這裡是一個 URL 背後多個 agent，跟規格「一卡一 agent」的模型
// 不同），誠實的做法是不再宣稱任何真實規格版號，並在卡片裡把差異寫清楚。
func TestBuildAgentCardDoesNotFalselyClaimProtocolConformance(t *testing.T) {
	card := BuildAgentCard("https://example.test/a2a", AgentStore{})
	if card.ProtocolVersion != "" {
		t.Fatalf("ProtocolVersion = %q, want empty: this server does not implement the official A2A wire protocol and must not claim a version number that promises it does", card.ProtocolVersion)
	}
	if card.Name == "" {
		t.Fatal("card missing Name")
	}
	// 兩個刻意保留的偏差都要能在卡片本身讀到，讀的人不必先去翻原始碼:
	// (1) 整體不相容於規格的 wire 形狀，(2) "dispatching" 這個規格沒有的
	// 狀態。
	if !strings.Contains(card.Description, "dispatching") {
		t.Fatalf("Description does not mention the non-standard \"dispatching\" task state: %q", card.Description)
	}
	lower := strings.ToLower(card.Description)
	if !strings.Contains(lower, "not") || !strings.Contains(lower, "conform") {
		t.Fatalf("Description does not disclose that this server is not A2A-wire-conformant: %q", card.Description)
	}
}

// Gap 2 也點名卡片「缺了規格要求的欄位」。這些欄位跟上面 protocolVersion
// 的誠實聲明彼此獨立——不管有沒有宣稱相容於規格，capabilities/
// defaultInputModes/defaultOutputModes/version 都是關於這台伺服器實際支援
// 什麼的、可以誠實填寫的事實，而且新增它們不改動 a2a_server.go 任何一條
// dispatch 邏輯。
func TestBuildAgentCardIncludesRequiredFields(t *testing.T) {
	card := BuildAgentCard("https://example.test/a2a", AgentStore{})
	if card.Version == "" {
		t.Fatal("card missing Version (A2A 0.2.0 requires it; it names this build, not protocol conformance)")
	}
	if len(card.DefaultInputModes) == 0 {
		t.Fatal("card missing DefaultInputModes")
	}
	if len(card.DefaultOutputModes) == 0 {
		t.Fatal("card missing DefaultOutputModes")
	}
	// 三項都老實回報 false：這裡沒有 SSE streaming、沒有 push notification、
	// 也沒有把狀態變更歷史開放查詢——不是隨便塞一個空物件充數。
	if card.Capabilities.Streaming || card.Capabilities.PushNotifications || card.Capabilities.StateTransitionHistory {
		t.Fatalf("Capabilities = %#v, want all false (none of these are actually implemented)", card.Capabilities)
	}

	blob, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"capabilities", "defaultInputModes", "defaultOutputModes", "version", "skills", "name", "description", "url"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("card JSON missing required A2A 0.2.0 AgentCard key %q: %s", key, blob)
		}
	}
}
