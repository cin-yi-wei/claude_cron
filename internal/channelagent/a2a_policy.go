package channelagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// GrantLevel 是沙盒的能力授權等級。呼叫方只能「選等級」，永遠不能自行組裝
// 規則集；等級的內容集中定義在本檔與 a2a_gate.go 的 sandboxDecision，要調整
// 授權範圍只會動到那幾處，不會散落在各個呼叫端。
type GrantLevel string

const (
	GrantReadOnly GrantLevel = "readonly"
	GrantDevelop  GrantLevel = "develop"
	GrantFull     GrantLevel = "full"
	// GrantRevoked 是內部狀態，不可授予。撤銷時把政策檔整個覆寫成這個值，
	// 於是還沒被殺掉的 in-flight 工具呼叫立刻開始被 gate 拒絕 —— 比等 tmux
	// session 真的死掉快得多。
	GrantRevoked GrantLevel = "revoked"
)

// grantRank 給三個可授予等級一個全序：readonly < develop < full。revoked 與
// 未知值一律回 0，於是它們永遠不可能「不小於」任何可授予等級。
func grantRank(l GrantLevel) int {
	switch l {
	case GrantReadOnly:
		return 1
	case GrantDevelop:
		return 2
	case GrantFull:
		return 3
	}
	return 0
}

// ValidGrantLevel 只對三個可授予等級回 true。
func ValidGrantLevel(l GrantLevel) bool { return grantRank(l) > 0 }

// MinGrantLevel 取兩者中較低的等級。任一方不是可授予等級時回 ""，呼叫端必須
// 把空字串視為「沒有有效等級」而拒絕，不得當成預設值填回去。
func MinGrantLevel(a, b GrantLevel) GrantLevel {
	if !ValidGrantLevel(a) || !ValidGrantLevel(b) {
		return ""
	}
	if grantRank(a) <= grantRank(b) {
		return a
	}
	return b
}

// SandboxPolicy 是一個沙盒 session 的執行期授權，由 SandboxExecutor.Start 在
// Sessions.Start 之前寫入 <root>/a2a-policies/<session>.json（0600）。
//
// 三個設計取捨，明確記錄：
//  1. 不從 tasks.json 讀：gate 是每次工具呼叫都被 spawn 的獨立行程，讀不到
//     tasksMu；而 tasks.json 是整檔 O(N)、N 由呼叫方決定。每個 session 一個
//     小檔是 O(1)，與 task store 的成長無關。
//  2. 不放在 <root>/sandboxes/<session>/ 底下：那是沙盒自己的 root，把政策
//     放在受限主體看得見的目錄裡是自找麻煩。放在 <root>/a2a-policies/ 讓它
//     落在「worktree 之外」這條固定規則的保護範圍內。
//  3. full 級沙盒的 Bash 仍能改寫它。這不構成提權（full 本來就等同 cc-），
//     但列為已知殘留。readonly / develop 改不到：它們的 Edit/Write 受範圍
//     規則擋下，Bash 受指令允許清單 + metacharacter 禁令擋下。
type SandboxPolicy struct {
	Session     string     `json:"session"`
	ContextID   string     `json:"context_id"`
	Agent       string     `json:"agent"`
	CallerID    string     `json:"caller_id"`
	Level       GrantLevel `json:"level"`
	Worktree    string     `json:"worktree"`     // 絕對路徑
	SandboxRoot string     `json:"sandbox_root"` // 絕對路徑
	WrittenAt   string     `json:"written_at"`
}

// sandboxSessionRe 限制 session 名可以出現在檔案路徑裡的字元。SessionNameFor
// 產生的是 "aa-" + agent + "-" + sanitize(contextID)，agent 名理論上受
// a2aNameRe 約束，但 LoadAgents 在 D10(b) 之前完全不驗證，所以政策檔這一層
// 自己再擋一次：含 '/' 或 '..' 的名字絕對不能拼進路徑。
var sandboxSessionRe = regexp.MustCompile(`^aa-[A-Za-z0-9-]+$`)

func PolicyDir(root string) string { return filepath.Join(root, "a2a-policies") }

// PolicyPath 回傳 session 的政策檔路徑，並在拼路徑前先驗證 session 名。
func PolicyPath(root, session string) (string, error) {
	if !sandboxSessionRe.MatchString(session) {
		return "", fmt.Errorf("invalid sandbox session name %q", session)
	}
	return filepath.Join(PolicyDir(root), session+".json"), nil
}

// WriteSandboxPolicy 寫入（或覆寫）一份政策。呼叫端必須把寫入失敗視為
// dispatch 失敗，不可以降級成「先開起來再說」—— 沒有政策檔的沙盒會被 gate
// 全面拒絕，那是安全的，但把它當成能跑的沙盒就不是。
func WriteSandboxPolicy(root string, p SandboxPolicy) error {
	path, err := PolicyPath(root, p.Session)
	if err != nil {
		return err
	}
	if p.WrittenAt == "" {
		p.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	}
	return AtomicWriteJSONMode(path, p, 0o600)
}

func LoadSandboxPolicy(root, session string) (SandboxPolicy, error) {
	path, err := PolicyPath(root, session)
	if err != nil {
		return SandboxPolicy{}, err
	}
	var p SandboxPolicy
	if err := ReadJSON(path, &p); err != nil {
		return SandboxPolicy{}, err
	}
	return p, nil
}

// RevokeSandboxPolicy 把政策整個覆寫成 revoked。刻意保留檔案而不是刪除：
// 刪掉會落到 gate 的 denied_no_policy，語意上分不出「還沒發」與「被撤銷」，
// gate log 也就查不出撤銷是否真的生效。政策檔不存在時視為已無沙盒，回 nil。
func RevokeSandboxPolicy(root, session string) error {
	path, err := PolicyPath(root, session)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	return AtomicWriteJSONMode(path, SandboxPolicy{
		Session:   session,
		Level:     GrantRevoked,
		WrittenAt: time.Now().UTC().Format(time.RFC3339),
	}, 0o600)
}

// RemoveSandboxPolicy 在 sweep 回收沙盒時刪除政策檔。清不掉只由呼叫端 log，
// 不影響回收判定（下一趟會重試）。
func RemoveSandboxPolicy(root, session string) error {
	path, err := PolicyPath(root, session)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SandboxSessionFromRegistryRoot 從 registryRoot 反推「這是不是一個沙盒」，
// 是的話一併回傳主 root（政策檔所在處）與 session 名。兩個條件必須同時成立，
// 缺一即視為非沙盒：父目錄名為 sandboxes，且自身以 aa- 開頭且字元合法。
//
// 用 registryRoot 而非 cwd 判別的理由：hi.CWD 是 Claude 自己回報的，沙盒內
// cd 到別處就會改變它；registryRoot 來自 tmux 環境變數 CC_REGISTRY_ROOT，
// 沙盒內的工具呼叫改不到 hook 行程的環境。
func SandboxSessionFromRegistryRoot(registryRoot string) (root, session string, ok bool) {
	clean := cleanAbs(registryRoot)
	parent := filepath.Dir(clean)
	if filepath.Base(parent) != "sandboxes" {
		return "", "", false
	}
	base := filepath.Base(clean)
	if !strings.HasPrefix(base, "aa-") || !sandboxSessionRe.MatchString(base) {
		return "", "", false
	}
	return filepath.Dir(parent), base, true
}
