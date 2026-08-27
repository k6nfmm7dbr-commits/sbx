package policy

import (
	"testing"
	"time"
)

func utcLoc() *time.Location {
	l, _ := time.LoadLocation("UTC")
	return l
}

func TestTimeOfDayRoundTrip(t *testing.T) {
	cases := []string{"00:00:00", "00:00:01", "12:34:56", "23:59:59", "08:05:09"}
	for _, c := range cases {
		secs := TimeOfDayToSeconds(c)
		if secs < 0 {
			t.Fatalf("%s 解析失败", c)
		}
		if got := SecondsToTimeOfDay(secs); got != c {
			t.Fatalf("%s round-trip = %s", c, got)
		}
	}
	for _, bad := range []string{"24:00:00", "12:60:00", "12:00:60", "abc", "1:2:3"} {
		if TimeOfDayToSeconds(bad) >= 0 {
			t.Fatalf("%q 应非法", bad)
		}
	}
}

func TestNextResetDailyExactSecond(t *testing.T) {
	l := utcLoc()
	// 08:00:00 之前 → 今天 08:00:00。
	now := time.Date(2026, 8, 27, 7, 30, 0, 0, l)
	next := NextResetAt(now, "daily", TimeOfDayToSeconds("08:00:00"), l)
	want := time.Date(2026, 8, 27, 8, 0, 0, 0, l).Unix()
	if next != want {
		t.Fatalf("daily 08:00 前: got %d want %d", next, want)
	}
	// 08:00:01 → 明天 08:00:00（精确到秒）。
	now = time.Date(2026, 8, 27, 8, 0, 1, 0, l)
	next = NextResetAt(now, "daily", TimeOfDayToSeconds("08:00:00"), l)
	want = time.Date(2026, 8, 28, 8, 0, 0, 0, l).Unix()
	if next != want {
		t.Fatalf("daily 08:00 后: got %d want %d", next, want)
	}
	// 恰好 08:00:00 → 严格 > now，取明天。
	now = time.Date(2026, 8, 27, 8, 0, 0, 0, l)
	next = NextResetAt(now, "daily", TimeOfDayToSeconds("08:00:00"), l)
	want = time.Date(2026, 8, 28, 8, 0, 0, 0, l).Unix()
	if next != want {
		t.Fatalf("daily 恰好 08:00: 应取明天, got %d want %d", next, want)
	}
}

func TestNextResetHourly(t *testing.T) {
	l := utcLoc()
	// hourly：相位 = 分:秒（00:15:30 → 每小时 15:30）。
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, l)
	next := NextResetAt(now, "hourly", TimeOfDayToSeconds("00:15:30"), l)
	want := time.Date(2026, 8, 27, 10, 15, 30, 0, l).Unix()
	if next != want {
		t.Fatalf("hourly: got %d want %d", next, want)
	}
	// 过了 10:15:30 → 11:15:30。
	now = time.Date(2026, 8, 27, 10, 15, 31, 0, l)
	next = NextResetAt(now, "hourly", TimeOfDayToSeconds("00:15:30"), l)
	want = time.Date(2026, 8, 27, 11, 15, 30, 0, l).Unix()
	if next != want {
		t.Fatalf("hourly 过点: got %d want %d", next, want)
	}
}

func TestNextResetWeeklyAdvance(t *testing.T) {
	l := utcLoc()
	// weekly 首次就近当天目标时刻；后续靠 AdvanceNext 每 7 天推进。
	now := time.Date(2026, 8, 27, 9, 0, 0, 0, l)
	first := NextResetAt(now, "weekly", TimeOfDayToSeconds("08:00:00"), l)
	// 08:00 已过 → 首次应是明天 08:00。
	want := time.Date(2026, 8, 28, 8, 0, 0, 0, l).Unix()
	if first != want {
		t.Fatalf("weekly 首次: got %d want %d", first, want)
	}
	// 推进 3 个周期，间隔恒 7 天。
	n := first
	for i := 0; i < 3; i++ {
		next := AdvanceNext(n, "weekly")
		if next-n != 7*86400 {
			t.Fatalf("weekly 步长应 7 天: got %d", next-n)
		}
		n = next
	}
}

func TestNextResetMonthly30Days(t *testing.T) {
	l := utcLoc()
	month := int64(30 * 86400)
	// 月 = 30 天：AdvanceNext 步长恒 30 天。
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, l)
	tod := TimeOfDayToSeconds("00:30:00")
	first := NextResetAt(now, "monthly", tod, l)
	if first != time.Date(2026, 8, 27, 0, 30, 0, 0, l).Unix() {
		t.Fatalf("monthly 首次: got %d", first)
	}
	n := first
	for i := 0; i < 3; i++ {
		next := AdvanceNext(n, "monthly")
		if next-n != month {
			t.Fatalf("monthly 步长应 30 天: got %d", next-n)
		}
		n = next
	}
}

func TestNextResetIn(t *testing.T) {
	l := utcLoc()
	now := time.Date(2026, 8, 27, 7, 30, 0, 0, l)
	rem := NextResetIn(now, "daily", TimeOfDayToSeconds("08:00:00"), l)
	if rem != 30*60 {
		t.Fatalf("剩余应 1800s, got %d", rem)
	}
}

func TestAdvanceNextPast(t *testing.T) {
	// 面板停摆多周期：next 已过期很久，应推进到严格 > now 的未来。
	next := int64(1000)
	now := int64(10 * 86400) // 10 天后
	got := AdvanceNextPast(next, "daily", now)
	if got <= now {
		t.Fatalf("AdvanceNextPast 应推进到未来: got %d now %d", got, now)
	}
	// 步长恒 1 天、相位保持：got 与 next 的差应是 86400 的整数倍。
	if (got-next)%86400 != 0 {
		t.Fatalf("daily 推进应保持日相位: %d", got-next)
	}
	// monthly 步长 30 天。
	got2 := AdvanceNextPast(int64(0), "monthly", int64(31*86400))
	if got2 <= int64(31*86400) || (got2)%(30*86400) != 0 {
		t.Fatalf("monthly 推进异常: %d", got2)
	}
}

func TestPeriodSecondsAndValid(t *testing.T) {
	if periodSeconds("hourly") != 3600 ||
		periodSeconds("daily") != 86400 ||
		periodSeconds("weekly") != 7*86400 ||
		periodSeconds("monthly") != 30*86400 {
		t.Fatal("periodSeconds 常量错误")
	}
	if !validPeriod("daily") || validPeriod("yearly") || validPeriod("") {
		t.Fatal("validPeriod 判定错误")
	}
	if periodSeconds("bogus") != 0 || NextResetAt(time.Now(), "bogus", 0, nil) != 0 {
		t.Fatal("非法周期应返回 0")
	}
}
