package notify

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// nodeScopePrefix 是 daily 表里节点流量行的 scope 前缀。
const nodeScopePrefix = "node:"

// Totals 是一组流量计数。
type Totals struct {
	Rx     int64
	Tx     int64
	RxPkts int64
	TxPkts int64
}

func (t *Totals) add(o Totals) {
	t.Rx += o.Rx
	t.Tx += o.Tx
	t.RxPkts += o.RxPkts
	t.TxPkts += o.TxPkts
}

// ByteTotal 返回上下行字节之和（用于排序与降级阈值判断）。
func (t Totals) ByteTotal() int64 { return t.Rx + t.Tx }

// NodeRow 是某个节点在某一天的流量与展示名。
type NodeRow struct {
	Scope string
	ID    string
	Name  string
	Totals

	// Deleted 表示该 scope 已不在 nodes.json 中（节点被删除，但历史流量仍在库中）。
	// 这类行必须照常推送——删节点不等于删历史数据。
	Deleted bool
}

// Snapshot 是一次推送所需的全部只读数据。
type Snapshot struct {
	// Kind 是 KindHeartbeat / KindDaily。
	Kind string
	// Day 是数据归属日（YYYY-MM-DD，面板时区）。
	Day string
	// Slot 是本次推送时刻。
	Slot time.Time
	// Totals 是 Day 当天各节点流量合计。
	Totals Totals
	// Grand 是 totals 表里各节点累计流量合计（即"累计"）。
	Grand Totals
	// Nodes 是当天的节点明细，按上下行字节之和降序。
	Nodes []NodeRow
	// Rate 是实时速率合计；采集器不可用或速率未知时为 nil。
	Rate *traffic.Rate
	// Healthy 表示采集器最近一轮是否正常。
	Healthy bool
	// LastOK 是最后一次成功采集的 Unix 秒；0 表示未知。
	LastOK int64
	// LagSec 是数据滞后秒数（now - 最后一次成功采集）；无数据时为 -1。
	LagSec int64
	// Pending 是尚未成功送达的日结日期（用于在心跳里补偿）。
	Pending []string
}

// HHMM 返回槽位时刻的 HH:MM。
func (s *Snapshot) HHMM() string { return s.Slot.Format("15:04") }

// NodeCount 返回当天有流量记录的节点数。
func (s *Snapshot) NodeCount() int { return len(s.Nodes) }

// Title 返回消息首行的时间标识。
func (s *Snapshot) Title() string {
	if s.Kind == KindDaily {
		return s.Day
	}
	return s.Day[5:] + " " + s.HHMM() // MM-DD HH:MM
}

// replayRows 生成本次推送给出的可回放行。
//
// 心跳只给一行总计（scope 用保留字面量 "total"），长度恒定、永不超限；
// 日结给出逐节点终值，用于灾后重建。
func (s *Snapshot) replayRows() []ReplayRow {
	if s.Kind == KindHeartbeat {
		return []ReplayRow{{
			Scope:  ReplayScopeTotal,
			Rx:     s.Totals.Rx,
			Tx:     s.Totals.Tx,
			RxPkts: s.Totals.RxPkts,
			TxPkts: s.Totals.TxPkts,
		}}
	}
	rows := make([]ReplayRow, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		rows = append(rows, ReplayRow{
			Scope:  n.Scope,
			Rx:     n.Rx,
			Tx:     n.Tx,
			RxPkts: n.RxPkts,
			TxPkts: n.TxPkts,
		})
	}
	return rows
}

// BuildSnapshot 组装一次推送的数据快照。
//
// 全程只读：仅 SELECT daily / totals，并读取 nodes.json 用于展示名。
// nodes.json 走宽松读取——它损坏时推送不该整体失败（节点名退化为 id 即可），
// 这与采集路径必须用 LoadPanelNodesStrict 的口径不同，是有意为之。
func BuildSnapshot(cfg *config.Config, db *sql.DB, live traffic.LiveSource,
	kind, day string, slot time.Time) (*Snapshot, error) {

	// 时间一律按配置时区呈现。
	//
	// day 由调用方的 traffic.TodayAt(cfg.TZ, ...) 得出，若这里的时刻用服务器本地
	// 时区格式化，跨时区部署下两者会互相矛盾——例如服务器 TZ=UTC、配置
	// tz=Asia/Shanghai 时，会出现"日期已是 10-13、时刻却显示 10-12 的 16:00"。
	slot = slot.In(traffic.Location(cfg.TZ))

	rows, err := traffic.QDailyByDay(db, day)
	if err != nil {
		return nil, fmt.Errorf("查询 daily 失败: %w", err)
	}
	totals, err := traffic.QTotals(db)
	if err != nil {
		return nil, fmt.Errorf("查询 totals 失败: %w", err)
	}

	nameByID := map[string]string{}
	for _, n := range nodes.LoadPanelNodes(cfg.NodesFile) {
		if id := nodes.IDString(n); id != "" {
			nameByID[id] = nodes.DisplayName(n)
		}
	}

	s := &Snapshot{Kind: kind, Day: day, Slot: slot, LagSec: -1}

	// 以 daily 表的 scope 为准（而不是以 nodes.json 为准）：
	// 这样已删除节点的历史流量同样会被推送出去，不会因为"节点没了"就丢掉数据。
	for scope, r := range rows {
		if !strings.HasPrefix(scope, nodeScopePrefix) {
			continue // system 等非节点 scope 不参与推送（它是网卡总量，语义不同）
		}
		id := scope[len(nodeScopePrefix):]
		if id == "" {
			continue
		}
		nr := NodeRow{
			Scope:  scope,
			ID:     id,
			Totals: Totals{Rx: r.Rx, Tx: r.Tx, RxPkts: r.RxPkts, TxPkts: r.TxPkts},
		}
		if name, ok := nameByID[id]; ok {
			nr.Name = name
		} else {
			nr.Name = id
			nr.Deleted = true
		}
		s.Nodes = append(s.Nodes, nr)
		s.Totals.add(nr.Totals)
	}
	sort.Slice(s.Nodes, func(i, j int) bool {
		bi, bj := s.Nodes[i].ByteTotal(), s.Nodes[j].ByteTotal()
		if bi != bj {
			return bi > bj
		}
		return s.Nodes[i].Scope < s.Nodes[j].Scope
	})

	for scope, r := range totals {
		if strings.HasPrefix(scope, nodeScopePrefix) {
			s.Grand.add(Totals{Rx: r.Rx, Tx: r.Tx, RxPkts: r.RxPkts, TxPkts: r.TxPkts})
		}
	}

	if live != nil {
		s.Healthy = true
		st := live.StatusLite()
		if st.Error != "" {
			s.Healthy = false
		}
		if st.LastOK > 0 {
			s.LastOK = st.LastOK
			s.LagSec = slot.Unix() - st.LastOK
			if s.LagSec < 0 {
				s.LagSec = 0
			}
		}
		if rs, ok := live.(traffic.RateSource); ok {
			if rates, ready, err := rs.RateSnapshot(); err == nil && ready {
				var total traffic.Rate
				for k, v := range rates {
					if strings.HasPrefix(k, nodeScopePrefix) {
						total.Rx += v.Rx
						total.Tx += v.Tx
					}
				}
				s.Rate = &total
			}
		}
	}
	return s, nil
}
