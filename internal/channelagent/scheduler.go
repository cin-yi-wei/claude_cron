package channelagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Trigger 是一個定時觸發規則：時間到了就合成一則訊息塞進指定 binding 的
// inbox，走現有 inbox→worker→outbox pipeline，等同使用者手動在該頻道打字。
type Trigger struct {
	Name    string `json:"name"`
	Binding string `json:"binding"`
	// Cron 是標準 5 欄位表達式（分 時 日 月 週）。
	Cron string `json:"cron"`
	// Timezone 是 IANA 時區名（例如 Asia/Taipei）；空字串預設 defaultTriggerTimezone。
	Timezone string `json:"timezone,omitempty"`
	Message  string `json:"message"`
	// CatchUp: 排程器離線（機器關機/serve 沒跑）錯過的時間點，回來後要不要補跑一次。
	// true = 補跑一次；false = 直接跳過，等下一次排定時間。
	CatchUp bool `json:"catch_up"`
	Enabled bool `json:"enabled"`
	// LastChecked 是排程器上次檢查到的時間點（RFC3339），用來界定「錯過」的起點。
	LastChecked string `json:"last_checked,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

type TriggerConfig struct {
	Triggers []Trigger `json:"triggers"`
}

// DefaultTriggerTimezone 是 Trigger.Timezone 留空時的預設時區。
const DefaultTriggerTimezone = "Asia/Taipei"

// staleTriggerSkip: CatchUp=false 時，錯過超過這麼久就放棄補跑（吞掉、往前推進），
// 避免排程器重啟後對著陳舊 LastChecked 誤判「剛好錯過」。
const staleTriggerSkip = 10 * time.Minute

func TriggersPath(root string) string {
	return filepath.Join(root, "triggers.json")
}

func LoadTriggers(root string) (TriggerConfig, error) {
	var cfg TriggerConfig
	if err := ReadJSON(TriggersPath(root), &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TriggerConfig{}, nil
		}
		return TriggerConfig{}, err
	}
	return cfg, nil
}

func SaveTriggers(root string, cfg TriggerConfig) error {
	return AtomicWriteJSON(TriggersPath(root), cfg)
}

func (c *TriggerConfig) Get(name string) (Trigger, bool) {
	for _, t := range c.Triggers {
		if t.Name == name {
			return t, true
		}
	}
	return Trigger{}, false
}

func (c *TriggerConfig) Add(t Trigger) error {
	if _, ok := c.Get(t.Name); ok {
		return fmt.Errorf("trigger %q 已存在", t.Name)
	}
	c.Triggers = append(c.Triggers, t)
	return nil
}

func (c *TriggerConfig) Remove(name string) bool {
	for i, t := range c.Triggers {
		if t.Name == name {
			c.Triggers = append(c.Triggers[:i], c.Triggers[i+1:]...)
			return true
		}
	}
	return false
}

// BuildTriggerMessage 合成一則「使用者訊息」，內容就是 trigger 設定的 payload；
// MessageID 帶時間戳確保每次觸發都是新訊息（不會被 IngestMessages 的去重擋掉）。
func BuildTriggerMessage(t Trigger, binding Binding, now time.Time) SourceMessage {
	return SourceMessage{
		Platform:  binding.PlatformOf(),
		ChannelID: binding.ChannelID,
		MessageID: fmt.Sprintf("sched-%s-%d", t.Name, now.UnixNano()),
		AuthorID:  "scheduler",
		CreatedAt: now.UTC().Format(time.RFC3339),
		Content:   t.Message,
	}
}

// RunSchedulerOnce 檢查所有已啟用的 trigger，該到期的就合成訊息送進對應
// binding 的 inbox。沿用 IngestMessages，不建新的送出路徑。回傳本次觸發次數。
func RunSchedulerOnce(ctx context.Context, root string, now time.Time) (int, error) {
	cfg, err := LoadTriggers(root)
	if err != nil {
		return 0, err
	}
	reg, err := LoadRegistry(root)
	if err != nil {
		return 0, err
	}

	fired := 0
	changed := false
	for i := range cfg.Triggers {
		t := &cfg.Triggers[i]
		if !t.Enabled {
			continue
		}
		binding, ok := reg.Get(t.Binding)
		if !ok {
			// binding 已被刪除/改名：略過，不讓整批排程卡住。
			continue
		}
		tzName := t.Timezone
		if tzName == "" {
			tzName = DefaultTriggerTimezone
		}
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			continue
		}
		spec, err := ParseCron(t.Cron)
		if err != nil {
			continue
		}

		from := now
		if t.LastChecked != "" {
			if parsed, err := time.Parse(time.RFC3339, t.LastChecked); err == nil {
				from = parsed
			}
		}

		due, ok := NextOccurrence(spec, loc, from)
		if !ok || due.After(now) {
			continue // 還沒到期
		}
		if !t.CatchUp && now.Sub(due) > staleTriggerSkip {
			// 錯過太久且不補跑：吞掉這次，時間戳往前推進到 now。
			t.LastChecked = now.UTC().Format(time.RFC3339)
			changed = true
			continue
		}

		msg := BuildTriggerMessage(*t, binding, now)
		if _, err := IngestMessages(ctx, binding.Root, []SourceMessage{msg}); err != nil {
			return fired, err
		}
		t.LastChecked = now.UTC().Format(time.RFC3339)
		changed = true
		fired++
	}

	if changed {
		if err := SaveTriggers(root, cfg); err != nil {
			return fired, err
		}
	}
	return fired, nil
}
