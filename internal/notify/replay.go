package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

// SBX1 行协议常量。
//
// 为什么用单行 JSON 而不是自定义分隔符：nodes.IDString 允许节点 id 是任意字符串
// （见 internal/nodes/store.go），scope 可能包含 | , " 甚至换行。任何自定义分隔符
// 方案都必须自带转义规则，容易出现"看起来能用、遇到特殊 id 就坏"的隐患。
// JSON 由标准库直接解析，无转义歧义。
const (
	// ReplayPrefix 用于从任意文本中识别可回放行。
	ReplayPrefix = "SBX1:"
	// replayVersion 是当前格式版本，写在每行的 "v" 字段。
	replayVersion = 1
	// KindHeartbeat / KindDaily 是行内的事件短名。
	KindHeartbeat = "hb"
	KindDaily     = "daily"

	// ReplayScopeTotal 是心跳使用的保留 scope。
	//
	// 必须与 daily 表的既有 scope 形态区分开：面板按 `scope LIKE 'node:%'` 聚合日趋势，
	// 若心跳使用形如 `node:*` 的伪 scope，恢复入库会让日总量凭空翻倍。
	// 用保留字面量 "total" 则天然不被任何既有查询匹配。
	ReplayScopeTotal = "total"

	// RecoveredScopeTotal 是恢复时把心跳数据写入 daily 表所用的 scope。
	// 同样不被 `LIKE 'node:%'` 匹配，因此不会干扰面板的节点聚合与 totals。
	RecoveredScopeTotal = "recovered:total"

	// maxJoinLines 是折行容错拼接的上限行数。
	// IM 客户端会视觉折行，用户手动复制时可能带入真实换行；
	// 拼接上限避免把一个坏片段和后续无关内容无限粘在一起。
	maxJoinLines = 8
)

// ReplayRow 是 daily 表的一行：[scope, rx, tx, rx_pkts, tx_pkts]。
type ReplayRow struct {
	Scope  string
	Rx     int64
	Tx     int64
	RxPkts int64
	TxPkts int64
}

// MarshalJSON 输出定长数组形式，兼顾紧凑与自描述。
func (r ReplayRow) MarshalJSON() ([]byte, error) {
	scope, err := fsx.MarshalCompact(r.Scope)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, len(scope)+48)
	buf = append(buf, '[')
	buf = append(buf, scope...)
	buf = append(buf, ',')
	buf = strconv.AppendInt(buf, r.Rx, 10)
	buf = append(buf, ',')
	buf = strconv.AppendInt(buf, r.Tx, 10)
	buf = append(buf, ',')
	buf = strconv.AppendInt(buf, r.RxPkts, 10)
	buf = append(buf, ',')
	buf = strconv.AppendInt(buf, r.TxPkts, 10)
	buf = append(buf, ']')
	return buf, nil
}

// UnmarshalJSON 解析定长数组，并对取值做合法性校验。
// 流量计数必须是非负整数：负数或小数说明数据已损坏，宁可报错也不入库。
func (r *ReplayRow) UnmarshalJSON(b []byte) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	if len(arr) != 5 {
		return fmt.Errorf("行应为 5 个元素 [scope,rx,tx,rx_pkts,tx_pkts], 实得 %d", len(arr))
	}
	if err := json.Unmarshal(arr[0], &r.Scope); err != nil {
		return fmt.Errorf("scope 非法: %w", err)
	}
	dst := [4]*int64{&r.Rx, &r.Tx, &r.RxPkts, &r.TxPkts}
	names := [4]string{"rx", "tx", "rx_pkts", "tx_pkts"}
	for i, p := range dst {
		var n json.Number
		if err := json.Unmarshal(arr[i+1], &n); err != nil {
			return fmt.Errorf("%s 必须是整数: %w", names[i], err)
		}
		v, err := n.Int64()
		if err != nil {
			return fmt.Errorf("%s 必须是整数: %w", names[i], err)
		}
		if v < 0 {
			return fmt.Errorf("%s 不能为负: %d", names[i], v)
		}
		*p = v
	}
	return nil
}

// ReplayRecord 是一条完整的可回放记录（可能只是某个日期的第 i/N 个分片）。
type ReplayRecord struct {
	V int         `json:"v"`
	K string      `json:"k"`
	D string      `json:"d"`
	T string      `json:"t,omitempty"`
	I int         `json:"i"`
	N int         `json:"n"`
	R []ReplayRow `json:"r"`
}

// EncodeReplay 渲染成可整行嵌入消息的文本。编码不会失败（字段均可序列化）；
// 极端情况下返回空串，调用方需按"空串即不追加"处理。
func EncodeReplay(rec ReplayRecord) string {
	rec.V = replayVersion
	if rec.R == nil {
		rec.R = []ReplayRow{}
	}
	b, err := fsx.MarshalCompact(rec)
	if err != nil {
		return ""
	}
	return ReplayPrefix + string(b)
}

// ParseReplayLine 解析单行。要求以 SBX1: 开头且尾部只有空白。
func ParseReplayLine(s string) (ReplayRecord, error) {
	var rec ReplayRecord
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, ReplayPrefix) {
		return rec, fmt.Errorf("不是 %s 行", ReplayPrefix)
	}
	dec := json.NewDecoder(strings.NewReader(s[len(ReplayPrefix):]))
	if err := dec.Decode(&rec); err != nil {
		return rec, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return rec, fmt.Errorf("尾部存在多余内容")
	}
	if err := validateRecord(rec); err != nil {
		return ReplayRecord{}, err
	}
	return rec, nil
}

func validateRecord(rec ReplayRecord) error {
	if rec.V != replayVersion {
		return fmt.Errorf("不支持的格式版本 %d", rec.V)
	}
	if rec.K != KindHeartbeat && rec.K != KindDaily {
		return fmt.Errorf("未知事件类型 %q", rec.K)
	}
	if !validDay(rec.D) {
		return fmt.Errorf("日期格式非法(应为 YYYY-MM-DD): %q", rec.D)
	}
	if rec.N < 1 {
		return fmt.Errorf("分片总数非法: %d", rec.N)
	}
	if rec.I < 0 || rec.I >= rec.N {
		return fmt.Errorf("分片序号越界: %d/%d", rec.I, rec.N)
	}
	return nil
}

// validDay 校验 YYYY-MM-DD 的字面形态（不校验日历合法性，由使用方按需处理）。
func validDay(d string) bool {
	if len(d) != 10 || d[4] != '-' || d[7] != '-' {
		return false
	}
	for i, c := range d {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ExtractReplays 从任意文本里提取所有可回放记录，返回记录与被跳过的坏片段数。
//
// 折行容错：若某个 SBX1: 片段单独解析失败，则把它与后续行拼接后重试（上限
// maxJoinLines 行）。拼接前会检查下一行是否包含新的 SBX1: 前缀——若有，
// 说明前一条是截断的坏数据，绝不能把两条不同记录粘在一起。
func ExtractReplays(text string) (recs []ReplayRecord, skipped int) {
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		idx := strings.Index(lines[i], ReplayPrefix)
		if idx < 0 {
			continue
		}
		cand := lines[i][idx:]
		ok := false
		for extra := 0; extra < maxJoinLines; extra++ {
			if rec, err := ParseReplayLine(cand); err == nil {
				recs = append(recs, rec)
				i += extra
				ok = true
				break
			}
			next := i + extra + 1
			if next >= len(lines) || strings.Contains(lines[next], ReplayPrefix) {
				break
			}
			cand += strings.TrimSpace(lines[next])
		}
		if !ok {
			skipped++
		}
	}
	return recs, skipped
}

// ShardRows 把行集切成若干不超过渠道容量的分片。
//
// fits 由调用方按渠道能力提供（Telegram 按 UTF-16 单元数，企业微信按字节数），
// 因此本函数不需要知道任何渠道细节。
//
// 返回：分片记录列表、以及"单行本身就装不下"因而被丢弃的行——
// 丢弃必须由调用方上报，绝不静默。
func ShardRows(kind, day, hhmm string, rows []ReplayRow, fits func(string) bool) ([]ReplayRecord, []ReplayRow) {
	var groups [][]ReplayRow
	var dropped []ReplayRow
	cur := []ReplayRow{}

	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, cur)
			cur = []ReplayRow{}
		}
	}
	measure := func(rs []ReplayRow) string {
		return EncodeReplay(ReplayRecord{K: kind, D: day, T: hhmm, I: 0, N: 1, R: rs})
	}

	for _, r := range rows {
		one := []ReplayRow{r}
		if !fits(measure(one)) {
			dropped = append(dropped, r)
			continue
		}
		cand := make([]ReplayRow, len(cur)+1)
		copy(cand, cur)
		cand[len(cur)] = r
		if !fits(measure(cand)) {
			flush()
			cur = one
			continue
		}
		cur = cand
	}
	flush()

	out := make([]ReplayRecord, 0, len(groups))
	for i, g := range groups {
		out = append(out, ReplayRecord{
			V: replayVersion, K: kind, D: day, T: hhmm,
			I: i, N: len(groups), R: g,
		})
	}
	return out, dropped
}

// MergeRecords 把同一天的多个分片重组成完整记录（恢复侧使用）。
// 返回重组后的记录与出错说明。分片缺失或不一致的组会被整体丢弃并计入错误，
// 绝不用"半个数据"去覆盖数据库。
func MergeRecords(recs []ReplayRecord) ([]ReplayRecord, []string) {
	type group struct {
		day   string
		parts map[int]ReplayRecord
		total int
	}
	keyOf := func(r ReplayRecord) string { return r.D + "|" + r.K + "|" + r.T }
	groups := map[string]*group{}
	var order []string

	for _, r := range recs {
		k := keyOf(r)
		g := groups[k]
		if g == nil {
			g = &group{day: r.D, parts: map[int]ReplayRecord{}, total: r.N}
			groups[k] = g
			order = append(order, k)
		}
		if g.total != r.N {
			return nil, []string{fmt.Sprintf("%s: 分片总数不一致(%d vs %d)", r.D, g.total, r.N)}
		}
		if _, dup := g.parts[r.I]; dup {
			return nil, []string{fmt.Sprintf("%s: 分片 %d 重复", r.D, r.I)}
		}
		g.parts[r.I] = r
	}

	sort.Strings(order)
	var out []ReplayRecord
	var errs []string
	for _, k := range order {
		g := groups[k]
		if len(g.parts) != g.total {
			errs = append(errs, fmt.Sprintf("%s: 分片不完整(%d/%d)，已跳过", g.day, len(g.parts), g.total))
			continue
		}
		merged := g.parts[0]
		merged.R = make([]ReplayRow, 0, 16)
		complete := true
		for i := 0; i < g.total; i++ {
			p, ok := g.parts[i]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: 缺少分片 %d，已跳过", g.day, i))
				complete = false
				break
			}
			merged.R = append(merged.R, p.R...)
		}
		if !complete {
			continue
		}
		merged.I, merged.N = 0, 1
		out = append(out, merged)
	}
	return out, errs
}
