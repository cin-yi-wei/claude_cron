package channelagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Command struct {
	Name  string
	Args  []string
	Flags map[string]bool
	// Opts holds --key=value options (e.g. --platform=tg). Bare --flag tokens
	// still go to Flags as booleans.
	Opts map[string]string
}

// ParseCommand parses a control message. Returns ok=false for non-command text
// (anything not starting with "/"). Tokens of the form --key=value become Opts;
// bare --flag become Flags; everything else is a positional Arg.
func ParseCommand(content string) (Command, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "/") {
		return Command{}, false
	}
	fields := strings.Fields(content[1:])
	if len(fields) == 0 {
		return Command{}, false
	}
	cmd := Command{Name: fields[0], Flags: map[string]bool{}, Opts: map[string]string{}}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "--") {
			kv := strings.TrimPrefix(f, "--")
			if k, v, ok := strings.Cut(kv, "="); ok {
				cmd.Opts[k] = v
				continue
			}
			cmd.Flags[kv] = true
			continue
		}
		cmd.Args = append(cmd.Args, f)
	}
	return cmd, true
}

// opt returns the value of a --key=value option, or "" if absent. Safe on a nil
// Opts map (commands built without options).
func (c Command) opt(key string) string {
	if c.Opts == nil {
		return ""
	}
	return c.Opts[key]
}

type ControlDeps struct {
	Root           string
	GuildID        string
	CreateChannel  func(ctx context.Context, guildID, name string) (string, error)
	DeleteChannel  func(ctx context.Context, channelID string) error
	EnsureWorktree func(ctx context.Context, projectDir, branch, worktree string) error
	RemoveWorktree func(ctx context.Context, projectDir, worktree string) error
	StartSession   func(ctx context.Context, session, cwd string) error
	StopSession    func(ctx context.Context, session string) error
	InitRoot       func(root string) error
}

const controlUsage = "指令: /bind <name> <project-dir> <branch> [--platform=dc|tg] [--mode=poll|push] [--chat-id=<id> (tg)] | /unbind <name> [--delete-channel] | /list | /status <name> | /help"

// HandleCommand executes a parsed control command against the registry, using
// deps for side effects. Returns a reply to post to the control channel and
// whether the registry changed (caller persists it).
func HandleCommand(ctx context.Context, deps ControlDeps, reg *Registry, cmd Command) (string, bool, error) {
	switch cmd.Name {
	case "bind":
		return handleBind(ctx, deps, reg, cmd)
	case "unbind":
		return handleUnbind(ctx, deps, reg, cmd)
	case "list":
		return handleList(reg), false, nil
	case "status":
		return handleStatus(reg, cmd), false, nil
	case "help":
		return controlUsage, false, nil
	default:
		return "未知指令。" + controlUsage, false, nil
	}
}

func handleBind(ctx context.Context, deps ControlDeps, reg *Registry, cmd Command) (string, bool, error) {
	if len(cmd.Args) != 3 {
		return "用法: /bind <name> <project-dir> <branch>", false, nil
	}
	name, projectDir, branch := cmd.Args[0], cmd.Args[1], cmd.Args[2]
	if !ValidName(name) {
		return fmt.Sprintf("name %q 不合法 (只能用 a-z 0-9 -)", name), false, nil
	}
	if name == "control" {
		return `name "control" 為保留字 (控管助理專用)，請換別的名稱`, false, nil
	}
	if _, ok := reg.Get(name); ok {
		return fmt.Sprintf("binding %q 已存在", name), false, nil
	}
	if _, err := os.Stat(projectDir); err != nil {
		return fmt.Sprintf("project-dir %q 不存在", projectDir), false, nil
	}

	platform, perr := normalizePlatform(cmd.opt("platform"))
	if perr != nil {
		return perr.Error(), false, nil
	}
	mode, merr := normalizeMode(cmd.opt("mode"))
	if merr != nil {
		return merr.Error(), false, nil
	}

	b := BindingDefaults(deps.Root, name, projectDir, branch)
	b.Platform = platform
	b.Mode = mode

	// Provision the channel/chat. Discord auto-creates a channel; Telegram reuses
	// an existing chat, so the chat id must be supplied via --chat-id.
	var channelID string
	if platform == PlatformTelegram {
		channelID = cmd.opt("chat-id")
		if channelID == "" {
			return "telegram 綁定需要 --chat-id=<chat id>", false, nil
		}
	} else {
		var err error
		channelID, err = deps.CreateChannel(ctx, deps.GuildID, name)
		if err != nil {
			return "", false, fmt.Errorf("建頻道失敗: %w", err)
		}
	}
	b.ChannelID = channelID

	// On failure, only tear down a channel we created. A Telegram chat is the
	// user's, never ours to delete.
	cleanupChannel := func() {
		if platform == PlatformDiscord {
			_ = deps.DeleteChannel(ctx, channelID)
		}
	}

	if err := deps.EnsureWorktree(ctx, projectDir, branch, b.Worktree); err != nil {
		_ = deps.RemoveWorktree(ctx, projectDir, b.Worktree)
		cleanupChannel()
		return "", false, fmt.Errorf("建 worktree 失敗: %w", err)
	}
	if err := deps.InitRoot(b.Root); err != nil {
		_ = deps.RemoveWorktree(ctx, projectDir, b.Worktree)
		cleanupChannel()
		return "", false, fmt.Errorf("init root 失敗: %w", err)
	}
	if err := deps.StartSession(ctx, b.TmuxSession, b.Worktree); err != nil {
		_ = deps.RemoveWorktree(ctx, projectDir, b.Worktree)
		cleanupChannel()
		return "", false, fmt.Errorf("啟 session 失敗: %w", err)
	}

	if err := reg.Add(b); err != nil {
		_ = deps.StopSession(ctx, b.TmuxSession)
		_ = deps.RemoveWorktree(ctx, projectDir, b.Worktree)
		cleanupChannel()
		return "", false, err
	}
	return fmt.Sprintf("✅ 綁定 %s [%s/%s] → channel %s (branch %s, session %s)", name, b.PlatformOf(), b.ModeOf(), channelID, branch, b.TmuxSession), true, nil
}

// normalizePlatform maps user input (incl. dc/tg aliases) to a canonical
// platform. Empty defaults to discord.
func normalizePlatform(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "dc", "discord":
		return PlatformDiscord, nil
	case "tg", "telegram":
		return PlatformTelegram, nil
	default:
		return "", fmt.Errorf("platform %q 不合法 (用 discord|dc 或 telegram|tg)", s)
	}
}

// normalizeMode maps user input to a canonical mode. Empty defaults to poll.
func normalizeMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "poll", "passive":
		return ModePoll, nil
	case "push", "active":
		return ModePush, nil
	default:
		return "", fmt.Errorf("mode %q 不合法 (用 poll|passive 或 push|active)", s)
	}
}

func handleUnbind(ctx context.Context, deps ControlDeps, reg *Registry, cmd Command) (string, bool, error) {
	if len(cmd.Args) != 1 {
		return "用法: /unbind <name> [--delete-channel]", false, nil
	}
	name := cmd.Args[0]
	b, ok := reg.Get(name)
	if !ok {
		return fmt.Sprintf("找不到 binding %q", name), false, nil
	}
	_ = deps.StopSession(ctx, b.TmuxSession)
	_ = deps.RemoveWorktree(ctx, b.ProjectDir, b.Worktree)
	if cmd.Flags["delete-channel"] {
		_ = deps.DeleteChannel(ctx, b.ChannelID)
	}
	_ = os.RemoveAll(b.Root)
	reg.Remove(name)
	return fmt.Sprintf("🗑️ 解綁 %s 完成", name), true, nil
}

func handleList(reg *Registry) string {
	if len(reg.Bindings) == 0 {
		return "(無綁定)"
	}
	var sb strings.Builder
	for _, b := range reg.Bindings {
		fmt.Fprintf(&sb, "• %s [%s/%s] → channel %s | branch %s | session %s\n", b.Name, b.PlatformOf(), b.ModeOf(), b.ChannelID, b.Branch, b.TmuxSession)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func handleStatus(reg *Registry, cmd Command) string {
	if len(cmd.Args) != 1 {
		return "用法: /status <name>"
	}
	b, ok := reg.Get(cmd.Args[0])
	if !ok {
		return fmt.Sprintf("找不到 binding %q", cmd.Args[0])
	}
	pending := countJSON(pathIn(b.Root, "inbox", "pending"))
	processing := countJSON(pathIn(b.Root, "inbox", "processing"))
	failed := countJSON(pathIn(b.Root, "inbox", "failed"))
	return fmt.Sprintf("%s: session %s | pending=%d processing=%d failed=%d", b.Name, b.TmuxSession, pending, processing, failed)
}

func countJSON(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// ControlBinding returns the reserved binding describing the control channel's
// own AI assistant session. It is not stored in the registry. The Worktree
// field is reused as the session's working directory (a plain sandbox dir, not
// a git worktree).
func ControlBinding(root string) Binding {
	return Binding{
		Name:        "control",
		TmuxSession: "cc-control",
		Root:        filepath.Join(root, "control"),
		Worktree:    filepath.Join(root, "control-workspace"),
	}
}

// controlSystemPrompt is appended to the cc-control Claude session so it knows
// its role, its workspace, and how to manage bindings.
func controlSystemPrompt(root, workspace string) string {
	return fmt.Sprintf(`你是 claude_cron 的控管助理，透過 Discord 控管頻道與使用者對話。
你的工作目錄（沙盒）是：%s
你可以在這裡執行 shell 指令、建立檔案/資料夾、回答關於這個系統的問題。

要管理「Discord 頻道 ↔ Claude session」綁定時，用以下 CLI（root 用絕對路徑 %s）：
  claude-cron bind <name> <project-dir> <branch> --root %s
  claude-cron unbind <name> [--delete-channel] --root %s
  claude-cron list --root %s

name 只能用小寫字母、數字、減號。回覆使用者時直接用一般文字即可。`,
		workspace, root, root, root, root)
}

// BuildControlDeps assembles a ControlDeps wired to the real Discord/worktree/
// tmux implementations. Shared by the supervisor and the management CLI so
// /bind and `claude-cron bind` behave identically.
func BuildControlDeps(root string, cfg Config) ControlDeps {
	token := os.Getenv(cfg.Discord.TokenEnv)
	admin := DiscordAdmin{BaseURL: cfg.Discord.BaseURL, Token: token}
	return ControlDeps{
		Root:           root,
		GuildID:        cfg.Discord.GuildID,
		CreateChannel:  admin.CreateChannel,
		DeleteChannel:  admin.DeleteChannel,
		EnsureWorktree: EnsureWorktree,
		RemoveWorktree: RemoveWorktree,
		StartSession:   StartTmuxClaude,
		StopSession:    StopTmuxSession,
		InitRoot:       Init,
	}
}

// RunControlOnce polls the control channel, executes any new commands, replies,
// persists the registry when it changed, and records processed message IDs so
// they are not handled twice. Dedup reuses the watcher's seen-state pattern
// under a control-specific state file.
func RunControlOnce(ctx context.Context, root, controlRoot string, deps ControlDeps, reg *Registry, source MessageSource, sender Sender) error {
	if err := Init(root); err != nil {
		return err
	}
	messages, err := source.Fetch(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].CreatedAt < messages[j].CreatedAt })

	statePath := pathIn(root, "state", "control_seen.json")
	state, err := readSeenState(statePath)
	if err != nil {
		return err
	}

	changed := false
	for _, m := range messages {
		key := seenKey(m)
		if state.MessageIDs[key] {
			continue
		}
		cmd, ok := ParseCommand(m.Content)
		if !ok {
			// Free text → hand to the control AI assistant via its job queue.
			if err := enqueueControlJob(controlRoot, m); err != nil {
				// leave unseen so it retries next poll
				continue
			}
			state.MessageIDs[key] = true
			continue
		}
		reply, regChanged, herr := HandleCommand(ctx, deps, reg, cmd)
		if herr != nil {
			// Leave unseen so a transient failure can be retried next poll.
			_ = sender.Send(ctx, OutputJob{Send: true, Text: "⚠️ " + herr.Error()})
			continue
		}
		state.MessageIDs[key] = true
		if regChanged {
			changed = true
		}
		if reply != "" {
			_ = sender.Send(ctx, OutputJob{Send: true, Text: reply})
		}
	}
	if changed {
		if err := SaveRegistry(root, *reg); err != nil {
			return err
		}
	}
	return AtomicWriteJSON(statePath, state)
}

// enqueueControlJob writes a free-text control message into the control
// binding's inbox so the cc-control assistant session processes it through the
// normal worker/sender pipeline. Mirrors the watcher's job construction.
func enqueueControlJob(controlRoot string, m SourceMessage) error {
	if err := Init(controlRoot); err != nil {
		return err
	}
	hash, err := HashSource(m)
	if err != nil {
		return err
	}
	job := InputJob{
		Schema:    1,
		JobID:     buildJobID(m, hash),
		RequestID: buildRequestID(m, hash),
		InputHash: hash,
		Source:    m,
		CreatedAt: m.CreatedAt,
	}
	return AtomicWriteJSON(pathIn(controlRoot, "inbox", "pending", job.JobID+".json"), job)
}
