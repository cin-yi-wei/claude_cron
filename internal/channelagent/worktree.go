package channelagent

import (
	"context"
	"fmt"
	"os"
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
	for time.Since(start) < sessionBootDelay {
		time.Sleep(readyProbeSettle)
		pane, err := runExternalCommandOutput(ctx, "tmux", "capture-pane", "-pt", session)
		if err == nil && sessionPaneReady(pane) {
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
// worktree. Read/Write/Edit are auto-allowed (read job, write reply); Bash,
// WebFetch, WebSearch and MCP route through the permission-gate so the user
// approves them in the channel (a tmux-driven session can't answer Claude's own
// interactive prompt, so everything not auto-allowed must go through the gate).
const agentSettings = `{
  "permissions": {
    "allow": ["Read", "Write", "Edit"]
  },
  "enabledPlugins": {
    "ruby-lsp@claude-plugins-official": false
  },
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "claude-cron session-hook" } ] }
    ],
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] },
      { "matcher": "WebFetch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] },
      { "matcher": "WebSearch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] },
      { "matcher": "mcp__.*", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] }
    ]
  }
}
`

// controlAgentSettings is the permission config for a CONTROL session. Same as a
// worker BUT Bash is auto-allowed (the control assistant runs management/deploy
// shell freely — gating it would prompt on every git/curl/sudo). WebFetch /
// WebSearch / MCP still route through the gate → the user approves in the channel.
const controlAgentSettings = `{
  "permissions": {
    "allow": ["Read", "Write", "Edit", "Bash"]
  },
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "claude-cron session-hook" } ] }
    ],
    "PreToolUse": [
      { "matcher": "WebFetch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] },
      { "matcher": "WebSearch", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] },
      { "matcher": "mcp__.*", "hooks": [ { "type": "command", "command": "claude-cron permission-gate --timeout=600s", "timeout": 660 } ] }
    ]
  }
}
`

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

// StartTmuxClaude ensures a detached tmux session named session is running
// `claude` with its working directory set to cwd. No-op if it already exists.
func StartTmuxClaude(ctx context.Context, session, cwd, registryRoot string) error {
	if err := EnsureAgentSettings(cwd); err != nil {
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
func StopTmuxSession(ctx context.Context, session string) error {
	_ = runExternalCommand(ctx, "tmux", "kill-session", "-t", session)
	return nil
}
