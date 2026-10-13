package notify

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// 快照类型。
const (
	// KindDaily 是每日日报：主体为已冻结的前一日，历史不含当天。
	KindDaily = "daily"
	// KindRecover 是异常重启补推：主体为当天至今，历史含当天（标记进行中）。
	KindRecover = "recover"
)

// maxHistoryDays 是单次查询的历史天上限（约 11 年）。
// 它只用于给 SQL 的 LIMIT 一个确定值，不参与展示逻辑。
const maxHistoryDays = 4000

// Totals 是一组流量计数。
type Totals struct {
	Rx     int64
	Tx     int64
	RxPkts int64
	TxPkts int64
}

// ByteTotal 返回上下行字节之和。
func (t Totals) ByteTotal() int64 { return t.Rx + t.Tx }

// DayRow 是某一天的流量合计。
type DayRow struct {
	// Day 是 YYYY-MM-DD（面板时区）。
	Day string
	Totals
	// InProgress 表示这是当天尚未结束的数据，仅补推场景会出现。
	InProgress bool
}

// Snapshot 是一次推送所需的全部只读数据。
type Snapshot struct {
	// Kind 是 KindDaily / KindRecover。
	Kind string
	// Day 是卡片主体对应的日期：日报为前一日，补推为当天。
	Day string
	// Slot 是生成时刻（面板时区）。
	Slot time.Time
	// Focus 是 Day 当天的合计，供顶部三栏使用；当天无数据时为零值。
	Focus Totals
	// Days 是历史每日合计，按日期**倒序**（新 → 旧）。
	Days []DayRow
	// Grand 是 Days 的累计。
	Grand Totals
	// Test 为 true 时标题会标注 [测试]，避免自检消息与真实日报混淆。
	Test bool
}

// HistoryStats 返回历史天数与累计字节，供 CLI 摘要展示。只读。
func HistoryStats(db *sql.DB) (int, int64, error) {
	rows, err := traffic.QDaily(db, maxHistoryDays, "")
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, r := range rows {
		total += r.Rx + r.Tx
	}
	return len(rows), total, nil
}

// BuildSnapshot 组装一次推送的数据快照。
//
// 全程只读：只 SELECT daily（scope 聚合到 node:*）。
// 刻意不再读 totals 表：那张表是"各节点累计"，无法按天拆分，
// 与"每天一行历史"的口径不一致，放进来只会让人对不上账。
func BuildSnapshot(cfg *config.Config, db *sql.DB, kind, day string, slot time.Time) (*Snapshot, error) {
	// 时间一律按配置时区呈现，避免跨时区部署下"日期与时刻互相矛盾"。
	slot = slot.In(traffic.Location(cfg.TZ))

	rows, err := traffic.QDaily(db, maxHistoryDays, "")
	if err != nil {
		return nil, fmt.Errorf("查询每日流量失败: %w", err)
	}

	s := &Snapshot{Kind: kind, Day: day, Slot: slot}

	// QDaily 返回升序（旧 → 新），这里反向遍历得到"新 → 旧"的展示顺序。
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		// 只保留不晚于主体的日期：日报因此天然排除"刚开始的今天"。
		if r.Day > day {
			continue
		}
		dr := DayRow{
			Day:    r.Day,
			Totals: Totals{Rx: r.Rx, Tx: r.Tx, RxPkts: r.RxPkts, TxPkts: r.TxPkts},
		}
		if kind == KindRecover && r.Day == day {
			dr.InProgress = true
		}
		s.Days = append(s.Days, dr)
		s.Grand.Rx += dr.Rx
		s.Grand.Tx += dr.Tx
		s.Grand.RxPkts += dr.RxPkts
		s.Grand.TxPkts += dr.TxPkts
		if r.Day == day {
			s.Focus = dr.Totals
		}
	}
	return s, nil
}
