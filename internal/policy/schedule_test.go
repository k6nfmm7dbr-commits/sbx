package policy

import (
	"testing"
	"time"
)

var cst = time.FixedZone("CST", 8*3600)

func at(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, cst)
}

func equalUnix(t *testing.T, what string, got int64, want time.Time) {
	t.Helper()
	if got != want.Unix() {
		t.Fatalf("%s: want %s(%d), got %d (%s)", what,
			want.Format("2006-01-02 15:04:05"), want.Unix(), got,
			time.Unix(got, 0).In(cst).Format("2006-01-02 15:04:05"))
	}
}

func TestParseResetTime(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"00:00", 0},
		{"08:00", 28800},
		{"23:59", 86340},
		{"00:00:00", 0},
		{"08:00:00", 28800},
		{"23:59:59", 86399},
		// 非法：段数错 / 范围错 / 非数字
		{"8:00", -1}, {"8:00:00", -1}, {"24:00", -1}, {"23:60", -1},
		{"23:59:60", -1}, {"", -1}, {"ab:cd", -1}, {"12:00:00:00", -1}, {"12-00", -1},
	}
	for _, c := range cases {
		if got := ParseResetTime(c.in); got != c.want {
			t.Errorf("ParseResetTime(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestValidResetDay(t *testing.T) {
	for _, d := range []int{1, 2, 15, 29, 30} {
		if !ValidResetDay(d) {
			t.Errorf("ValidResetDay(%d) 应为 true", d)
		}
	}
	for _, d := range []int{-1, 0, 31, 100} {
		if ValidResetDay(d) {
			t.Errorf("ValidResetDay(%d) 应为 false", d)
		}
	}
}

// 8 月 27 日 22:39 设「每月 21 日 00:00」→ 下次是 9 月 21 日 00:00。
// 不是 8 月 21 日（已过去），更不是旧 30 天步长模型的「明天的同一时刻」。
func TestNextResetMonthlyDay21(t *testing.T) {
	now := at(2026, time.August, 27, 22, 39, 0)
	equalUnix(t, "next", NextResetAt(now, 21, 0, cst), at(2026, time.September, 21, 0, 0, 0))
}

// 当月配置时刻还没过 → 本月命中。
func TestNextResetSameMonth(t *testing.T) {
	now := at(2026, time.August, 10, 8, 0, 0)
	equalUnix(t, "next", NextResetAt(now, 21, 9*3600+30*60, cst), at(2026, time.August, 21, 9, 30, 0))
}

// 严格大于 now：now 恰好等于配置时刻时返回下个月。
func TestNextResetStrictlyFuture(t *testing.T) {
	now := at(2026, time.August, 21, 0, 0, 0)
	equalUnix(t, "next", NextResetAt(now, 21, 0, cst), at(2026, time.September, 21, 0, 0, 0))
}

// 29/30 在小月钳到月末；闰年 2 月钳到 29。
func TestNextResetClampShortMonth(t *testing.T) {
	// 2026-02 平月 28 天：day=30 → 2 月 28 日 00:00
	equalUnix(t, "2026-02", NextResetAt(at(2026, time.February, 10, 0, 0, 0), 30, 0, cst),
		at(2026, time.February, 28, 0, 0, 0))
	// 2028-02 闰年 29 天：day=30 → 2 月 29 日
	equalUnix(t, "2028-02", NextResetAt(at(2028, time.February, 10, 0, 0, 0), 30, 0, cst),
		at(2028, time.February, 29, 0, 0, 0))
	// 30 天的月份（4 月）：day=30 原样命中
	equalUnix(t, "2026-04", NextResetAt(at(2026, time.April, 10, 0, 0, 0), 30, 0, cst),
		at(2026, time.April, 30, 0, 0, 0))
}

// 推进以「配置日」锚定：2 月钳到 28 之后，3 月回到配置日 30，而非停在 28。
func TestAdvanceNextAnchoredToConfiguredDay(t *testing.T) {
	feb := NextResetAt(at(2026, time.February, 10, 0, 0, 0), 30, 0, cst) // 2026-02-28
	equalUnix(t, "mar", AdvanceNext(feb, 30, 0, cst), at(2026, time.March, 30, 0, 0, 0))
	// 常规推进：9-21 → 10-21，时刻保持
	sep := NextResetAt(at(2026, time.September, 22, 0, 0, 0), 21, 8*3600, cst)
	equalUnix(t, "oct", AdvanceNext(sep, 21, 8*3600, cst), at(2026, time.October, 21, 8, 0, 0))
}

// 停摆多周期恢复：1 月 21 日停机、4 月 10 日恢复 → 直接推进到 4 月 21 日 00:00，
// 日历日始终是 21（旧 30 天步长模型会漂移到 20/19 日）。
func TestAdvanceNextPastRecoversToCalendarDay(t *testing.T) {
	stale := at(2026, time.January, 21, 0, 0, 0).Unix()
	got := AdvanceNextPast(stale, 21, 0, cst, at(2026, time.April, 10, 12, 0, 0).Unix())
	equalUnix(t, "next", got, at(2026, time.April, 21, 0, 0, 0))
}

// 不变量：连续推进严格递增、时刻相位恒定；非法配置原样/返回 0。
func TestScheduleInvariants(t *testing.T) {
	base := at(2026, time.January, 31, 23, 59, 59).Unix()
	for i := 0; i < 24; i++ {
		nx := AdvanceNext(base, 21, 9*3600+30*60, cst)
		if nx <= base {
			t.Fatalf("第 %d 次推进未前进: %d -> %d", i, base, nx)
		}
		g := time.Unix(nx, 0).In(cst)
		if g.Day() != 21 || g.Hour() != 9 || g.Minute() != 30 || g.Second() != 0 {
			t.Fatalf("推进后相位漂移: %s", g)
		}
		base = nx
	}
	if AdvanceNext(12345, 0, 0, cst) != 12345 {
		t.Fatal("day 非法时 AdvanceNext 应原样返回")
	}
	if AdvanceNextPast(1, 0, 0, cst, 9999999) != 1 {
		t.Fatal("day 非法时 AdvanceNextPast 应原样返回")
	}
	if NextResetAt(time.Now(), 0, 0, cst) != 0 {
		t.Fatal("day 非法时 NextResetAt 应返回 0")
	}
	if NextResetAt(time.Now(), 21, -1, cst) != 0 {
		t.Fatal("时刻非法时 NextResetAt 应返回 0")
	}
	if NextResetIn(at(2026, time.August, 27, 0, 0, 0), 28, 8*3600, cst) != int64(8*3600+24*3600) {
		t.Fatal("NextResetIn 剩余秒数不对")
	}
}
