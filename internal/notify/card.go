package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// maxListDays 是卡片里逐天列出的上限；更早的部分按月归并成一行。
//
// 为什么必须有这个上限：「每天都带全部历史」意味着卡片会一天天变长，
// 而飞书对单条消息有大小限制——超限时推送会**直接失败**，且失败得很安静，
// 用户很可能几天后才发现日报没了。宁可有损展示，不可静默断供。
const maxListDays = 60

// cardTemplate 按类型选标题栏配色。
func cardTemplate(kind string) string {
	if kind == KindRecover {
		return "orange"
	}
	return "green"
}

// BuildCard 生成一张飞书 interactive 卡片。
//
// 结构：标题栏（含日期）→ 三栏概览 → 分隔线 → 历史记录列表。
// 刻意不放底部小字与条形图：正文越短，越不容易在不同客户端上错位。
func BuildCard(s *Snapshot) map[string]any {
	elements := []any{
		map[string]any{
			"tag":       "column_set",
			"flex_mode": "none",
			"columns": []any{
				cardColumn("总量", traffic.Human(s.Focus.ByteTotal())),
				cardColumn("上传", traffic.Human(s.Focus.Rx)),
				cardColumn("下载", traffic.Human(s.Focus.Tx)),
			},
		},
		map[string]any{"tag": "hr"},
		map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": historyText(s)},
		},
	}
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": cardTemplate(s.Kind),
			"title":    map[string]any{"tag": "plain_text", "content": cardTitle(s)},
		},
		"elements": elements,
	}
}

func cardColumn(label, value string) map[string]any {
	return map[string]any{
		"tag":    "column",
		"width":  "weighted",
		"weight": 1,
		"elements": []any{
			map[string]any{
				"tag":  "div",
				"text": map[string]any{"tag": "lark_md", "content": "**" + label + "**\n" + value},
			},
		},
	}
}

// cardTitle 生成标题栏文本。日期必须进标题——否则过几天回看，
// 分不清说的是哪一天的流量。
func cardTitle(s *Snapshot) string {
	prefix := ""
	if s.Test {
		prefix = "[测试] "
	}
	if s.Kind == KindRecover {
		return prefix + "⚠️ SBX 重启补发 · " + cnMonthDay(s.Day, s.Slot.Location()) +
			" " + s.Slot.Format("15:04")
	}
	return prefix + "📊 SBX 流量日报 · " + cnMonthDay(s.Day, s.Slot.Location()) +
		" " + cnWeekday(s.Slot.Location(), s.Day)
}

// historyText 渲染历史记录区块：标题行 + 逐天列表（可能折叠）+ 早期按月汇总。
func historyText(s *Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**历史记录** · 累计 %s · %d 天",
		traffic.Human(s.Grand.ByteTotal()), len(s.Days))

	if len(s.Days) == 0 {
		b.WriteString("\n暂无历史数据")
		return b.String()
	}

	listed := s.Days
	var overflow []DayRow
	if len(listed) > maxListDays {
		overflow = listed[maxListDays:]
		listed = listed[:maxListDays]
	}
	for _, d := range listed {
		b.WriteString("\n" + dayLine(d))
	}
	if len(overflow) > 0 {
		b.WriteString("\n" + monthSummary(overflow))
	}
	return b.String()
}

func dayLine(d DayRow) string {
	mark := ""
	if d.InProgress {
		mark = "（进行中）"
	}
	return fmt.Sprintf("%s    %s%s", shortDay(d.Day), traffic.Human(d.ByteTotal()), mark)
}

// shortDay 把 YYYY-MM-DD 压成 MM-DD；长度异常时原样返回，绝不切片越界。
func shortDay(day string) string {
	if len(day) == len("2006-01-02") {
		return day[5:]
	}
	return day
}

// monthSummary 把更早的日期按月归并，每月一行。
func monthSummary(rows []DayRow) string {
	type acc struct {
		days  int
		bytes int64
	}
	var order []string
	agg := map[string]*acc{}
	for _, r := range rows {
		key := r.Day
		if len(key) >= 7 {
			key = key[:7]
		}
		a := agg[key]
		if a == nil {
			a = &acc{}
			agg[key] = a
			order = append(order, key)
		}
		a.days++
		a.bytes += r.ByteTotal()
	}
	lines := make([]string, 0, len(order))
	for _, k := range order {
		a := agg[k]
		lines = append(lines, fmt.Sprintf("%s · %d 天 · %s", k, a.days, traffic.Human(a.bytes)))
	}
	return strings.Join(lines, "\n")
}

// cnMonthDay 把 YYYY-MM-DD 渲染成 "10月13日"。
func cnMonthDay(day string, loc *time.Location) string {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return day
	}
	return fmt.Sprintf("%d月%d日", int(t.Month()), t.Day())
}

// cnWeekday 返回 "周一"…"周日"。
func cnWeekday(loc *time.Location, day string) string {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return ""
	}
	return [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(t.Weekday())]
}
