package notify

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ReplayOptions 控制恢复行为。
type ReplayOptions struct {
	// DryRun 只解析与统计，不写库。
	DryRun bool
	// Overwrite 直接覆盖既有值；默认取 MAX(旧值, 新值)。
	Overwrite bool
	// OnlyMissing 只填充 daily 表中不存在的日期（最保守）。
	OnlyMissing bool
}

// ReplayDayInfo 描述某一天的恢复来源与行数。
type ReplayDayInfo struct {
	Day    string
	Source string // "daily" 或 "hb"
	Rows   int
}

// ReplayResult 汇总一次恢复的结果。
type ReplayResult struct {
	Days      int
	FromDaily int
	FromHB    int
	Skipped   int
	DayInfos  []ReplayDayInfo
	Errors    []string
}

// normalizeReplayInput 把输入规整成"含 SBX1 行的纯文本"。
//
// 输入通常是从 IM 复制出来的纯文本，但也可能是通用 webhook 接收方落盘的
// JSONL（每行一个 payload 对象）。后者里 SBX1 行是被 JSON 转义过的字符串，
// 直接扫描文本会解析失败并报"跳过损坏片段"——用户会看到"解析出 0 天"
// 却完全不知道为什么。这里先识别并取出 replay 字段。
//
// 只取 replay 而不额外取 text：text 是给人读的，且两者可能包含同一行，
// 重复的行会被 MergeRecords 判为"分片重复"而拒绝整组。
func normalizeReplayInput(text string) string {
	if !strings.Contains(text, "{") {
		return text
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			out = append(out, line)
			continue
		}
		var obj struct {
			Replay []string `json:"replay"`
			Text   string   `json:"text"`
		}
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			out = append(out, line)
			continue
		}
		switch {
		case len(obj.Replay) > 0:
			out = append(out, obj.Replay...)
		case strings.Contains(obj.Text, ReplayPrefix):
			// 兼容手工构造或第三方实现只给了 text 的 payload。
			out = append(out, obj.Text)
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// SQL 常量。
//
// 默认取 MAX(旧值, 新值)：推送中的计数是单调递增的，取较大者更安全。
// 注意 UPSERT 里未限定的列名指原表当前值，excluded.* 指本次要写入的值
// （与 internal/traffic 的 upsert 写法保持一致）。
const (
	sqlReplayMax = "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?) " +
		"ON CONFLICT(day,scope) DO UPDATE SET rx=MAX(rx,excluded.rx), tx=MAX(tx,excluded.tx), " +
		"rx_pkts=MAX(rx_pkts,excluded.rx_pkts), tx_pkts=MAX(tx_pkts,excluded.tx_pkts)"
	sqlReplayOverwrite = "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?) " +
		"ON CONFLICT(day,scope) DO UPDATE SET rx=excluded.rx, tx=excluded.tx, " +
		"rx_pkts=excluded.rx_pkts, tx_pkts=excluded.tx_pkts"
	sqlReplayIgnore = "INSERT OR IGNORE INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)"
)

// ReplayInto 从聊天记录文本里提取可回放数据并写入 daily 表。
//
// 三条硬约束：
//   - **绝不写 totals**：累计无法从日数据反推，伪造一个"看起来对"的累计
//     比留空更危险。恢复后由使用者自行决定如何处理累计。
//   - 心跳来源的数据只写入 RecoveredScopeTotal，绝不冒充 node:<id> ——
//     否则会被面板的 `scope LIKE 'node:%'` 聚合误计，让日总量凭空翻倍。
//   - 同一个日期优先用日结（终值）；只有缺失日结时才用该日时刻最新的心跳。
func ReplayInto(db *sql.DB, text string, opt ReplayOptions) (*ReplayResult, error) {
	recs, skipped := ExtractReplays(normalizeReplayInput(text))
	merged, mergeErrs := MergeRecords(recs)

	res := &ReplayResult{Skipped: skipped, Errors: mergeErrs}

	type dayData struct {
		daily *ReplayRecord
		hb    *ReplayRecord
	}
	byDay := map[string]*dayData{}
	for i := range merged {
		r := merged[i]
		d := byDay[r.D]
		if d == nil {
			d = &dayData{}
			byDay[r.D] = d
		}
		switch r.K {
		case KindDaily:
			// 同日多条日结取最后一条（合并后通常只有一条）。
			d.daily = &merged[i]
		case KindHeartbeat:
			// 心跳是进行中值：取时刻最新的一条，它最接近当日完整量。
			if d.hb == nil || r.T > d.hb.T {
				d.hb = &merged[i]
			}
		}
	}

	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)

	q := sqlReplayMax
	switch {
	case opt.Overwrite:
		q = sqlReplayOverwrite
	case opt.OnlyMissing:
		q = sqlReplayIgnore
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, day := range days {
		d := byDay[day]
		var rows []ReplayRow
		source := ""
		switch {
		case d.daily != nil:
			rows, source = d.daily.R, "daily"
		case d.hb != nil:
			rows, source = d.hb.R, "hb"
		default:
			continue
		}

		applied := 0
		for _, row := range rows {
			var scope string
			if source == "hb" {
				// 心跳只有总计：写入保留 scope，不与 node:% 口径混淆。
				scope = RecoveredScopeTotal
			} else {
				scope = row.Scope
				if !strings.HasPrefix(scope, nodeScopePrefix) {
					// 日结里出现非节点 scope 说明数据可疑，拒绝写入。
					res.Errors = append(res.Errors,
						fmt.Sprintf("%s: 跳过非法 scope %q", day, truncate(scope, 40)))
					continue
				}
			}
			if opt.DryRun {
				applied++
				continue
			}
			if _, err := tx.Exec(q, day, scope, row.Rx, row.Tx, row.RxPkts, row.TxPkts); err != nil {
				return nil, fmt.Errorf("写入 %s/%s 失败: %w", day, scope, err)
			}
			applied++
		}
		res.Days++
		if source == "daily" {
			res.FromDaily++
		} else {
			res.FromHB++
		}
		res.DayInfos = append(res.DayInfos, ReplayDayInfo{Day: day, Source: source, Rows: applied})
	}

	if opt.DryRun {
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}
