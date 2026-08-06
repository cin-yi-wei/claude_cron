package channelagent

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Platform     string         `json:"platform"`
	PollInterval string         `json:"poll_interval"`
	Mock         MockConfig     `json:"mock"`
	Discord      DiscordConfig  `json:"discord"`
	Telegram     TelegramConfig `json:"telegram"`
	Claude       ClaudeConfig   `json:"claude"`
	Push         PushConfig     `json:"push,omitempty"`
	Admin        AdminConfig    `json:"admin,omitempty"`
	Control      ControlConfig  `json:"control,omitempty"`
	A2A          A2AConfig      `json:"a2a,omitempty"`
	// IdleSleepMinutes: a binding idle this long auto-sleeps (session killed to
	// free RAM; auto-wakes on next message). 0 = default (30 min); <0 = disabled.
	IdleSleepMinutes int `json:"idle_sleep_minutes,omitempty"`
	// StallMinutes: a session with queued work but no transcript progress for this
	// long is treated as stuck and restarted. 0 = default (10 min); <0 = disabled.
	StallMinutes int `json:"stall_minutes,omitempty"`
}

// IdleSleepTimeout resolves the auto-sleep idle threshold. Zero return = feature
// disabled (the supervisor only sleeps when timeout > 0).
func (c Config) IdleSleepTimeout() time.Duration {
	m := c.IdleSleepMinutes
	if m == 0 {
		m = 30 // default: sleep after 30 min idle
	}
	if m < 0 {
		return 0 // explicitly disabled
	}
	return time.Duration(m) * time.Minute
}

// StallTimeout resolves the stall-watchdog threshold. Zero return = disabled.
func (c Config) StallTimeout() time.Duration {
	m := c.StallMinutes
	if m == 0 {
		m = 10 // default: restart after 10 min of no transcript progress
	}
	if m < 0 {
		return 0
	}
	return time.Duration(m) * time.Minute
}

// ControlConfig configures the control channel's own ingestion. Mode "push"
// adds a Discord Gateway feed on top of polling (poll always runs as a backstop
// — the control channel is the lifeline); empty/"poll" is plain polling.
//
// TelegramChatID, when set, enables a SECOND, isolated control plane on Telegram
// (same bot as worker bindings, distinguished by chat id). A user talking to that
// chat can /bind /list /pause etc. but only sees bindings their own plane owns.
type ControlConfig struct {
	Mode           string `json:"mode,omitempty"`
	TelegramChatID string `json:"telegram_chat_id,omitempty"`
}

// ControlPlane identifies one control entrance: its namespace Name (used to tag
// and filter the bindings it owns), the Platform new bindings default to, and the
// channel/chat id it listens on. The Discord plane is named "discord" and keeps
// the legacy cc-control session + root/control paths; others are suffixed.
type ControlPlane struct {
	Name      string
	Platform  string
	ChannelID string
}

// Transport returns a binding's ACTUAL message transport, derived from the
// global demux flags rather than the per-binding mode. Under demux (the current
// model) every binding of a platform shares one connection, so the legacy
// per-binding mode (poll/push) no longer drives ingestion — it's only the
// fallback when demux is off. This keeps display/behaviour aligned with reality.
func (c Config) Transport(b Binding) string {
	switch b.PlatformOf() {
	case PlatformDiscord:
		return c.DiscordTransport()
	case PlatformTelegram:
		return c.TelegramTransport()
	case PlatformWeb:
		return "browser"
	}
	return b.ModeOf()
}

// ControlPlanes returns the configured control entrances: always the Discord
// plane (back-compat), plus a Telegram plane when ControlConfig.TelegramChatID is
// set. Token resolution and source/sender selection happen in the supervisor.
func (c Config) ControlPlanes() []ControlPlane {
	planes := []ControlPlane{{Name: PlatformDiscord, Platform: PlatformDiscord, ChannelID: c.Discord.ChannelID}}
	if c.Control.TelegramChatID != "" {
		planes = append(planes, ControlPlane{Name: PlatformTelegram, Platform: PlatformTelegram, ChannelID: c.Control.TelegramChatID})
	}
	return planes
}

// AdminConfig configures the admin HTTP API. Token, when set, is required as a
// Bearer token on every request. Binding a non-loopback Listen without a Token
// is refused (the API can create/delete shell-capable sessions).
type AdminConfig struct {
	Listen string `json:"listen,omitempty"`
	Token  string `json:"token,omitempty"`
}

// A2AConfig configures the agent-to-agent listener. Listen MUST differ from the
// admin API address: the admin API can create shell-capable bindings and must
// never become externally reachable. Enabled defaults to false — with it off,
// serve starts no listener and runs no A2A sweeps, leaving existing behaviour
// byte-for-byte unchanged.
type A2AConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// CycleSeconds 是 A2A 生命週期迴圈的間隔；0 表示採用預設值（10 秒）。
	CycleSeconds int `json:"cycle_seconds,omitempty"`
}

// A2ACycleInterval is how often the A2A lifecycle runs. It has its own ticker
// and goroutine, separate from the cron scheduler's: DrainQueue starts sandboxes
// synchronously and would otherwise stall scheduling for every cc- binding.
func (c Config) A2ACycleInterval() time.Duration {
	if c.A2A.CycleSeconds > 0 {
		return time.Duration(c.A2A.CycleSeconds) * time.Second
	}
	return 10 * time.Second
}

// A2AListen resolves the A2A listen address, defaulting to a port distinct from
// the admin default (127.0.0.1:8787).
func (c Config) A2AListen() string {
	if c.A2A.Listen != "" {
		return c.A2A.Listen
	}
	return "127.0.0.1:8790"
}

// PushConfig configures push-mode (active) ingestion. Listen is the local
// address the webhook/HTTP server binds (e.g. ":8443"); Secret, when set, is
// the token Telegram echoes in X-Telegram-Bot-Api-Secret-Token. All optional;
// poll-mode bindings ignore this block entirely.
type PushConfig struct {
	Listen string `json:"listen,omitempty"`
	Secret string `json:"secret,omitempty"`
	// PublicURL is the externally reachable base URL Telegram POSTs to (the
	// binding's path is appended). Empty disables webhook registration, so the
	// server still runs locally but Telegram is not told to push.
	PublicURL string `json:"public_url,omitempty"`
}

type MockConfig struct {
	SourcePath string `json:"source_path"`
}

type DiscordConfig struct {
	TokenEnv  string `json:"token_env"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// Transport selects how the whole bot ingests: "gateway" (single shared
	// Gateway websocket, demuxed by channel id) or "poll" (per-binding REST poll).
	// This is the authoritative knob; GatewayDemux is the legacy boolean kept only
	// as a fallback for old configs (see DiscordTransport).
	Transport string `json:"transport,omitempty"`
	// Deprecated: use Transport. Read only as a fallback when Transport is empty.
	GatewayDemux bool `json:"gateway_demux,omitempty"`
}

const (
	TransportGateway = "gateway"
	TransportWebhook = "webhook"
	TransportPoll    = "poll"
)

// DiscordTransport resolves the Discord ingestion transport: the explicit
// Transport enum if set, else the legacy GatewayDemux boolean, else "gateway"
// (the demux model is the default).
func (c Config) DiscordTransport() string {
	if c.Discord.Transport != "" {
		return c.Discord.Transport
	}
	if c.Discord.GatewayDemux {
		return TransportGateway
	}
	return TransportGateway
}

type TelegramConfig struct {
	TokenEnv string `json:"token_env"`
	ChatID   string `json:"chat_id"`
	BaseURL  string `json:"base_url,omitempty"`
	// Transport: "webhook" (single demux endpoint) or "poll" (shared getUpdates
	// reader). Authoritative; Webhook is the legacy boolean fallback.
	Transport string `json:"transport,omitempty"`
	// Deprecated: use Transport. Read only as a fallback when Transport is empty.
	Webhook bool `json:"webhook,omitempty"`
}

// TelegramTransport resolves the Telegram ingestion transport: the explicit
// Transport enum if set, else the legacy Webhook boolean, else "poll".
func (c Config) TelegramTransport() string {
	if c.Telegram.Transport != "" {
		return c.Telegram.Transport
	}
	if c.Telegram.Webhook {
		return TransportWebhook
	}
	return TransportPoll
}

type ClaudeConfig struct {
	TmuxSession string `json:"tmux_session"`
	Timeout     string `json:"timeout"`
	AutoStart   bool   `json:"auto_start"`
}

func DefaultConfig(platform string) (Config, error) {
	if platform == "" {
		platform = "mock"
	}
	switch platform {
	case "mock", "discord", "telegram", "tg":
	default:
		return Config{}, fmt.Errorf("unsupported platform %q", platform)
	}
	if platform == "tg" {
		platform = "telegram"
	}
	return Config{
		Platform:     platform,
		PollInterval: "10s",
		Mock: MockConfig{
			SourcePath: ".channel-agent/mock/source_messages.json",
		},
		Discord: DiscordConfig{
			TokenEnv: "DISCORD_BOT_TOKEN",
		},
		Telegram: TelegramConfig{
			TokenEnv: "TELEGRAM_BOT_TOKEN",
		},
		Claude: ClaudeConfig{
			TmuxSession: "claude-cron",
			Timeout:     "120s",
			AutoStart:   true,
		},
	}, nil
}

func ConfigPath(root string) string {
	return filepath.Join(root, "config.json")
}

func SaveConfig(root string, cfg Config) error {
	if err := Init(root); err != nil {
		return err
	}
	return AtomicWriteJSON(ConfigPath(root), cfg)
}

func LoadConfig(root string) (Config, error) {
	var cfg Config
	if err := ReadJSON(ConfigPath(root), &cfg); err != nil {
		return Config{}, err
	}
	// A2AConfig 的 docstring 白紙黑字寫著 Listen MUST differ from the admin
	// address，但從來沒有人驗證過。admin API 能建立可執行 shell 的 binding，
	// 讓它跟對外監聽器共用位址等於把管理面公開出去。只在 A2A 啟用時檢查，
	// 於是預設關閉的既有部署行為完全不變。
	//
	// round 10 review, Minor（D10-3）：比對用 addrsCollide，不是原始字串
	// ==——":8787"、"127.0.0.1:8787"、"localhost:8787" 是同一台機器上同一個
	// port 的三種不同寫法，naive 字串比對會被其中任何一種寫法差異繞過這道
	// 檢查。
	if cfg.A2A.Enabled && cfg.Admin.Listen != "" && addrsCollide(cfg.A2AListen(), cfg.Admin.Listen) {
		return Config{}, fmt.Errorf("a2a.listen (%s) must differ from admin.listen (%s): the admin API can create shell-capable bindings and must never be externally reachable", cfg.A2AListen(), cfg.Admin.Listen)
	}
	return cfg, nil
}

// addrsCollide reports whether two "host:port" listen addresses would
// actually contend for the same port on the same machine. A raw string
// comparison misses this: ":8787", "127.0.0.1:8787" and "localhost:8787" are
// three different strings but the same port on the same loopback interface.
// Addresses that don't parse as host:port are compared as raw strings —
// conservative, since an unparseable value can't be proven equivalent to
// anything else.
func addrsCollide(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	ha, pa, erra := net.SplitHostPort(a)
	hb, pb, errb := net.SplitHostPort(b)
	if erra != nil || errb != nil {
		return a == b
	}
	if pa != pb {
		return false
	}
	na, nb := normalizeListenHost(ha), normalizeListenHost(hb)
	// 「所有介面」跟任何其他寫法在同一個 port 上都算衝突——它本身就佔住了
	// 那個 port 在每一個介面上，包括 loopback，不是只跟另一個「所有介面」
	// 寫法衝突。
	if na == anyInterfaceHost || nb == anyInterfaceHost {
		return true
	}
	return na == nb
}

// anyInterfaceHost is normalizeListenHost's sentinel for "binds every
// interface" (""/"0.0.0.0"/"::"). Never a value a real hostname could
// produce (leading NUL), so it can't collide with normalizeListenHost's
// literal default branch by accident.
const anyInterfaceHost = "\x00any"

// loopbackHost is normalizeListenHost's sentinel for "127.0.0.1"/"localhost"/
// "::1" — three different spellings of the same loopback interface.
const loopbackHost = "\x00loopback"

// normalizeListenHost 把「同一台機器上等價」的幾種常見寫法收斂成同一個代表
// 值：空字串／0.0.0.0／:: 代表「所有介面」；127.0.0.1／localhost／::1 收斂
// 成 loopback。其他寫法只做小寫化、原樣比對，刻意不做 DNS 解析：這個檢查只
// 在 serve 啟動載入設定檔時跑一次，不該為了比對兩個位址而依賴網路或本機解
// 析器行為（測試也不可以因此需要真的網路存取）。
func normalizeListenHost(h string) string {
	h = strings.ToLower(h)
	switch h {
	case "", "0.0.0.0", "::":
		return anyInterfaceHost
	case "127.0.0.1", "localhost", "::1":
		return loopbackHost
	default:
		return h
	}
}
