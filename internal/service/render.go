package service

import (
	"fmt"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// 列宽（显示列）。numW 覆盖 "1023.99 TiB" 这类最长值。
const (
	nameW = 16 // 节点名列宽
	numW  = 12 // 数值列宽
	dateW = 10 // 日期列宽
)

// sepLine 输出一条 ASCII 分隔线（不用 Unicode 框线，避免 CJK/字体错位）。
func sepLine(w int) string { return strings.Repeat("-", w) }

// renderShow 渲染「流量统计」主视图：概览 + 节点流量。
// cols 为终端列宽；<=0 时按窄布局（<=80）处理。
func renderShow(s *traffic.Summary, cols int) string {
	wide := cols >= 100
	var b strings.Builder

	fmt.Fprintf(&b, "后端: %s   日期: %s   时区: %s\n", s.Backend, s.Day, s.TZ)

	// 概览（key:value，key 均为 4 个 CJK 字=8 列，天然对齐）
	b.WriteString("\n总览\n")
	b.WriteString("  " + padRightDisp("今日上传", 8) + " " + traffic.Human(s.Today.Rx) + "\n")
	b.WriteString("  " + padRightDisp("今日下载", 8) + " " + traffic.Human(s.Today.Tx) + "\n")
	b.WriteString("  " + padRightDisp("今日合计", 8) + " " + traffic.Human(s.Today.Rx+s.Today.Tx) + "\n")
	b.WriteString("  " + padRightDisp("累计上传", 8) + " " + traffic.Human(s.Total.Rx) + "\n")
	b.WriteString("  " + padRightDisp("累计下载", 8) + " " + traffic.Human(s.Total.Tx) + "\n")
	b.WriteString("  " + padRightDisp("累计合计", 8) + " " + traffic.Human(s.Total.Rx+s.Total.Tx) + "\n")

	// 节点流量
	b.WriteString("\n节点流量\n")
	if len(s.Nodes) == 0 {
		b.WriteString("  暂无节点流量数据\n")
		return b.String()
	}

	if wide {
		b.WriteString("  " + padRightDisp("节点", nameW) + " " +
			padLeftDisp("今日上传", numW) + " " + padLeftDisp("今日下载", numW) + " " +
			padLeftDisp("累计上传", numW) + " " + padLeftDisp("累计下载", numW) + "\n")
		for _, n := range s.Nodes {
			b.WriteString("  " + padRightDisp(truncDisp(n.Name, nameW), nameW) + " " +
				padLeftDisp(traffic.Human(n.Today.Rx), numW) + " " + padLeftDisp(traffic.Human(n.Today.Tx), numW) + " " +
				padLeftDisp(traffic.Human(n.Total.Rx), numW) + " " + padLeftDisp(traffic.Human(n.Total.Tx), numW) + "\n")
		}
		b.WriteString("  " + sepLine(nameW+1+numW*4+3) + "\n")
		b.WriteString("  " + padRightDisp("合计", nameW) + " " +
			padLeftDisp(traffic.Human(s.Today.Rx), numW) + " " + padLeftDisp(traffic.Human(s.Today.Tx), numW) + " " +
			padLeftDisp(traffic.Human(s.Total.Rx), numW) + " " + padLeftDisp(traffic.Human(s.Total.Tx), numW) + "\n")
	} else {
		b.WriteString("  " + padRightDisp("节点", nameW) + " " +
			padLeftDisp("今日上传", numW) + " " + padLeftDisp("今日下载", numW) + "\n")
		for _, n := range s.Nodes {
			b.WriteString("  " + padRightDisp(truncDisp(n.Name, nameW), nameW) + " " +
				padLeftDisp(traffic.Human(n.Today.Rx), numW) + " " + padLeftDisp(traffic.Human(n.Today.Tx), numW) + "\n")
		}
		b.WriteString("  " + sepLine(nameW+1+numW*2+1) + "\n")
		b.WriteString("  " + padRightDisp("合计", nameW) + " " +
			padLeftDisp(traffic.Human(s.Today.Rx), numW) + " " + padLeftDisp(traffic.Human(s.Today.Tx), numW) + "\n")
	}
	return b.String()
}

// renderDaily 渲染「最近 N 天」表格。cols 为终端列宽；窄布局省略「合计」列。
func renderDaily(rows []traffic.DailyRow, cols int) string {
	wide := cols >= 100
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("  暂无历史流量数据\n")
		return b.String()
	}
	if wide {
		b.WriteString("  " + padRightDisp("日期", dateW) + " " +
			padLeftDisp("上传", numW) + " " + padLeftDisp("下载", numW) + " " +
			padLeftDisp("合计", numW) + "\n")
		for _, r := range rows {
			b.WriteString("  " + padRightDisp(r.Day, dateW) + " " +
				padLeftDisp(traffic.Human(r.Rx), numW) + " " + padLeftDisp(traffic.Human(r.Tx), numW) + " " +
				padLeftDisp(traffic.Human(r.Rx+r.Tx), numW) + "\n")
		}
	} else {
		b.WriteString("  " + padRightDisp("日期", dateW) + " " +
			padLeftDisp("上传", numW) + " " + padLeftDisp("下载", numW) + "\n")
		for _, r := range rows {
			b.WriteString("  " + padRightDisp(r.Day, dateW) + " " +
				padLeftDisp(traffic.Human(r.Rx), numW) + " " + padLeftDisp(traffic.Human(r.Tx), numW) + "\n")
		}
	}
	return b.String()
}
