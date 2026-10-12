package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func mkSnapshot(kind string, nodeCount int) *Snapshot {
	slot := time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC)
	s := &Snapshot{Kind: kind, Day: "2026-10-12", Slot: slot, Healthy: true}
	for i := 0; i < nodeCount; i++ {
		nr := NodeRow{
			Scope: fmt.Sprintf("node:test%02d", i),
			ID:    fmt.Sprintf("test%02d", i),
			Name:  fmt.Sprintf("Node-%02d", i),
			Totals: Totals{
				Rx:     int64(i+1) * (1 << 20),
				Tx:     int64(i+1) * (1 << 19),
				RxPkts: int64(i + 1),
				TxPkts: int64(i + 1),
			},
		}
		s.Nodes = append(s.Nodes, nr)
		s.Totals.add(nr.Totals)
	}
	return s
}

// TestHeartbeatReplayUsesReservedTotalScope 守住一个容易被"优化"掉的设计约束。
//
// 心跳若使用形如 node:* 的伪 scope，会被面板 `scope LIKE 'node:%'` 的日趋势聚合
// 匹配到——一旦恢复入库，日总量就会凭空翻倍。必须使用保留字面量 "total"。
func TestHeartbeatReplayUsesReservedTotalScope(t *testing.T) {
	s := mkSnapshot(KindHeartbeat, 5)
	rows := s.replayRows()
	if len(rows) != 1 {
		t.Fatalf("心跳应只产生一行总计, 实得 %d 行", len(rows))
	}
	if rows[0].Scope != ReplayScopeTotal {
		t.Fatalf("心跳 scope 必须是 %q, 实得 %q", ReplayScopeTotal, rows[0].Scope)
	}
	if strings.HasPrefix(rows[0].Scope, "node:") {
		t.Fatal("心跳不得使用 node: 前缀的 scope")
	}
	if rows[0].Rx != s.Totals.Rx || rows[0].Tx != s.Totals.Tx {
		t.Errorf("心跳总计应与节点合计一致: %+v vs %+v", rows[0], s.Totals)
	}
}

// TestRenderKeepsReplayDataIntact 断言"容量压力下正文可以缩水，可回放数据绝不缩水"。
func TestRenderKeepsReplayDataIntact(t *testing.T) {
	const nodes = 40
	s := mkSnapshot(KindDaily, nodes)
	capsList := []Capabilities{
		{MaxUTF16: 4096}, // Telegram
		{MaxBytes: 4096}, // 企业微信 markdown
		{MaxBytes: 2048}, // 企业微信 text
		{MaxBytes: 700},  // 极端受限
	}
	for _, caps := range capsList {
		blocks, dropped := Render(s, caps)
		if len(dropped) != 0 {
			t.Errorf("caps=%+v: 不应丢弃数据行, 实得 %d", caps, len(dropped))
		}
		for i, b := range blocks {
			if !caps.Fits(b) {
				t.Errorf("caps=%+v: 第 %d 块超出容量(%d)", caps, i, len(b))
			}
		}
		// 把所有块拼起来应能还原出完整的 40 行数据。
		recs, skipped := ExtractReplays(strings.Join(blocks, "\n"))
		if skipped != 0 {
			t.Errorf("caps=%+v: 出现 %d 个不可解析片段", caps, skipped)
		}
		merged, errs := MergeRecords(recs)
		if len(errs) != 0 {
			t.Errorf("caps=%+v: 重组报错 %v", caps, errs)
		}
		if len(merged) != 1 {
			t.Fatalf("caps=%+v: 应重组成 1 条记录, 实得 %d", caps, len(merged))
		}
		if len(merged[0].R) != nodes {
			t.Errorf("caps=%+v: 可回放数据不完整, %d 行 (期望 %d)", caps, len(merged[0].R), nodes)
		}
	}
}

func TestRenderFirstBlockCarriesReplay(t *testing.T) {
	s := mkSnapshot(KindHeartbeat, 3)
	blocks, _ := Render(s, Capabilities{MaxBytes: 2048})
	if len(blocks) == 0 {
		t.Fatal("应至少产出一个文本块")
	}
	if !strings.Contains(blocks[0], ReplayPrefix) {
		t.Error("第一个块应包含可回放行，否则人类可读的正文与备份会脱节")
	}
}

func TestRenderMarksDeletedNodes(t *testing.T) {
	s := mkSnapshot(KindDaily, 3)
	s.Nodes[0].Deleted = true
	blocks, _ := Render(s, Capabilities{MaxBytes: 4096})
	all := strings.Join(blocks, "\n")
	if !strings.Contains(all, "已删除") {
		t.Error("已从 nodes.json 删除的节点仍应展示其历史流量, 并标注(已删除)")
	}
}

func TestRenderDoesNotLeakInternalErrorText(t *testing.T) {
	s := mkSnapshot(KindHeartbeat, 2)
	s.Healthy = false
	s.LastOK = s.Slot.Unix() - 900
	s.LagSec = 900
	blocks, _ := Render(s, Capabilities{MaxBytes: 4096})
	all := strings.Join(blocks, "\n")
	if !strings.Contains(all, "采集异常") {
		t.Error("采集异常时应给出状态提示")
	}
	// 推送内容会留在第三方服务器上，系统错误细节不该外流。
	for _, banned := range []string{"/etc/", "permission denied", "no such file"} {
		if strings.Contains(all, banned) {
			t.Errorf("正文不应包含内部错误细节 %q", banned)
		}
	}
}

func TestRenderDailyHasFrozenMarker(t *testing.T) {
	s := mkSnapshot(KindDaily, 2)
	blocks, _ := Render(s, Capabilities{MaxBytes: 4096})
	if !strings.Contains(blocks[0], "已冻结") {
		t.Error("日结应标明数据已冻结（终值），否则用户无法区分终值与进行中值")
	}
}

func TestCapabilitiesMeasurement(t *testing.T) {
	// 10 个中文字符 = 10 个 UTF-16 单元，但占 30 字节。
	utf16Caps := Capabilities{MaxUTF16: 10}
	if !utf16Caps.Fits(strings.Repeat("中", 10)) {
		t.Error("10 个中文应恰好装进 10 个 UTF-16 单元")
	}
	if utf16Caps.Fits(strings.Repeat("中", 11)) {
		t.Error("11 个中文应超出 10 个 UTF-16 单元")
	}
	byteCaps := Capabilities{MaxBytes: 10}
	if byteCaps.Fits(strings.Repeat("中", 10)) {
		t.Error("按字节计量时 30 字节应超限——两种口径不能混用")
	}
}

func TestClipUTF16(t *testing.T) {
	if got := clipUTF16("abcdef", 3); got != "abc" {
		t.Errorf("应截到 3, 实得 %q", got)
	}
	if got := clipUTF16(strings.Repeat("中", 5), 2); got != "中中" {
		t.Errorf("中文应按单元截断, 实得 %q", got)
	}
	if got := clipUTF16("short", 100); got != "short" {
		t.Errorf("未超限时应原样返回, 实得 %q", got)
	}
}
