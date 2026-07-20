package channelagent

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSpec 是解析後的 5 欄位 cron 表達式（分 時 日 月 週，週日=0）。
type CronSpec struct {
	minute, hour, day, month, weekday func(int) bool
}

// ParseCron 解析標準 5 欄位 cron 表達式：分(0-59) 時(0-23) 日(1-31) 月(1-12) 週(0-6，週日=0)。
// 每欄支援 *、N、N-M、N,M,...、*/S、N-M/S。
func ParseCron(expr string) (CronSpec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return CronSpec{}, fmt.Errorf("cron 表達式需 5 個欄位（分 時 日 月 週），收到 %d 個: %q", len(fields), expr)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return CronSpec{}, fmt.Errorf("分欄位錯誤: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return CronSpec{}, fmt.Errorf("時欄位錯誤: %w", err)
	}
	day, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return CronSpec{}, fmt.Errorf("日欄位錯誤: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return CronSpec{}, fmt.Errorf("月欄位錯誤: %w", err)
	}
	weekday, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return CronSpec{}, fmt.Errorf("週欄位錯誤: %w", err)
	}
	return CronSpec{minute: minute, hour: hour, day: day, month: month, weekday: weekday}, nil
}

func parseCronField(field string, min, max int) (func(int) bool, error) {
	items := strings.Split(field, ",")
	ranges := make([][2]int, 0, len(items))
	step := make([]int, 0, len(items))
	for _, item := range items {
		lo, hi, st, err := parseCronItem(item, min, max)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, [2]int{lo, hi})
		step = append(step, st)
	}
	return func(v int) bool {
		for i, r := range ranges {
			if v < r[0] || v > r[1] {
				continue
			}
			if (v-r[0])%step[i] == 0 {
				return true
			}
		}
		return false
	}, nil
}

// parseCronItem 解析單一項目（*、N、N-M、*/S、N-M/S），回傳 [lo, hi] 範圍與 step。
func parseCronItem(item string, min, max int) (lo, hi, step int, err error) {
	step = 1
	body := item
	if idx := strings.Index(item, "/"); idx >= 0 {
		body = item[:idx]
		step, err = strconv.Atoi(item[idx+1:])
		if err != nil || step <= 0 {
			return 0, 0, 0, fmt.Errorf("非法 step: %q", item)
		}
	}
	if body == "*" {
		return min, max, step, nil
	}
	if dash := strings.Index(body, "-"); dash >= 0 {
		lo, err = strconv.Atoi(body[:dash])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("非法範圍: %q", item)
		}
		hi, err = strconv.Atoi(body[dash+1:])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("非法範圍: %q", item)
		}
	} else {
		n, err := strconv.Atoi(body)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("非法數值: %q", item)
		}
		lo, hi = n, n
	}
	if lo < min || hi > max || lo > hi {
		return 0, 0, 0, fmt.Errorf("值超出範圍(%d-%d): %q", min, max, item)
	}
	return lo, hi, step, nil
}

// Matches 判斷 t（在其自身時區下）是否符合這個 cron 表達式。
func (c CronSpec) Matches(t time.Time) bool {
	return c.minute(t.Minute()) &&
		c.hour(t.Hour()) &&
		c.day(t.Day()) &&
		c.month(int(t.Month())) &&
		c.weekday(int(t.Weekday()))
}

// cronSearchLimit 限制 NextOccurrence 的最長搜尋範圍，避免不可能滿足的表達式
// （例如日=31 又月=2）造成無窮迴圈。
const cronSearchLimit = 4 * 366 * 24 * time.Hour

// NextOccurrence 找出 after 之後（不含 after 本身，取分鐘對齊）第一個符合 spec
// 的時間點，以 loc 時區計算。找不到（在搜尋上限內）回傳 ok=false。
func NextOccurrence(spec CronSpec, loc *time.Location, after time.Time) (time.Time, bool) {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := after.Add(cronSearchLimit)
	for t.Before(limit) {
		if spec.Matches(t) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}
