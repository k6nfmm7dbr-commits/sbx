package service

import (
	"os"
	"strconv"
	"strings"
)

// runeWidth 返回 rune 的终端显示宽度（近似 wcwidth，覆盖常用 CJK/全角/表情）。
// 用于 CLI 表格对齐——中文字符占 2 个终端列，不能用 len(string) 或 rune 数假设宽度。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || r == 0x7f:
		return 0 // 控制字符
	case r < 0x300:
		return 1
	case r >= 0x300 && r <= 0x36f: // 组合附加符
		return 0
	case r >= 0x1100 && r <= 0x115f: // Hangul Jamo
		return 2
	case r >= 0x2e80 && r <= 0x303e: // CJK 部首/标点
		return 2
	case r >= 0x3041 && r <= 0x33ff: // 假名/CJK 符号
		return 2
	case r >= 0x3400 && r <= 0x4dbf: // CJK 扩展 A
		return 2
	case r >= 0x4e00 && r <= 0x9fff: // CJK 统一表意
		return 2
	case r >= 0xa000 && r <= 0xa4cf: // 彝文
		return 2
	case r >= 0xac00 && r <= 0xd7a3: // Hangul 音节
		return 2
	case r >= 0xf900 && r <= 0xfaff: // CJK 兼容
		return 2
	case r >= 0xfe30 && r <= 0xfe4f: // CJK 兼容形式
		return 2
	case r >= 0xff00 && r <= 0xff60: // 全角
		return 2
	case r >= 0xffe0 && r <= 0xffe6:
		return 2
	case r >= 0x1f300 && r <= 0x1faff: // 表情/符号
		return 2
	case r >= 0x20000 && r <= 0x3fffd: // CJK 扩展 B+
		return 2
	default:
		return 1
	}
}

// dispWidth 返回字符串的终端显示宽度（列数）。
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// padRightDisp 用空格把 s 右补到 w 个显示列。
func padRightDisp(s string, w int) string {
	n := w - dispWidth(s)
	if n <= 0 {
		return s
	}
	return s + strings.Repeat(" ", n)
}

// padLeftDisp 用空格把 s 左补到 w 个显示列。
func padLeftDisp(s string, w int) string {
	n := w - dispWidth(s)
	if n <= 0 {
		return s
	}
	return strings.Repeat(" ", n) + s
}

// truncDisp 把 s 截断到最多 w 个显示列；超长时以 … 结尾（省略号占 1 列）。
// 仅影响展示，不改真实数据。
func truncDisp(s string, w int) string {
	if dispWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	target := w - 1
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := runeWidth(r)
		if used+rw > target {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	b.WriteRune('…')
	return b.String()
}

// terminalCols 读取终端列宽（COLUMNS 环境变量），失败/缺失时默认 80。
// 返回 0 表示无法确定（调用方按窄布局处理）。
func terminalCols() int {
	s := strings.TrimSpace(os.Getenv("COLUMNS"))
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
