package notify

import (
	"strings"
	"testing"
)

func sampleRows(n int) []ReplayRow {
	rows := make([]ReplayRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, ReplayRow{
			Scope:  "node:" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Rx:     int64(i+1) * 1024,
			Tx:     int64(i+1) * 512,
			RxPkts: int64(i * 3),
			TxPkts: int64(i * 2),
		})
	}
	return rows
}

func TestReplayRoundTrip(t *testing.T) {
	rec := ReplayRecord{
		K: KindDaily, D: "2026-10-11", I: 0, N: 1,
		R: sampleRows(3),
	}
	line := EncodeReplay(rec)
	if !strings.HasPrefix(line, ReplayPrefix) {
		t.Fatalf("缺少前缀: %s", line)
	}
	if strings.ContainsAny(line, "\n\r") {
		t.Fatalf("可回放行必须是单行: %q", line)
	}
	got, err := ParseReplayLine(line)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.V != 1 || got.K != KindDaily || got.D != "2026-10-11" {
		t.Errorf("头部字段不符: %+v", got)
	}
	if len(got.R) != len(rec.R) {
		t.Fatalf("行数不符: %d != %d", len(got.R), len(rec.R))
	}
	for i := range rec.R {
		if got.R[i] != rec.R[i] {
			t.Errorf("第 %d 行不符:\n  got  %+v\n  want %+v", i, got.R[i], rec.R[i])
		}
	}
}

// TestReplayRoundTripHostileScopes 覆盖"节点 id 是任意字符串"这一事实：
// scope 可能含引号、反斜杠、分隔符、emoji 甚至换行。用 JSON 编码的意义就在这里——
// 任何自定义分隔符方案都会在这些输入上崩掉。
func TestReplayRoundTripHostileScopes(t *testing.T) {
	scopes := []string{
		`node:plain`,
		`node:has"quote`,
		`node:has\backslash`,
		`node:has|pipe,and,comma`,
		`node:emoji-🚀-ok`,
		"node:embed\nnewline",
		"node:embed\ttab",
		`node:${dollar}and%percent`,
		"node: 前后有空格 ",
	}
	rows := make([]ReplayRow, 0, len(scopes))
	for i, s := range scopes {
		rows = append(rows, ReplayRow{Scope: s, Rx: int64(i) + 1, Tx: 7, RxPkts: 2, TxPkts: 3})
	}
	line := EncodeReplay(ReplayRecord{K: KindDaily, D: "2026-10-11", I: 0, N: 1, R: rows})

	if strings.ContainsAny(line, "\n\r") {
		t.Fatalf("含换行的 scope 必须被转义成单行: %q", line)
	}
	got, err := ParseReplayLine(line)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got.R) != len(rows) {
		t.Fatalf("行数不符: %d != %d", len(got.R), len(rows))
	}
	for i := range rows {
		if got.R[i].Scope != rows[i].Scope {
			t.Errorf("scope 往返不一致:\n  got  %q\n  want %q", got.R[i].Scope, rows[i].Scope)
		}
	}
}

func TestParseReplayRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"缺前缀", `{"v":1,"k":"daily","d":"2026-10-11","i":0,"n":1,"r":[]}`},
		{"非 JSON", ReplayPrefix + "not json"},
		{"版本不符", ReplayPrefix + `{"v":2,"k":"daily","d":"2026-10-11","i":0,"n":1,"r":[]}`},
		{"未知事件", ReplayPrefix + `{"v":1,"k":"weekly","d":"2026-10-11","i":0,"n":1,"r":[]}`},
		{"日期非法", ReplayPrefix + `{"v":1,"k":"daily","d":"2026/10/11","i":0,"n":1,"r":[]}`},
		{"分片序号越界", ReplayPrefix + `{"v":1,"k":"daily","d":"2026-10-11","i":5,"n":1,"r":[]}`},
		{"行长度不足", ReplayPrefix + `{"v":1,"k":"daily","d":"2026-10-11","i":0,"n":1,"r":[["a",1,2]]}`},
		{"负数流量", ReplayPrefix + `{"v":1,"k":"daily","d":"2026-10-11","i":0,"n":1,"r":[["a",-1,2,3,4]]}`},
		{"小数流量", ReplayPrefix + `{"v":1,"k":"daily","d":"2026-10-11","i":0,"n":1,"r":[["a",1.5,2,3,4]]}`},
		{"尾部多余内容", ReplayPrefix + `{"v":1,"k":"daily","d":"2026-10-11","i":0,"n":1,"r":[]} trailing`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseReplayLine(c.in); err == nil {
				t.Fatalf("应拒绝: %s", c.in)
			}
		})
	}
}

func TestShardRowsCapacityAndCompleteness(t *testing.T) {
	fits := func(s string) bool { return len(s) <= 400 }
	rows := sampleRows(40)

	shards, dropped := ShardRows(KindDaily, "2026-10-11", "", rows, fits)
	if len(dropped) != 0 {
		t.Fatalf("不应有行被丢弃: %d", len(dropped))
	}
	if len(shards) < 2 {
		t.Fatalf("40 行在 400 字节容量下应分成多片, 实得 %d 片", len(shards))
	}
	total := 0
	for i, sh := range shards {
		line := EncodeReplay(sh)
		if !fits(line) {
			t.Errorf("第 %d 片超出容量: %d 字节", i, len(line))
		}
		if sh.I != i || sh.N != len(shards) {
			t.Errorf("第 %d 片的 i/n 不符: %d/%d", i, sh.I, sh.N)
		}
		// 每片必须是独立可解析的合法记录（消息可能乱序或丢片）。
		if _, err := ParseReplayLine(line); err != nil {
			t.Errorf("第 %d 片无法独立解析: %v", i, err)
		}
		total += len(sh.R)
	}
	if total != len(rows) {
		t.Errorf("分片丢行: %d != %d", total, len(rows))
	}

	// 重组后应与原行序完全一致。
	merged, errs := MergeRecords(shards)
	if len(errs) != 0 {
		t.Fatalf("重组报错: %v", errs)
	}
	if len(merged) != 1 || len(merged[0].R) != len(rows) {
		t.Fatalf("重组结果不符: %+v", merged)
	}
	for i := range rows {
		if merged[0].R[i] != rows[i] {
			t.Errorf("重组后第 %d 行不一致", i)
			break
		}
	}
}

func TestShardRowsReportsOversizeRow(t *testing.T) {
	fits := func(s string) bool { return len(s) <= 120 }
	rows := []ReplayRow{
		{Scope: "node:small", Rx: 1},
		{Scope: "node:" + strings.Repeat("x", 500), Rx: 2}, // 单行本身就装不下
		{Scope: "node:another", Rx: 3},
	}
	shards, dropped := ShardRows(KindDaily, "2026-10-11", "", rows, fits)
	if len(dropped) != 1 {
		t.Fatalf("应报告 1 行超长被丢弃, 实得 %d", len(dropped))
	}
	if dropped[0].Rx != 2 {
		t.Errorf("丢弃的行不对: %+v", dropped[0])
	}
	kept := 0
	for _, sh := range shards {
		kept += len(sh.R)
	}
	if kept != 2 {
		t.Errorf("应保留 2 行, 实得 %d", kept)
	}
}

func TestExtractReplaysHandlesWrappedLines(t *testing.T) {
	line := EncodeReplay(ReplayRecord{
		K: KindDaily, D: "2026-10-11", I: 0, N: 1, R: sampleRows(5),
	})
	// 模拟 IM 客户端折行 / 用户复制时带入的真实换行。
	mid := len(line) / 2
	broken := line[:mid] + "\n" + line[mid:]

	text := "这是一条普通消息\n" + broken + "\n后面还有别的内容\n"
	recs, skipped := ExtractReplays(text)
	if skipped != 0 {
		t.Errorf("折行不应被计为损坏片段: skipped=%d", skipped)
	}
	if len(recs) != 1 {
		t.Fatalf("应提取出 1 条记录, 实得 %d", len(recs))
	}
	if len(recs[0].R) != 5 {
		t.Errorf("折行拼接后行数不符: %d", len(recs[0].R))
	}
}

func TestExtractReplaysStopsAtNextRecord(t *testing.T) {
	a := EncodeReplay(ReplayRecord{K: KindDaily, D: "2026-10-11", I: 0, N: 1, R: sampleRows(2)})
	b := EncodeReplay(ReplayRecord{K: KindDaily, D: "2026-10-12", I: 0, N: 1, R: sampleRows(3)})
	// a 被截断（模拟消息被截）后紧跟 b：不得把两条粘在一起。
	truncated := a[:len(a)/2]
	text := truncated + "\n" + b + "\n"

	recs, skipped := ExtractReplays(text)
	if skipped != 1 {
		t.Errorf("截断片段应计为 1 个损坏片段, 实得 %d", skipped)
	}
	if len(recs) != 1 {
		t.Fatalf("应只提取到后一条完整记录, 实得 %d", len(recs))
	}
	if recs[0].D != "2026-10-12" {
		t.Errorf("提取到的应是完整那条: %s", recs[0].D)
	}
}

func TestExtractReplaysMultipleInOneText(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 3; i++ {
		sb.WriteString(EncodeReplay(ReplayRecord{
			K: KindDaily, D: "2026-10-1" + string(rune('0'+i)), I: 0, N: 1, R: sampleRows(2),
		}))
		sb.WriteString("\n说明文字\n")
	}
	recs, skipped := ExtractReplays(sb.String())
	if len(recs) != 3 || skipped != 0 {
		t.Fatalf("应提取 3 条记录且无损坏, 实得 %d 条 / %d 损坏", len(recs), skipped)
	}
}

func TestMergeRecordsRejectsIncomplete(t *testing.T) {
	fits := func(s string) bool { return len(s) <= 300 }
	shards, _ := ShardRows(KindDaily, "2026-10-11", "", sampleRows(30), fits)
	if len(shards) < 3 {
		t.Skipf("样本未产生足够分片(%d)", len(shards))
	}
	// 丢掉中间一片。
	incomplete := append(append([]ReplayRecord{}, shards[:1]...), shards[2:]...)
	merged, errs := MergeRecords(incomplete)
	if len(merged) != 0 {
		t.Errorf("分片不完整时不得产出记录")
	}
	if len(errs) == 0 {
		t.Errorf("分片不完整必须报告错误")
	}
}

func TestMergeRecordsRejectsConflictingTotal(t *testing.T) {
	a := ReplayRecord{K: KindDaily, D: "2026-10-11", I: 0, N: 2, R: sampleRows(1)}
	b := ReplayRecord{K: KindDaily, D: "2026-10-11", I: 1, N: 5, R: sampleRows(1)}
	merged, errs := MergeRecords([]ReplayRecord{a, b})
	if len(merged) != 0 || len(errs) == 0 {
		t.Errorf("分片总数冲突应被拒绝: merged=%d errs=%v", len(merged), errs)
	}
}
