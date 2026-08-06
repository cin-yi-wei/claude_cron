package channelagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeConfigPath 回傳 Claude Code 存放 per-project 狀態（含資料夾信任旗標）的設定檔路徑。
func ClaudeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// EnsureFolderTrusted 將 projectDir 標記為已信任，讓 sandbox session 不會卡在信任對話框上。
// 該對話框不受 PreToolUse 攔截，而 sandbox 又沒有頻道可以回答它，因此若不處理，
// session 會一直卡住直到 hard timeout 才結束。
//
// 實測（2026-08-06）證實信任是「逐目錄」記錄的，不是綁在 git common root：
// dataseai-dev、fatgame-jfg-4334 等都是既有 repo 的 git worktree，卻各自在
// ~/.claude.json 裡有獨立的 hasTrustDialogAccepted 項目，與其主專案的項目分開。
// 因此呼叫端必須對每一個 sandbox 自己的工作目錄各呼叫一次本函式，
// 而不能只信任 git root 一次就以為涵蓋所有由它切出來的 sandbox。
//
// 設定檔會被每一個執行中的 claude 行程共用，內容遠不只信任旗標而已，因此這裡
// 一律解成通用 map 再重新編碼，讓所有無關的 key 都完整保留——覆寫掉會弄壞這台機器上
// 其他不相關的 session。設定檔不存在時絕不會建立新檔：那代表環境本身有問題，
// 生出一個空殼設定檔只會把問題蓋住。
func EnsureFolderTrusted(configPath, projectDir string) error {
	info, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("stat claude config: %w", err)
	}
	blob, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read claude config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return fmt.Errorf("decode claude config: %w", err)
	}

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[projectDir].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[projectDir] = entry
	}
	if trusted, _ := entry["hasTrustDialogAccepted"].(bool); trusted {
		return nil // 已經信任過了，檔案原封不動
	}
	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 這支設定檔是這台機器上所有 claude 行程共用的活生生設定檔，中斷（OOM、被
	// kill）發生在寫到一半時絕不能留下截斷或無效 JSON 的檔案——那會讓機器上
	// 每一個正在跑的 claude 行程（含 production bindings 與 control session
	// 本身）全部壞掉。因此透過 AtomicWriteFile 寫到同目錄的暫存檔再 rename，
	// 檔案永遠只會是「舊內容」或「新內容」兩者之一。權限沿用原檔的 mode，
	// 不要硬編一個假的權限值誤導讀者以為這裡會收緊權限。
	return AtomicWriteFile(configPath, out, info.Mode())
}
