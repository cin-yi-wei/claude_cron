package channelagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxResumeTranscriptBytes caps the size of a transcript we will `--resume`. A
// session whose .jsonl grows past this (the control session hit ~120MB) fails to
// boot: `claude --resume` must replay the whole file into context before the
// session is live, so it never reaches the point where in-session compaction
// could help — it OOMs/stalls on load and the supervisor loop-kills it. Past the
// cap we archive the file and start fresh; durable context lives in the memory
// files (~/.claude/.../memory), not the verbatim transcript, so a fresh session
// still picks up where the last left off.
const maxResumeTranscriptBytes = 40 << 20 // 40 MiB

// archiveOversizedTranscript moves an oversized transcript out of the project dir
// into ~/.claude/projects/_archive/ so the next session boots fresh. The archive
// name is self-describing — <encoded-project-dir>__<session-id>__<stamp>.jsonl —
// so each file says which binding and session it came from and when it was
// retired; _archive is a single dir the user can back up wholesale. Best-effort:
// on any failure we still report the transcript as gone so we never resume the
// monster. Returns true if the file was over the cap (archived or not).
func archiveOversizedTranscript(home, projectDir, id string, size int64) bool {
	if size <= maxResumeTranscriptBytes {
		return false
	}
	src := filepath.Join(home, ".claude", "projects", projectDir, id+".jsonl")
	archiveDir := filepath.Join(home, ".claude", "projects", "_archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return true
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(archiveDir, fmt.Sprintf("%s__%s__%s.jsonl", projectDir, id, stamp))
	_ = os.Rename(src, dst)
	return true
}

// sessionBootDelay bounds how long waitSessionReady waits for a freshly-created
// tmux Claude session to finish booting. A blind fixed delay was too short on
// cold start: the first injected prompt raced the Claude TUI boot splash, the
// keystrokes dropped, and the job stalled until its 120s timeout. waitSessionReady
// returns as soon as the input prompt renders (usually a few seconds), so this is
// an upper bound, not a fixed cost. Generous because a slow upstream API pushes
// cold boot to ~24s+ (vs ~14s normal); polling is cheap so over-budgeting is
// safe. Set to 0 in tests to skip probing.
var sessionBootDelay = 90 * time.Second

// readyProbeSettle is the pause between successive readiness pane captures.
var readyProbeSettle = 500 * time.Millisecond

// readyStableWindow is how long the pane must stay UNCHANGED (after the prompt
// renders) before a freshly --resume'd session is declared ready — guards against
// injecting while a large transcript is still replaying into the pane.
var readyStableWindow = 2 * time.Second

// waitSessionReady blocks until a freshly-created tmux Claude session has finished
// booting and is rendering its input prompt. It detects this PURELY by reading
// the pane — it NEVER sends a keystroke. Sending any key (a sentinel probe, a
// C-c) before the boot splash clears interrupts Claude's startup and the session
// exits (status 1); on the create/probe path that recreates-and-dies every cycle
// (a death-loop), and it fires even with an empty inbox because it precedes any
// inject. The earlier sentinel-echo probe WAS that killer. No-op when
// sessionBootDelay <= 0 (tests).
func waitSessionReady(ctx context.Context, session string) {
	if sessionBootDelay <= 0 {
		return
	}
	start := time.Now()
	var last string
	var stableSince time.Time
	for time.Since(start) < sessionBootDelay {
		time.Sleep(readyProbeSettle)
		pane, err := runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", session)
		if err != nil || !sessionPaneReady(pane) {
			last, stableSince = "", time.Time{}
			continue
		}
		// Prompt has rendered — but a large `--resume` may still be REPLAYING the
		// transcript into the pane (the prompt shows while history streams in). If
		// we return here, the next-cycle inject types into a session still digesting
		// its resume and the turn glitches. So also require the pane to be STABLE
		// (unchanged for readyStableWindow) before declaring ready — a replaying
		// pane keeps changing, so it waits until the resume settles.
		if pane != last {
			last, stableSince = pane, time.Now()
			continue
		}
		if !stableSince.IsZero() && time.Since(stableSince) >= readyStableWindow {
			return
		}
	}
}

// sessionPaneReady reports whether a Claude TUI pane snapshot shows the input
// prompt has rendered (boot complete). Read-only — used by waitSessionReady to
// gate injection without ever touching the keyboard.
func sessionPaneReady(pane string) bool {
	s := stripANSI(pane)
	return lastPromptLineSeen(s) || strings.Contains(strings.ToLower(s), "? for shortcuts")
}

// agentSettings is the Claude Code permission config for a WORKER binding's
// worktree. Read is auto-allowed. Edit/Write/Bash/WebFetch/WebSearch/MCP all
// route through the permission-gate — but the gate itself auto-allows Edit/Write
// that stay inside the binding's own worktree and ordinary (non-risky) Bash, so
// in practice only Edit/Write reaching outside the worktree, risky Bash, and
// WebFetch/WebSearch/MCP actually ask the channel (a tmux-driven session can't
// answer Claude's own interactive prompt, so everything not auto-allowed must go
// through the gate).
const agentSettings = `{
  "model": "opus",
  "permissions": {
    "allow": ["Read"]
  },
  "enabledPlugins": {
    "ruby-lsp@claude-plugins-official": false
  },
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "claude-cron session-hook" } ] }
    ],
    "PreToolUse": [
      { "matcher": "Edit", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "Write", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "WebFetch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "WebSearch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "mcp__.*", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] }
    ]
  }
}
`

// controlAgentSettings is the permission config for a CONTROL session. Same as a
// worker BUT Bash is auto-allowed outright (the control assistant runs
// management/deploy shell freely — gating it would prompt on every git/curl/
// sudo). Edit/Write still route through the gate, which auto-allows them inside
// the control session's own worktree and only asks the channel for edits that
// reach outside it (e.g. ~/.claude/skills/). WebFetch/WebSearch/MCP still route
// through the gate too → the user approves in the channel.
const controlAgentSettings = `{
  "permissions": {
    "allow": ["Read", "Bash"]
  },
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "claude-cron session-hook" } ] }
    ],
    "PreToolUse": [
      { "matcher": "Edit", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "Write", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "WebFetch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "WebSearch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "mcp__.*", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] }
    ]
  }
}
`

// sandboxAgentSettings 是 aa- 沙盒 worktree 的 Claude Code 權限設定:與
// agentSettings 逐字相同,只刪掉 SessionStart 區塊,六條 PreToolUse matcher
// 一條不少。
//
// 為什麼要刪:帶 hooks 的 settings.local.json 會讓 Claude Code 開機時跳
// 「Managed settings require approval」閘。該畫面被 classifyScreen 判為
// ScreenLogin(screen.go:55 → paneAwaitingManagedSettings),而
// autoAnswerSandboxConfirm 只在 ScreenConfirm 時動作 —— 於是沒有任何東西會
// 答它,prompt 被 RunWorkerOnce 打進核准畫面(paneBusy 不把 ScreenLogin 算成
// 忙碌)、Inject 回報成功、job 移進 done、prompt 消失,任務停在 working 兩
// 小時後被 sweep 判成 canceled。沙盒不需要 SessionStart(那個 hook 只記錄
// transcript 路徑給 cc- 的 supervisor 用),所以直接不裝是最乾淨的做法。
//
// agentSettings 與 EnsureAgentSettings 一個字都不能改:cc- 的行為必須逐位元
// 不變。
const sandboxAgentSettings = `{
  "model": "opus",
  "permissions": {
    "allow": ["Read"]
  },
  "enabledPlugins": {
    "ruby-lsp@claude-plugins-official": false
  },
  "hooks": {
    "PreToolUse": [
      { "matcher": "Edit", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "Write", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "WebFetch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "WebSearch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] },
      { "matcher": "mcp__.*", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=1800s", "timeout": 1860 } ] }
    ]
  }
}
`

// stripSessionStartHook 結構性地檢查既有 settings.local.json 是不是帶著
// hooks.SessionStart——不是對整份檔案做字串掃描(那樣任何只是「提到」這個
// 字的檔案,例如放在別的欄位裡當註解或範例值,都會被誤判成該重寫)。有的話
// 只刪掉那一個 key,其餘內容(包含使用者或別的流程手動加上的任何 key,例如
// permissions 之外自訂的規則)原封不動保留,回傳重新編碼後的完整內容與
// changed=true;呼叫端據此決定要不要真的落地。沒有 hooks 或沒有
// hooks.SessionStart 都回傳 changed=false。existing 若不是合法 JSON,回傳
// error——沒辦法結構性確認的檔案不能亂猜著重寫。
func stripSessionStartHook(existing []byte) (rewritten []byte, changed bool, err error) {
	var cfg map[string]any
	if err := json.Unmarshal(existing, &cfg); err != nil {
		return nil, false, err
	}
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	if _, has := hooks["SessionStart"]; !has {
		return nil, false, nil
	}
	delete(hooks, "SessionStart")
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}

// EnsureSandboxSettings 把 aa- 沙盒的權限設定寫進 dir。與 EnsureAgentSettings/
// EnsureControlSettings 不同,既有檔案不是永遠不動:如果既有 settings.local.json
// 結構上帶著 hooks.SessionStart,代表這個 worktree 是升級前的舊二進位留下的
// (那正是這個 task 要拔掉的閘的來源),必須把那一個 key 拔掉,否則這個沙盒
// 開機時還是會卡在同一個 managed-settings 閘上,升級等於沒修。拔除是針對性
// 的(只刪 hooks.SessionStart,見 stripSessionStartHook),不是整份蓋掉
// sandboxAgentSettings——這樣才不會把使用者或別的流程手動加在同一份檔案裡
// 的其他 key 一起沖掉。落地走 AtomicWriteFile(跟 EnsureFolderTrusted 一樣),
// 不是直接 os.WriteFile:一個正在跑的 claude 行程可能同時在讀這份設定檔,
// 直接寫會讓它讀到寫一半的截斷內容。不帶 SessionStart 的既有檔案(已經是
// 沙盒版本、或不相關的自訂內容)照舊不動。writeAgentSettings/
// EnsureAgentSettings 的「已存在則不動」行為完全不受影響。
func EnsureSandboxSettings(dir string) error {
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	existing, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		rewritten, changed, perr := stripSessionStartHook(existing)
		if perr != nil {
			return fmt.Errorf("parse existing sandbox settings %s: %w", settingsPath, perr)
		}
		if !changed {
			return nil // 已經是沙盒版本(或不相關的自訂內容),不動
		}
		mode := os.FileMode(0o644)
		if info, statErr := os.Stat(settingsPath); statErr == nil {
			mode = info.Mode()
		}
		return AtomicWriteFile(settingsPath, rewritten, mode)
	case os.IsNotExist(err):
		// 第一次開機,走跟 writeAgentSettings 一樣的建立路徑。
	default:
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, []byte(sandboxAgentSettings), 0o644)
}

// EnsureAgentSettings writes the WORKER permission config into dir's
// .claude/settings.local.json if absent (existing file left untouched).
func EnsureAgentSettings(dir string) error { return writeAgentSettings(dir, agentSettings) }

// EnsureControlSettings writes the CONTROL permission config (Bash auto-allowed,
// WebFetch/WebSearch/MCP gated) into dir if absent.
func EnsureControlSettings(dir string) error { return writeAgentSettings(dir, controlAgentSettings) }

func writeAgentSettings(dir, content string) error {
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if _, err := os.Stat(settingsPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, []byte(content), 0o644)
}

// gitIdentity supplies a fallback committer so commits work even when the host
// has no global git user configured. Real per-repo identity, if set, wins.
var gitIdentity = []string{"-c", "user.name=claude_cron", "-c", "user.email=claude_cron@localhost"}

// EnsureProjectRepo makes sure projectDir exists and is a git repo, so a binding
// can be created against a brand-new project. Existing repos are a no-op. A fresh
// project is created with `git init -b dev`, seeded with a README, and given one
// initial commit so a branch (and HEAD) exists for `git worktree add` to fork.
func EnsureProjectRepo(ctx context.Context, projectDir string) error {
	if runExternalCommand(ctx, "git", "-C", projectDir, "rev-parse", "--git-dir") == nil {
		return nil
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return err
	}
	if err := runExternalCommand(ctx, "git", "-C", projectDir, "init", "-b", "dev"); err != nil {
		return err
	}
	readme := filepath.Join(projectDir, "README.md")
	if _, err := os.Stat(readme); err != nil {
		if werr := os.WriteFile(readme, []byte("# "+filepath.Base(projectDir)+"\n"), 0o644); werr != nil {
			return werr
		}
	}
	if err := runExternalCommand(ctx, "git", "-C", projectDir, "add", "-A"); err != nil {
		return err
	}
	args := append([]string{"-c", "core.hooksPath=/dev/null"}, gitIdentity...)
	args = append(args, "-C", projectDir, "commit", "-m", "chore: init project (claude_cron)")
	return runExternalCommand(ctx, "git", args...)
}

// WipCommit commits any uncommitted changes in worktree onto its current branch
// before the worktree is removed on /unbind, so in-flight work is preserved on
// the branch (which lives in the shared main repo). No-op if the worktree is gone
// or has nothing to commit.
func WipCommit(ctx context.Context, worktree string) error {
	if _, err := os.Stat(worktree); err != nil {
		return nil
	}
	_ = runExternalCommand(ctx, "git", "-C", worktree, "add", "-A")
	// diff --cached --quiet exits 0 when nothing is staged → nothing to commit.
	if runExternalCommand(ctx, "git", "-C", worktree, "diff", "--cached", "--quiet") == nil {
		return nil
	}
	args := append([]string{"-c", "core.hooksPath=/dev/null"}, gitIdentity...)
	args = append(args, "-C", worktree, "commit", "-m", "wip: claude_cron unbind snapshot")
	return runExternalCommand(ctx, "git", args...)
}

// EnsureWorktree makes sure worktreePath is a git worktree of branch, checked
// out from projectDir. Idempotent: if worktreePath already exists it is a no-op.
// If the branch does not exist yet it is created from current HEAD.
func EnsureWorktree(ctx context.Context, projectDir, branch, worktreePath string) error {
	if _, err := os.Stat(worktreePath); err == nil {
		return nil
	}
	branchExists := runExternalCommand(ctx, "git", "-C", projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
	// `-c core.hooksPath=/dev/null` disables repo git hooks for this command. A
	// `worktree add` runs the post-checkout hook, which fails (non-zero exit) in
	// repos using hook frameworks like Overcommit when their tooling is not
	// installed — that failure should not block provisioning. worktreePath must
	// be absolute: git resolves a relative path against the -C directory, not the
	// caller's cwd, which would place the worktree inside the project repo.
	if branchExists {
		return runExternalCommand(ctx, "git", "-c", "core.hooksPath=/dev/null", "-C", projectDir, "worktree", "add", worktreePath, branch)
	}
	return runExternalCommand(ctx, "git", "-c", "core.hooksPath=/dev/null", "-C", projectDir, "worktree", "add", "-b", branch, worktreePath)
}

// RemoveWorktree removes a git worktree. Force is used so dirty worktrees are
// still cleaned up on /unbind.
func RemoveWorktree(ctx context.Context, projectDir, worktreePath string) error {
	err := runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "remove", "--force", worktreePath)
	// Prune any stale registration, then make sure the directory is actually
	// gone — `git worktree remove` can leave the dir/registration behind (busy
	// session, gitdir pointer issues), which used to orphan worktrees on unbind.
	_ = runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "prune")
	if _, statErr := os.Stat(worktreePath); statErr == nil {
		if rmErr := os.RemoveAll(worktreePath); rmErr != nil && err == nil {
			err = rmErr
		}
		_ = runExternalCommand(ctx, "git", "-C", projectDir, "worktree", "prune")
	}
	return err
}

// StartTmuxClaude 啟動一個 cc- binding 的 session。簽章與行為與改動前完全
// 相同(傳 EnsureAgentSettings)。
func StartTmuxClaude(ctx context.Context, session, cwd, registryRoot string) error {
	return startTmuxClaudeWith(ctx, session, cwd, registryRoot, EnsureAgentSettings)
}

// StartTmuxClaudeSandbox 啟動一個 aa- 沙盒的 session:唯一的差別是寫入不含
// SessionStart hook 的 settings(見 sandboxAgentSettings)。
func StartTmuxClaudeSandbox(ctx context.Context, session, cwd, registryRoot string) error {
	return startTmuxClaudeWith(ctx, session, cwd, registryRoot, EnsureSandboxSettings)
}

// startTmuxClaudeWith ensures a detached tmux session named session is
// running `claude` with its working directory set to cwd. No-op if it
// already exists. ensure writes whichever settings.local.json variant the
// caller needs (cc- worker vs aa- sandbox) before the session starts.
func startTmuxClaudeWith(ctx context.Context, session, cwd, registryRoot string, ensure func(string) error) error {
	if err := ensure(cwd); err != nil {
		return err
	}
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	// CC_REGISTRY_ROOT lets the PreToolUse permission-gate hook find the registry
	// (to resolve this worktree's binding + channel) without per-binding config.
	// claudeArgs resumes the latest transcript so a (re)created session — on
	// reap, serve restart, or reboot — keeps its prior conversation.
	base := []string{"new-session", "-d", "-s", session, "-c", cwd, "-e", "CC_REGISTRY_ROOT=" + registryRoot}
	base = append(base, oauthTokenEnvArgs()...)
	args := append(base, claudeArgs(cwd)...)
	if err := runExternalCommand(ctx, "tmux", args...); err != nil {
		return err
	}
	waitSessionReady(ctx, session)
	return nil
}

// oauthTokenEnvArgs passes a long-lived subscription token (from `claude
// setup-token`, set in serve's env via .env) into the session so it never needs
// an interactive /login. Empty when not configured. CLAUDE_CODE_OAUTH_TOKEN is a
// subscription OAuth token (NOT a pay-per-token API key), so billing stays on
// the plan; it is deliberately NOT stripped by claudeArgs.
func oauthTokenEnvArgs() []string {
	if t := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		return []string{"-e", "CLAUDE_CODE_OAUTH_TOKEN=" + t}
	}
	return nil
}

// encodeProjectDir maps an absolute path to Claude Code's project-history dir
// name: every non-alphanumeric character becomes '-'.
func encodeProjectDir(p string) string {
	var b strings.Builder
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// latestTranscript returns the id of the most recent Claude transcript for a
// session whose cwd is worktree, or "" if none.
func latestTranscript(worktree string) string {
	abs := worktree
	if a, err := filepath.Abs(worktree); err == nil {
		abs = a
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectDir := encodeProjectDir(abs)
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "projects", projectDir))
	if err != nil {
		return ""
	}
	var newest string
	var newestT time.Time
	var newestSize int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestT) {
			newestT = info.ModTime()
			newestSize = info.Size()
			newest = strings.TrimSuffix(e.Name(), ".jsonl")
		}
	}
	// Don't resume a transcript too big to boot — archive it and start fresh.
	if newest != "" && archiveOversizedTranscript(home, projectDir, newest, newestSize) {
		return ""
	}
	return newest
}

// claudeArgs builds the `claude ...` tail for a tmux launch, resuming the latest
// transcript for cwd when one exists. extra is appended after (e.g. flags).
func claudeArgs(cwd string, extra ...string) []string {
	// `env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN` strips inherited API
	// credentials so the session always authenticates with the interactive Claude
	// subscription (credentials.json), never pay-per-token API — even if a key
	// gets added to .env later. (pikiloom strips both vars for the same reason.)
	args := []string{"env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN", "claude"}
	if id := latestTranscript(cwd); id != "" {
		args = append(args, "--resume", id)
	}
	return append(args, extra...)
}

// StartControlSession starts the control channel's AI assistant session: a
// detached tmux session running `claude` with the given system prompt appended
// and the Discord bot token injected into the session environment (so the
// assistant's `claude-cron` management calls can authenticate). No-op if the
// session already exists. tokenEnv is the env var name, tokenValue its value.
func StartControlSession(ctx context.Context, session, cwd, registryRoot, tokenEnv, tokenValue, systemPrompt string) error {
	if err := EnsureControlSettings(cwd); err != nil {
		return err
	}
	if runExternalCommand(ctx, "tmux", "has-session", "-t", session) == nil {
		return nil
	}
	// CC_REGISTRY_ROOT lets the PreToolUse / SessionStart hooks find the registry
	// (the hooks have no flags) so permission-gate routing + session-hook work.
	base := []string{"new-session", "-d", "-s", session, "-c", cwd, "-e", "CC_REGISTRY_ROOT=" + registryRoot}
	if tokenEnv != "" {
		// A web control plane has no bot token; only inject -e when there is one.
		base = append(base, "-e", tokenEnv+"="+tokenValue)
	}
	base = append(base, oauthTokenEnvArgs()...)
	args := append(base, claudeArgs(cwd, "--append-system-prompt", systemPrompt)...)
	if err := runExternalCommand(ctx, "tmux", args...); err != nil {
		return err
	}
	waitSessionReady(ctx, session)
	return nil
}

// StopTmuxSession kills a tmux session. A missing session is not an error.
//
// 「tmux 真的跑起來、用離開碼回報了結果」一律視為成功：不論是砍掉了、還是根
// 本沒有這個 session（也包含 tmux server 沒起來），對呼叫方的結論相同——那個
// session 現在不在跑了。
//
// 但「我們根本沒問到答案」不可以再被吞成 nil（2026-08-06 minor triage，
// Fix 1b）。舊版本無條件 return nil，於是 fork EAGAIN（這台機器有 OOM 史）、
// tmux 執行檔暫時找不到、ctx 被取消這三種情況，全部長得跟「已經停掉了」一模
// 一樣；A2A 的回收路徑因此會在 session 其實還活著的時候刪掉它的 worktree 與
// sandbox root，留下一個 cwd 已被刪除、沒有任何 row 指得到的 claude 行程。
// 判定方式與 TmuxSessionAlive 完全一致（同樣的三分法），理由也一樣。
//
// cc- 的十個呼叫點（supervisor.go 四處、control.go 經 ControlDeps.StopSession
// 轉呼叫的四處加上接線本身、admin.go 一處）今天全部寫成
// `_ = StopTmuxSession(...)` / `_ = deps.StopSession(...)`，一律忽略回傳值，
// 所以這個修正對 cc- 沒有任何可觀察到的行為改變；只有 A2A 的拆除路徑會去看
// 它。（2026-08-06 followup review：先前這裡寫「四個呼叫點」，是還沒把
// control.go 經 ControlDeps.StopSession 間接呼叫的那四處算進去時的舊數字。）
func StopTmuxSession(ctx context.Context, session string) error {
	err := runExternalCommand(ctx, "tmux", "kill-session", "-t", session)
	if err == nil {
		return nil
	}
	// ctx 取消/逾時優先判斷：這時 cmd.Run() 回的錯誤型別不保證是 *exec.ExitError。
	if ctx.Err() != nil {
		return fmt.Errorf("stop tmux session %s: %w", session, ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil // tmux 回答了：沒有這個 session（或沒有 server）
	}
	return fmt.Errorf("stop tmux session %s: %w", session, err)
}
