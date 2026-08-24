// Package traffic 实现流量采集（单调差分累加）、SQLite 入账与速率查询，
// 行为逐条对齐旧 Python panel.py 的 Collector。
package traffic

import (
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	// 内嵌 tzdata：无 /usr/share/zoneinfo 的精简系统也能解析 Asia/Shanghai 等。
	_ "time/tzdata"
)

var (
	tzMu    sync.Mutex
	tzCache = map[string]*time.Location{}
	tzWarn  sync.Map
)

// 无 tzdata 系统上的固定偏移回退表（这些区无夏令时，固定偏移完全等价）。
var tzFallbackHours = map[string]float64{
	"asia/shanghai": 8, "asia/chongqing": 8, "asia/urumqi": 6, "asia/hong_kong": 8,
	"asia/macau": 8, "asia/taipei": 8, "asia/singapore": 8, "asia/tokyo": 9,
	"asia/seoul": 9, "asia/bangkok": 7, "asia/kolkata": 5.5, "asia/dubai": 4,
	"utc": 0, "gmt": 0,
}

var tzOffsetRe = regexp.MustCompile(`(?i)^(?:UTC|GMT)?([+-])(\d{1,2})(?::?(\d{2}))?$`)

// Location 解析统计用时区，回退链与旧 _tzinfo 一致：
// zoneinfo -> UTC±HH(:MM) 解析 -> 固定偏移表 -> 兜底 UTC+8。
// local/system 返回系统本地时区。
func Location(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Asia/Shanghai"
	}
	if strings.EqualFold(name, "local") || strings.EqualFold(name, "system") {
		return time.Local
	}
	tzMu.Lock()
	defer tzMu.Unlock()
	if loc, ok := tzCache[name]; ok {
		return loc
	}
	loc := resolveLocation(name)
	tzCache[name] = loc
	return loc
}

func resolveLocation(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	if m := tzOffsetRe.FindStringSubmatch(name); m != nil {
		sign := 1
		if m[1] == "-" {
			sign = -1
		}
		h := atoi(m[2])
		min := atoi("0" + m[3])
		return time.FixedZone(name, sign*(h*3600+min*60))
	}
	if hours, ok := tzFallbackHours[strings.ToLower(name)]; ok {
		return time.FixedZone(name, int(hours*float64(time.Hour/time.Second)))
	}
	if _, loaded := tzWarn.LoadOrStore(strings.ToLower(name), true); !loaded {
		slog.Warn("时区无法解析, 回退到 UTC+8", "tz", name)
	}
	return time.FixedZone("UTC+8", 8*3600)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// TodayStr 返回配置时区下的今天（YYYY-MM-DD）。
func TodayStr(tzName string) string {
	return TimeIn(tzName).Format("2006-01-02")
}

// TimeNow 是包级时钟接缝：生产为 wall clock，测试注入固定时间，
// 保证采集入账与 summary 构建的“今天”完全由同一时钟驱动。
var TimeNow = time.Now

// TodayAt 返回 t 在配置时区下的日期串。
func TodayAt(tzName string, t time.Time) string {
	return t.In(Location(tzName)).Format("2006-01-02")
}

// TimeIn 返回配置时区下的当前时间。
func TimeIn(tzName string) time.Time {
	return time.Now().In(Location(tzName))
}

// LocalZoneName 返回本地时区缩写名，用于 conf.tz 为空时的展示兜底。
func LocalZoneName() string {
	name, _ := time.Now().Zone()
	if name == "" {
		return "UTC"
	}
	return name
}
