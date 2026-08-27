package policy

import "time"

// 定时重置周期的时长（秒）。
// 月严格按「每月 30 天」计算（用户要求），即 30 * 86400 秒。
const (
	PeriodSecondsHourly  = int64(3600)
	PeriodSecondsDaily   = int64(86400)
	PeriodSecondsWeekly  = int64(7 * 86400)
	PeriodSecondsMonthly = int64(30 * 86400)
)

// validPeriod 判定周期字符串是否合法。
func validPeriod(p string) bool {
	switch p {
	case "hourly", "daily", "weekly", "monthly":
		return true
	}
	return false
}

func periodSeconds(p string) int64 {
	switch p {
	case "hourly":
		return PeriodSecondsHourly
	case "daily":
		return PeriodSecondsDaily
	case "weekly":
		return PeriodSecondsWeekly
	case "monthly":
		return PeriodSecondsMonthly
	}
	return 0
}

// TimeOfDayToSeconds 把 "HH:MM:SS" 解析成当天秒数（0..86399）。非法返回 -1。
func TimeOfDayToSeconds(t string) int {
	if len(t) != 8 || t[2] != ':' || t[5] != ':' {
		return -1
	}
	h := int(t[0]-'0')*10 + int(t[1]-'0')
	m := int(t[3]-'0')*10 + int(t[4]-'0')
	s := int(t[6]-'0')*10 + int(t[7]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 || s < 0 || s > 59 {
		return -1
	}
	return h*3600 + m*60 + s
}

// SecondsToTimeOfDay 把当天秒数格式化为 "HH:MM:SS"。
func SecondsToTimeOfDay(secs int) string {
	secs %= 86400
	if secs < 0 {
		secs += 86400
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	var b [8]byte
	b[0] = byte('0' + h/10)
	b[1] = byte('0' + h%10)
	b[2] = ':'
	b[3] = byte('0' + m/10)
	b[4] = byte('0' + m%10)
	b[5] = ':'
	b[6] = byte('0' + s/10)
	b[7] = byte('0' + s%10)
	return string(b[:])
}

// NextResetAt 计算「首次 reset 触发时刻」= now 之后最近一次满足条件的未来时刻，
// 返回 Unix 秒。严格大于 now，精确到秒，且以 loc 时区对齐。
//
//   - hourly：就近的整点（loc 本地小时）后的「分:秒」相位；
//   - daily / weekly / monthly：就近的当天（loc 本地日）的目标时刻。
//     三者的「首次时刻」算法一致——相位（哪天/那周/哪月）由持久化的 next 与
//     后续 +periodSeconds 推进决定，不在首次计算里硬锚定星期/月份。
//
// loc 为 nil 时用 time.Local。
func NextResetAt(now time.Time, period string, resetAtSecs int, loc *time.Location) int64 {
	if !validPeriod(period) {
		return 0
	}
	if loc == nil {
		loc = time.Local
	}
	n := now.In(loc)
	tod := resetAtSecs
	if tod < 0 {
		tod = 0
	}
	if tod > 86399 {
		tod = 86399
	}

	var cand time.Time
	if period == "hourly" {
		// 对齐本地整点 + 「分:秒」相位。
		h, m, d := n.Date()
		hh := n.Hour()
		cand = time.Date(h, m, d, hh, 0, 0, 0, loc).Add(time.Duration(tod%3600) * time.Second)
		if !cand.After(n) {
			cand = cand.Add(time.Hour)
		}
	} else {
		h, m, d := n.Date()
		cand = time.Date(h, m, d, 0, 0, 0, 0, loc).Add(time.Duration(tod) * time.Second)
		if !cand.After(n) {
			cand = cand.Add(24 * time.Hour)
		}
	}
	return cand.Unix()
}

// AdvanceNext 到期后把下次时间推进一个周期（保持相位，月按 30 天）。
// 返回推进后的 Unix 秒；period 非法时返回原值。
func AdvanceNext(next int64, period string) int64 {
	p := periodSeconds(period)
	if p <= 0 {
		return next
	}
	return next + p
}

// AdvanceNextPast 把 next 推进到严格大于 now 的最早未来时刻（应对面板停摆
// 多个周期后恢复的场景）。返回推进后的 Unix 秒。
func AdvanceNextPast(next int64, period string, nowUnix int64) int64 {
	p := periodSeconds(period)
	if p <= 0 {
		return next
	}
	for next <= nowUnix {
		next += p
	}
	return next
}

// NextResetIn 返回距下一次重置的剩余秒数（<=0 表示已到/无效）。
func NextResetIn(now time.Time, period string, resetAtSecs int, loc *time.Location) int64 {
	next := NextResetAt(now, period, resetAtSecs, loc)
	if next == 0 {
		return 0
	}
	return next - now.Unix()
}
