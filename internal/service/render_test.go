package service

import (
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// ---- 显示宽度 / 截断 ---------------------------------------------------------

func TestDispWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"abc", 3},
		{"中文", 4},
		{"中a文", 5},
		{"↑", 1},
		{"今日上传", 8},
		{"2022-blake3", 11},
		{"", 0},
	}
	for _, c := range cases {
		if got := dispWidth(c.s); got != c.want {
			t.Errorf("dispWidth(%q)=%d, want %d", c.s, got, c.want)
		}
	}
}

func TestTruncDisp(t *testing.T) {
	cases := []struct {
		s    string
		w    int
		want string
	}{
		{"short", 20, "short"},
		{"2022-blake3-aes-256-gcm-japan-kddi", 16, "2022-blake3-aes…"},
		{"中文节点名很长", 6, "中文…"},
	}
	for _, c := range cases {
		if got := truncDisp(c.s, c.w); got != c.want {
			t.Errorf("truncDisp(%q,%d)=%q, want %q", c.s, c.w, got, c.want)
		}
	}
}

// ---- 渲染：无节点 / 单节点 / 多节点 / 长名 / 合计 ----------------------------

func mkSummary(nodes []traffic.SummaryNode, today, total traffic.Counters) *traffic.Summary {
	return &traffic.Summary{
		Backend: "nftables",
		Day:     "2026-08-25",
		TZ:      "Asia/Shanghai",
		Nodes:   nodes,
		Today:   today,
		Total:   total,
	}
}

func mkNode(name string, tRx, tTx, totRx, totTx int64) traffic.SummaryNode {
	return traffic.SummaryNode{
		Name:  name,
		Today: traffic.Counters{Rx: tRx, Tx: tTx},
		Total: traffic.Counters{Rx: totRx, Tx: totTx},
	}
}

func TestRenderShowNoNodes(t *testing.T) {
	s := mkSummary(nil, traffic.Counters{}, traffic.Counters{})
	out := renderShow(s, 80)
	if !strings.Contains(out, "暂无节点流量数据") {
		t.Errorf("无节点时应显示占位提示:\n%s", out)
	}
	if strings.Contains(out, "节点流量\n  节点") {
		t.Errorf("无节点时不应打印表头:\n%s", out)
	}
}

func TestRenderShowMultiNodesTotal(t *testing.T) {
	nodes := []traffic.SummaryNode{
		mkNode("trojan-8443", 107*1024+737, 157*1024+911, 5*1024*1024, 6*1024*1024),
		mkNode("ss-38722", 6*1024+543, 5*1024+554, 0, 0),
		mkNode("zero-node", 0, 0, 0, 0),
	}
	s := mkSummary(nodes,
		traffic.Counters{Rx: 114 * 1024, Tx: 163 * 1024},
		traffic.Counters{Rx: 11 * 1024 * 1024, Tx: 12 * 1024 * 1024})
	out := renderShow(s, 80)

	if !strings.Contains(out, "合计") {
		t.Errorf("应包含合计行:\n%s", out)
	}
	if !strings.Contains(out, "0 B") {
		t.Errorf("0 值应显示为 0 B:\n%s", out)
	}
	// 窄布局不含累计列
	if strings.Contains(out, "累计上传") && strings.Contains(out, "trojan-8443") {
		// 概览里有"累计上传"，但节点表里不能有该表头
		if strings.Contains(out, "今日上传") && strings.Contains(out, "今日下载") {
			// 窄布局表头应只有 今日上传/今日下载
			if strings.Count(out, "累计下载") > 1 {
				t.Errorf("窄布局节点表不应含累计列:\n%s", out)
			}
		}
	}
}

func TestRenderShowLongNameTruncated(t *testing.T) {
	nodes := []traffic.SummaryNode{
		mkNode("2022-blake3-aes-256-gcm-japan-kddi", 1024, 1024, 1024, 1024),
	}
	s := mkSummary(nodes, traffic.Counters{Rx: 1024, Tx: 1024}, traffic.Counters{})
	out := renderShow(s, 80)
	if strings.Contains(out, "japan-kddi") {
		t.Errorf("长节点名应被截断:\n%s", out)
	}
}

func TestRenderShowWideLayout(t *testing.T) {
	nodes := []traffic.SummaryNode{mkNode("n1", 1, 2, 3, 4)}
	s := mkSummary(nodes, traffic.Counters{Rx: 1, Tx: 2}, traffic.Counters{Rx: 3, Tx: 4})
	wide := renderShow(s, 120)
	if !strings.Contains(wide, "累计上传") || !strings.Contains(wide, "累计下载") {
		t.Errorf(">=100 列应显示累计列:\n%s", wide)
	}
	narrow := renderShow(s, 80)
	// 窄布局节点表不应有累计列表头（概览里有一个"累计上传/下载"标签，需精确判断）
	if strings.Contains(narrow, "累计上传") && strings.Count(narrow, "今日上传") >= 1 {
		// 概览部分有累计上传/下载各一次；节点表窄布局不应再有表头形式的累计
		// 直接断言窄布局表头行不含累计
		lines := strings.Split(narrow, "\n")
		for _, ln := range lines {
			if strings.Contains(ln, "今日上传") && strings.Contains(ln, "累计上传") {
				t.Errorf("窄布局表头行不应含累计列: %q", ln)
			}
		}
	}
}

// ---- 渲染：14 天表格 ---------------------------------------------------------

func TestRenderDailyEmpty(t *testing.T) {
	out := renderDaily(nil, 80)
	if !strings.Contains(out, "暂无历史流量数据") {
		t.Errorf("无历史数据应显示占位提示:\n%s", out)
	}
}

func TestRenderDailyRows(t *testing.T) {
	rows := []traffic.DailyRow{
		{Day: "2026-08-24", Rx: 1024 * 1024, Tx: 2 * 1024 * 1024},
		{Day: "2026-08-25", Rx: 145 * 1024, Tx: 195 * 1024},
	}
	out := renderDaily(rows, 80)
	if !strings.Contains(out, "2026-08-25") || !strings.Contains(out, "2026-08-24") {
		t.Errorf("日期应完整显示:\n%s", out)
	}
	// 窄布局不含合计列
	lines := strings.Split(out, "\n")
	for _, ln := range lines {
		if strings.Contains(ln, "合计") {
			t.Errorf("窄布局每日表不应含合计列: %q", ln)
		}
	}
}

func TestRenderDailyWide(t *testing.T) {
	rows := []traffic.DailyRow{{Day: "2026-08-25", Rx: 1, Tx: 2}}
	out := renderDaily(rows, 120)
	if !strings.Contains(out, "合计") {
		t.Errorf(">=100 列每日表应含合计列:\n%s", out)
	}
}

// ---- 窄终端不换行（60/70/80 列）---------------------------------------------

func TestRenderShowNoWrapNarrow(t *testing.T) {
	nodes := []traffic.SummaryNode{
		mkNode("trojan-8443", 107*1024+737, 157*1024+911, 5*1024*1024, 6*1024*1024),
		mkNode("中文节点", 31*1024+440, 31*1024+700, 0, 0),
	}
	s := mkSummary(nodes,
		traffic.Counters{Rx: 114 * 1024, Tx: 163 * 1024},
		traffic.Counters{Rx: 11 * 1024 * 1024, Tx: 12 * 1024 * 1024})
	for _, cols := range []int{60, 70, 80} {
		out := renderShow(s, cols)
		for _, ln := range strings.Split(out, "\n") {
			if dispWidth(ln) > cols {
				t.Errorf("cols=%d 行宽 %d 超限: %q", cols, dispWidth(ln), ln)
			}
		}
	}
}

func TestRenderDailyNoWrapNarrow(t *testing.T) {
	rows := []traffic.DailyRow{
		{Day: "2026-08-25", Rx: 1024 * 1024 * 1024, Tx: 2 * 1024 * 1024 * 1024},
	}
	for _, cols := range []int{60, 70, 80} {
		out := renderDaily(rows, cols)
		for _, ln := range strings.Split(out, "\n") {
			if dispWidth(ln) > cols {
				t.Errorf("cols=%d 行宽 %d 超限: %q", cols, dispWidth(ln), ln)
			}
		}
	}
}
