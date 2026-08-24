package traffic

import (
	"testing"
	"time"
)

func TestHuman(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.00 B"},
		{512, "512.00 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
	}
	for _, c := range cases {
		if got := Human(c.in); got != c.want {
			t.Errorf("Human(%d)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestLocationFallbacks(t *testing.T) {
	cases := []struct {
		name    string
		wantOff int // 秒
	}{
		{"Asia/Shanghai", 8 * 3600},
		{"UTC+8", 8 * 3600},
		{"UTC-5:30", -5*3600 - 1800},
		{"GMT+2", 2 * 3600},
		{"asia/kolkata", 5*3600 + 1800},
		{"Asia/Tokyo", 9 * 3600},
	}
	for _, c := range cases {
		loc := Location(c.name)
		_, off := time.Date(2024, 1, 1, 12, 0, 0, 0, loc).Zone()
		if off != c.wantOff {
			t.Errorf("Location(%q) offset=%d, want %d", c.name, off, c.wantOff)
		}
	}
	// 无法解析的名称回退 UTC+8
	loc := Location("Mars/Olympus")
	_, off := time.Date(2024, 1, 1, 0, 0, 0, 0, loc).Zone()
	if off != 8*3600 {
		t.Errorf("未知时区应回退 UTC+8, got %d", off)
	}
	// local/system
	if Location("local") != time.Local || Location("system") != time.Local {
		t.Error("local/system 应返回 time.Local")
	}
}

func TestTodayStr(t *testing.T) {
	// 同一 UTC 时刻在 UTC+8 已跨天（22:13Z -> 次日 06:13+08）
	u := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	got := u.In(Location("Asia/Shanghai")).Format("2006-01-02")
	if got != "2023-11-15" {
		t.Fatalf("前置检查失败: %s", got)
	}
	if TodayStr("Asia/Shanghai")[4] != '-' { // 冒烟：格式合法
		t.Fatal("日期格式非法")
	}
}
