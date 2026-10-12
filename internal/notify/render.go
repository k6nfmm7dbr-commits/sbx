package notify

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// Capabilities 描述一个渠道的单条容量。
//
// 两种计量口径必须分开：Telegram 按 UTF-16 code unit（4096），企业微信按字节
// （text 2048 / markdown 4096）。若统一按字节算，中文消息会被过度保守地切成多片。
type Capabilities struct {
	// MaxBytes 是字节上限（0 = 不限）。
	MaxBytes int
	// MaxUTF16 是 UTF-16 code unit 上限（0 = 不限）。
	MaxUTF16 int
}

// Fits 判断文本是否在容量内。
func (c Capabilities) Fits(s string) bool {
	if c.MaxBytes > 0 && len(s) > c.MaxBytes {
		return false
	}
	if c.MaxUTF16 > 0 && utf16Len(s) > c.MaxUTF16 {
		return false
	}
	return true
}

// utf16Len 按 UTF-16 code unit 计数（Telegram 的口径）。
// 不能用 len(s)：中文与 emoji 在 UTF-8 下占 3–4 字节，却只占 1–2 个 UTF-16 单元。
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// clipUTF16 把字符串裁剪到不超过 n 个 UTF-16 单元（用于推送服务的标题长度上限）。
// 逐字符回退而不是切片，避免切碎多字节字符。
func clipUTF16(s string, n int) string {
	r := []rune(s)
	for len(r) > 0 && utf16Len(string(r)) > n {
		r = r[:len(r)-1]
	}
	return string(r)
}

// 渲染降级级别：容量不够时逐级裁剪正文，但**绝不动可回放数据**。
const (
	levelFull      = iota // 合计 + 全部节点明细
	levelFoldNodes        // 明细只保留前 nodeDetailTopN 个，其余合并为一行
	levelNoNodes          // 无明细，只有合计
	levelMinimal          // 仅一句提示（可回放数据仍然完整保留）
)

// nodeDetailTopN 是折叠档保留的明细行数。
const nodeDetailTopN = 20

// Render 渲染某个渠道要发送的文本块。
//
// 返回：文本块列表（可能多块，需逐块发送）、以及因单行本身超长而无法纳入备份的行。
// dropped 必须由调用方上报——静默丢数据是不可接受的。
//
// 约定：可回放行永不被截断。装不下时降级正文；正文降到最简仍装不下时，
// 该块就只包含可回放行。
func Render(s *Snapshot, caps Capabilities) (blocks []string, dropped []ReplayRow) {
	shards, dropped := ShardRows(s.Kind, s.Day, s.HHMM(), s.replayRows(), caps.Fits)

	// 正文逐级降级，直到装得下。
	body := ""
	for _, level := range []int{levelFull, levelFoldNodes, levelNoNodes, levelMinimal} {
		b := renderBody(s, level)
		if caps.Fits(b) {
			body = b
			break
		}
	}

	if len(shards) == 0 {
		if body != "" {
			blocks = append(blocks, body)
		}
		return blocks, dropped
	}

	// 第一块尽量把正文与第一个分片合在一起（人读体验更好）；
	// 合起来超限就拆成两块。
	head := joinBodyReplay(body, EncodeReplay(shards[0]))
	if caps.Fits(head) {
		blocks = append(blocks, head)
	} else {
		if body != "" {
			blocks = append(blocks, body)
		}
		blocks = append(blocks, EncodeReplay(shards[0]))
	}
	// 后续分片各自成块：每片都是独立合法的 SBX1 行，可单独解析。
	// 刻意不加"第 x/y 片"提示行——它会计入容量，而分片已承担容量压力。
	for _, sh := range shards[1:] {
		blocks = append(blocks, EncodeReplay(sh))
	}
	return blocks, dropped
}

func joinBodyReplay(body, replay string) string {
	if body == "" {
		return replay
	}
	return body + "\n\n" + replay
}

func renderBody(s *Snapshot, level int) string {
	if level == levelMinimal {
		if s.Kind == KindDaily {
			return "📊 SBX 日报 " + s.Day + "（正文过长，仅保留可回放数据）"
		}
		return "🟢 SBX 流量（正文过长，仅保留可回放数据）"
	}
	if s.Kind == KindDaily {
		return renderDailyBody(s, level)
	}
	return renderHeartbeatBody(s, level)
}

// renderHeartbeatBody 渲染心跳正文。
//
// 采集异常时只给状态位与滞后时长，**不写内部错误原文**——
// 推送会把内容留在第三方服务器上，系统错误细节不该外流。
func renderHeartbeatBody(s *Snapshot, level int) string {
	var b strings.Builder
	icon := "🟢"
	if !s.Healthy {
		icon = "🟡"
	}
	b.WriteString(icon + " SBX 流量 " + s.Title())
	if !s.Healthy {
		b.WriteString("  ⚠ 采集异常")
	}
	b.WriteString("\n今日 " + bytesLine(s.Totals))

	if level == levelFull && s.Rate != nil {
		b.WriteString("\n速率 " + rateLine(*s.Rate))
	}
	if !s.Healthy && s.LastOK > 0 {
		b.WriteString("\n" + lagLine(s))
	}
	b.WriteString(fmt.Sprintf("\n%d 个节点", s.NodeCount()))
	if len(s.Pending) > 0 {
		b.WriteString(fmt.Sprintf("\n⚠ 有 %d 天日结未送达，已随本条补发", len(s.Pending)))
	}
	return b.String()
}

// renderDailyBody 渲染日结正文。
func renderDailyBody(s *Snapshot, level int) string {
	var b strings.Builder
	b.WriteString("📊 SBX 日报 " + s.Day + "（已冻结）\n")
	b.WriteString("合计 " + bytesLine(s.Totals) + "\n")
	b.WriteString("累计 " + traffic.Human(s.Grand.ByteTotal()) + "\n")
	b.WriteString(fmt.Sprintf("%d 个节点", s.NodeCount()))

	switch level {
	case levelFull:
		if len(s.Nodes) > 0 {
			b.WriteString("\n")
			for _, n := range s.Nodes {
				b.WriteString("\n" + nodeLine(n))
			}
		}
	case levelFoldNodes:
		top := s.Nodes
		if len(top) > nodeDetailTopN {
			top = top[:nodeDetailTopN]
		}
		if len(top) > 0 {
			b.WriteString("\n")
			for _, n := range top {
				b.WriteString("\n" + nodeLine(n))
			}
		}
		if rest := len(s.Nodes) - len(top); rest > 0 {
			var r Totals
			for _, n := range s.Nodes[len(top):] {
				r.add(n.Totals)
			}
			b.WriteString(fmt.Sprintf("\n（其余 %d 个节点合计 %s）", rest, traffic.Human(r.ByteTotal())))
		}
	}
	return b.String()
}

func nodeLine(n NodeRow) string {
	name := n.Name
	if n.Deleted {
		// 节点已删除但历史流量仍在：必须照常展示，并明确标注。
		name += "(已删除)"
	}
	return fmt.Sprintf("%s  %s ↑%s ↓%s",
		name, traffic.Human(n.ByteTotal()), traffic.Human(n.Rx), traffic.Human(n.Tx))
}

// bytesLine 按 README 的口径渲染：rx = 服务器接收(用户上传) = ↑，tx = 服务器发送(用户下载) = ↓。
func bytesLine(t Totals) string {
	return fmt.Sprintf("%s ↑%s ↓%s",
		traffic.Human(t.ByteTotal()), traffic.Human(t.Rx), traffic.Human(t.Tx))
}

func rateLine(r traffic.Rate) string {
	return fmt.Sprintf("↑%s/s ↓%s/s", traffic.Human(int64(r.Rx)), traffic.Human(int64(r.Tx)))
}

func lagLine(s *Snapshot) string {
	if s.LagSec >= 60 {
		at := time.Unix(s.LastOK, 0).Format("01-02 15:04")
		return fmt.Sprintf("数据时间 %s（已滞后 %d 分钟）", at, s.LagSec/60)
	}
	return fmt.Sprintf("数据已滞后 %d 秒", s.LagSec)
}
