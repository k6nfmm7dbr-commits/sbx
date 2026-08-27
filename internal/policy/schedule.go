package policy

import "time"

// 定时重置只保留「每月」一种周期：每月第 D 日 HH:MM:SS（面板本地时区）自动把
// 本期已用流量归零。
//
// 配置语义（UI 上的「起始时间」）：
//   - 日：1~30（ResetDay）；29/30 在小月自动钳到该月最后一天（2 月 28/29 日），
//     大月回归配置日本身（见 AdvanceNext 的锚定说明）；
//   - 时：00~23；分：00~59；秒固定为 00 整（内部仍存 HH:MM:SS 规范格式）。
//
// 调度采用「日历月锚定」而非固定 30 天步长：每月的同一天同一时刻触发，
// 倒计时与用户直觉一致（21 号就是每月 21 号）。

// ResetDayMin / ResetDayMax 是起始时间「日」段的合法范围。
const (
	ResetDayMin = 1
	ResetDayMax = 30
)

// ValidResetDay 判定「日」是否合法（1~30）。
func ValidResetDay(day int) bool { return day >= ResetDayMin && day <= ResetDayMax }

// ParseResetTime 解析 "HH:MM" 或 "HH:MM:SS" 为当天秒数（0..86399）。
// 两种宽度都收（兼容浏览器 time 输入只给 HH:MM 的场景），秒缺省 00。
// 非法返回 -1。
func ParseResetTime(t string) int {
	var h, m, s int
	switch len(t) {
	case 5:
		if t[2] != ':' {
			return -1
		}
		s = 0
	case 8:
		if t[2] != ':' || t[5] != ':' {
			return -1
		}
		s = int(t[6]-'0')*10 + int(t[7]-'0')
	default:
		return -1
	}
	h = int(t[0]-'0')*10 + int(t[1]-'0')
	m = int(t[3]-'0')*10 + int(t[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 || s < 0 || s > 59 {
		return -1
	}
	return h*3600 + m*60 + s
}

// SecondsToTimeOfDay 把当天秒数格式化为规范 "HH:MM:SS"。
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

// monthLastDay 返回 y 年 m 月的实际天数。
func monthLastDay(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// monthOccurrence 计算 y 年 m 月里「第 day 日 + 当天 todSecs 秒」的本地时刻；
// day 超出该月天数时钳到月末（29/30 遇 2 月 → 28/29 日）。
func monthOccurrence(y int, m time.Month, day, todSecs int, loc *time.Location) time.Time {
	if day > monthLastDay(y, m) {
		day = monthLastDay(y, m)
	}
	return time.Date(y, m, day, todSecs/3600, (todSecs%3600)/60, todSecs%60, 0, loc)
}

// NextResetAt 计算 now 之后最近一次「每月 D 日 HH:MM:SS」（loc 时区对齐），
// 返回 Unix 秒，严格大于 now。配置非法返回 0。loc 为 nil 时用 time.Local。
func NextResetAt(now time.Time, day, todSecs int, loc *time.Location) int64 {
	if !ValidResetDay(day) || todSecs < 0 || todSecs > 86399 {
		return 0
	}
	if loc == nil {
		loc = time.Local
	}
	n := now.In(loc)
	y, m, _ := n.Date()
	base := time.Date(y, m, 1, 0, 0, 0, 0, loc)
	// 任何合法 day/时刻两年内必然命中，49 个月是防御性上限。
	for i := 0; i < 49; i++ {
		yy, mm, _ := base.AddDate(0, i, 0).Date()
		t := monthOccurrence(yy, mm, day, todSecs, loc)
		if t.After(n) {
			return t.Unix()
		}
	}
	return 0
}

// AdvanceNext 从已触发的 next 推进到下一个日历月的同一「配置日 + 时刻」。
// 关键：用配置的 day（而非 next 的实际落点）参与计算——2 月被钳到 28/29 后，
// 3 月仍回到配置日（如 30 日），不会停在 28 日。
// day 非法时原样返回。
func AdvanceNext(next int64, day, todSecs int, loc *time.Location) int64 {
	if !ValidResetDay(day) {
		return next
	}
	if loc == nil {
		loc = time.Local
	}
	t := time.Unix(next, 0).In(loc)
	y, m, _ := t.Date()
	first := time.Date(y, m, 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
	yy, mm, _ := first.Date()
	return monthOccurrence(yy, mm, day, todSecs, loc).Unix()
}

// AdvanceNextPast 把 next 推进到严格大于 nowUnix 的最早未来时刻
// （面板停摆跨多个周期后恢复的场景）。day 非法时原样返回。
func AdvanceNextPast(next int64, day, todSecs int, loc *time.Location, nowUnix int64) int64 {
	if !ValidResetDay(day) {
		return next
	}
	for i := 0; i < 600 && next <= nowUnix; i++ {
		next = AdvanceNext(next, day, todSecs, loc)
	}
	return next
}

// NextResetIn 返回距下一次重置的剩余秒数（<=0 表示已到/配置无效）。
func NextResetIn(now time.Time, day, todSecs int, loc *time.Location) int64 {
	next := NextResetAt(now, day, todSecs, loc)
	if next == 0 {
		return 0
	}
	return next - now.Unix()
}
