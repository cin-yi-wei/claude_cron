package channelagent

import (
	"testing"
	"time"
)

func TestParseCronRejectsWrongFieldCount(t *testing.T) {
	if _, err := ParseCron("* * * *"); err == nil {
		t.Fatal("expected error for 4-field expression")
	}
	if _, err := ParseCron("* * * * * *"); err == nil {
		t.Fatal("expected error for 6-field expression")
	}
}

func TestParseCronRejectsOutOfRange(t *testing.T) {
	cases := []string{"60 * * * *", "* 24 * * *", "* * 32 * *", "* * * 13 *", "* * * * 7"}
	for _, expr := range cases {
		if _, err := ParseCron(expr); err == nil {
			t.Fatalf("expected error for %q", expr)
		}
	}
}

func TestCronSpecMatchesEveryMinute(t *testing.T) {
	spec, err := ParseCron("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Matches(time.Date(2026, 7, 20, 13, 37, 0, 0, time.UTC)) {
		t.Fatal("expected * * * * * to match any minute")
	}
}

func TestCronSpecMatchesExactTime(t *testing.T) {
	spec, err := ParseCron("30 9 * * *") // 每天 09:30
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Matches(time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC)) {
		t.Fatal("expected 09:30 to match")
	}
	if spec.Matches(time.Date(2026, 7, 20, 9, 31, 0, 0, time.UTC)) {
		t.Fatal("expected 09:31 to NOT match")
	}
}

func TestCronSpecStep(t *testing.T) {
	spec, err := ParseCron("*/15 * * * *") // 每 15 分
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []int{0, 15, 30, 45} {
		if !spec.Matches(time.Date(2026, 7, 20, 0, m, 0, 0, time.UTC)) {
			t.Fatalf("expected minute %d to match */15", m)
		}
	}
	for _, m := range []int{1, 14, 16, 44} {
		if spec.Matches(time.Date(2026, 7, 20, 0, m, 0, 0, time.UTC)) {
			t.Fatalf("expected minute %d to NOT match */15", m)
		}
	}
}

func TestCronSpecList(t *testing.T) {
	spec, err := ParseCron("0 9,18 * * *") // 每天 09:00 及 18:00
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Matches(time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)) {
		t.Fatal("expected 09:00 to match")
	}
	if !spec.Matches(time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)) {
		t.Fatal("expected 18:00 to match")
	}
	if spec.Matches(time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected 12:00 to NOT match")
	}
}

func TestCronSpecWeekday(t *testing.T) {
	spec, err := ParseCron("0 9 * * 1-5") // 平日 09:00
	if err != nil {
		t.Fatal(err)
	}
	mon := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC) // 2026-07-20 是週一
	if mon.Weekday() != time.Monday {
		t.Fatalf("test fixture date is not Monday: %v", mon.Weekday())
	}
	if !spec.Matches(mon) {
		t.Fatal("expected Monday 09:00 to match weekday 1-5")
	}
	sun := mon.AddDate(0, 0, -1)
	if spec.Matches(sun) {
		t.Fatal("expected Sunday to NOT match weekday 1-5")
	}
}

func TestNextOccurrenceFindsFollowingMinute(t *testing.T) {
	spec, err := ParseCron("0 9 * * *") // 每天 09:00
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 7, 20, 8, 59, 30, 0, time.UTC)
	got, ok := NextOccurrence(spec, time.UTC, after)
	if !ok {
		t.Fatal("expected an occurrence")
	}
	want := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextOccurrenceSkipsToNextDayWhenPast(t *testing.T) {
	spec, err := ParseCron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC) // 已過今天 09:00
	got, ok := NextOccurrence(spec, time.UTC, after)
	if !ok {
		t.Fatal("expected an occurrence")
	}
	want := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextOccurrenceHonorsTimezone(t *testing.T) {
	spec, err := ParseCron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	taipei, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// after 是 UTC 01:00 = 台北 09:00，所以下一次 09:00（台北）是隔天。
	after := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	got, ok := NextOccurrence(spec, taipei, after)
	if !ok {
		t.Fatal("expected an occurrence")
	}
	if got.Hour() != 9 {
		t.Fatalf("expected hour 9 in Asia/Taipei, got %v", got)
	}
	if got.Day() != 21 {
		t.Fatalf("expected next day (21), got day %d", got.Day())
	}
}

func TestNextOccurrenceUnsatisfiableReturnsFalse(t *testing.T) {
	// 日=31 且月=4（4月只有30天）永遠不會發生。
	spec, err := ParseCron("0 0 31 4 *")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := NextOccurrence(spec, time.UTC, after); ok {
		t.Fatal("expected no occurrence for impossible day/month combo")
	}
}
