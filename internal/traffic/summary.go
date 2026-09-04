package traffic

import (
	"database/sql"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// Counters 对齐旧 {"rx","tx","rx_pkts","tx_pkts"}。
type Counters struct {
	Rx     int64 `json:"rx"`
	Tx     int64 `json:"tx"`
	RxPkts int64 `json:"rx_pkts"`
	TxPkts int64 `json:"tx_pkts"`
}

// LiveSource 是 API 层获取采集器快照的最小接口（可为 nil，等价 collector=None）。
type LiveSource interface {
	Snapshot() Status
	BackendName() string
}

func countersOf(t TotalsRow) Counters {
	return Counters{Rx: t.Rx, Tx: t.Tx, RxPkts: t.RxPkts, TxPkts: t.TxPkts}
}

// resolveConns 复刻旧取值顺序：优先采集线程缓存；缓存尚未生成（首轮之前）
// 时才直接读 /proc。空 map ≠ nil：采集失败后的空结果不会被反复重算。
func resolveConns(st Status, list []nodes.Node) (map[string]connection.Conns, error) {
	if st.Conns != nil {
		return st.Conns, nil
	}
	res, err := connection.CountForNodes(list)
	if err != nil {
		return nil, err
	}
	return res.Conns, nil
}

func rateTotal(rates map[string]Rate) Rate {
	var total Rate
	for k, v := range rates {
		if strings.HasPrefix(k, "node:") {
			total.Rx += v.Rx
			total.Tx += v.Tx
		}
	}
	return total
}

// connsTotals 复刻 _conns_total：仅对非空值求和。
func connsTotals(conns map[string]connection.Conns) (tcpTotal, udpTotal int64) {
	for _, c := range conns {
		if c.TCP != nil {
			tcpTotal += int64(*c.TCP)
		}
		if c.UDP != nil {
			udpTotal += int64(*c.UDP)
		}
	}
	return
}

// SummaryNode 是 /api/summary 中单个节点条目。
type SummaryNode struct {
	ID       any      `json:"id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Port     any      `json:"port"`
	Today    Counters `json:"today"`
	Total    Counters `json:"total"`
	Rate     Rate     `json:"rate"`
	ConnsTCP *int     `json:"conns_tcp"`
	ConnsUDP *int     `json:"conns_udp"`
	// 策略字段（由 API 层填充；traffic 层不感知 policy 语义）。
	// 全部 omitempty：未启用策略的节点不输出这些键，保持旧 API shape 兼容。
	QuotaEnabled bool   `json:"quota_enabled,omitempty"`
	QuotaLimit   int64  `json:"quota_limit_bytes,omitempty"`
	QuotaUsed    int64  `json:"quota_used_bytes,omitempty"`
	QuotaState   string `json:"quota_state,omitempty"`
	IPLimitOn    bool   `json:"ip_limit_enabled,omitempty"`
	IPLimitMax   int    `json:"ip_limit_max,omitempty"`
	ActiveIPs    int    `json:"active_ip_count,omitempty"`
	IPLimitState string `json:"ip_limit_state,omitempty"`
}

// Summary 对齐 build_summary 返回结构。
type Summary struct {
	Now           int64         `json:"now"`
	Day           string        `json:"day"`
	TZ            string        `json:"tz"`
	Interval      int           `json:"interval"`
	Backend       string        `json:"backend"`
	Healthy       bool          `json:"healthy"`
	Error         string        `json:"error"`
	LastSample    int64         `json:"last_sample"`
	Nodes         []SummaryNode `json:"nodes"`
	Today         Counters      `json:"today"`
	Total         Counters      `json:"total"`
	SystemToday   Counters      `json:"system_today"`
	SystemTotal   Counters      `json:"system_total"`
	RateTotal     Rate          `json:"rate_total"`
	RateKnown     bool          `json:"rate_known"`
	ConnsTotal    int64         `json:"conns_total"`
	ConnsUDPTotal int64         `json:"conns_udp_total"`
	// PolicyError 是策略层最近一次 enforcement 错误（由 API 层填充）：
	// nft 规则应用失败等瞬态故障会在此呈现，便于用户在面板直接看到
	// 「阻断未生效」而不是误以为已被限制。
	PolicyError string `json:"policy_error,omitempty"`
}

// BuildSummary 汇总今日/累计/速率/连接数。src 为 nil 时不读采集器缓存
// （CLI show 场景），与旧实现 collector=None 行为一致。
func BuildSummary(cfg *config.Config, db *sql.DB, src LiveSource) (*Summary, error) {
	list := nodes.LoadPanelNodes(cfg.NodesFile)
	totals, err := QTotals(db)
	if err != nil {
		return nil, err
	}
	nowT := TimeNow()
	rates, err := QRate(db, cfg.Interval, nowT.Unix())
	if err != nil {
		return nil, err
	}

	var st Status
	hasCollector := src != nil
	if hasCollector {
		st = src.Snapshot()
	}
	conns, err := resolveConns(st, list)
	if err != nil {
		return nil, err
	}
	today := TodayAt(cfg.TZ, nowT)
	todayRows, err := qDailyByDay(db, today)
	if err != nil {
		return nil, err
	}

	s := &Summary{
		Now:        nowT.Unix(),
		Day:        today,
		TZ:         displayTZ(cfg),
		Interval:   cfg.Interval,
		Nodes:      make([]SummaryNode, 0, len(list)),
		RateKnown:  len(rates) > 0,
		RateTotal:  rateTotal(rates),
		Healthy:    hasCollector && st.Error == "",
		Error:      st.Error,
		LastSample: st.LastOK,
		Backend:    backendName(src),
	}

	for _, n := range list {
		scope := "node:" + nodes.IDString(n)
		t := todayRows[scope]
		a := totals[scope]
		c := conns[nodes.IDString(n)]
		sn := SummaryNode{
			ID:       n["id"],
			Name:     nodes.DisplayName(n),
			Type:     nodes.DisplayType(n),
			Port:     n["port"],
			Today:    countersOf(t),
			Total:    countersOf(a),
			Rate:     rates["node:"+nodes.IDString(n)],
			ConnsTCP: c.TCP,
			ConnsUDP: c.UDP,
		}
		s.Nodes = append(s.Nodes, sn)
		s.Today.Rx += sn.Today.Rx
		s.Today.Tx += sn.Today.Tx
		s.Today.RxPkts += sn.Today.RxPkts
		s.Today.TxPkts += sn.Today.TxPkts
		s.Total.Rx += sn.Total.Rx
		s.Total.Tx += sn.Total.Tx
		s.Total.RxPkts += sn.Total.RxPkts
		s.Total.TxPkts += sn.Total.TxPkts
	}

	s.SystemToday = countersOf(todayRows["system"])
	s.SystemTotal = countersOf(totals["system"])
	s.ConnsTotal, s.ConnsUDPTotal = connsTotals(conns)
	return s, nil
}

// LiveNode 是 /api/live 中单个节点条目。
type LiveNode struct {
	ID       any  `json:"id"`
	Rate     Rate `json:"rate"`
	ConnsTCP *int `json:"conns_tcp"`
	ConnsUDP *int `json:"conns_udp"`
	// 在线 IP 数与 IP 限制（由 API 层填充，供前端高频刷新节点卡片的在线 IP 显示）。
	ActiveIPs  int  `json:"active_ip_count,omitempty"`
	IPLimitOn  bool `json:"ip_limit_enabled,omitempty"`
	IPLimitMax int  `json:"ip_limit_max,omitempty"`
}

// Live 对齐 build_live 返回结构：只算当前速率 + 连接数的高频轻量端点。
type Live struct {
	Now           int64      `json:"now"`
	Healthy       bool       `json:"healthy"`
	Error         string     `json:"error"`
	LastSample    int64      `json:"last_sample"`
	Interval      int        `json:"interval"`
	RateKnown     bool       `json:"rate_known"`
	RateTotal     Rate       `json:"rate_total"`
	ConnsTotal    int64      `json:"conns_total"`
	ConnsUDPTotal int64      `json:"conns_udp_total"`
	Nodes         []LiveNode `json:"nodes"`
}

// BuildLive 构建实时端点数据；不碰 daily/totals 表。
func BuildLive(cfg *config.Config, db *sql.DB, src LiveSource) (*Live, error) {
	list := nodes.LoadPanelNodes(cfg.NodesFile)
	rates, err := QRate(db, cfg.Interval, TimeNow().Unix())
	if err != nil {
		return nil, err
	}
	var st Status
	hasCollector := src != nil
	if hasCollector {
		st = src.Snapshot()
	}
	conns, err := resolveConns(st, list)
	if err != nil {
		return nil, err
	}

	l := &Live{
		Now:        TimeNow().Unix(),
		Healthy:    hasCollector && st.Error == "",
		Error:      st.Error,
		LastSample: st.LastOK,
		Interval:   cfg.Interval,
		RateKnown:  len(rates) > 0,
		RateTotal:  rateTotal(rates),
		Nodes:      make([]LiveNode, 0, len(list)),
	}
	l.ConnsTotal, l.ConnsUDPTotal = connsTotals(conns)
	for _, n := range list {
		c := conns[nodes.IDString(n)]
		l.Nodes = append(l.Nodes, LiveNode{
			ID:       n["id"],
			Rate:     rates["node:"+nodes.IDString(n)],
			ConnsTCP: c.TCP,
			ConnsUDP: c.UDP,
		})
	}
	return l, nil
}

// qDailyByDay 返回某天所有 scope 的行。
func qDailyByDay(db *sql.DB, day string) (map[string]TotalsRow, error) {
	rows, err := db.Query(
		"SELECT scope,rx,tx,rx_pkts,tx_pkts FROM daily WHERE day=?", day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TotalsRow{}
	for rows.Next() {
		var r TotalsRow
		if err := rows.Scan(&r.Scope, &r.Rx, &r.Tx, &r.RxPkts, &r.TxPkts); err != nil {
			return nil, err
		}
		out[r.Scope] = r
	}
	return out, rows.Err()
}

func displayTZ(cfg *config.Config) string {
	if tz := strings.TrimSpace(cfg.TZ); tz != "" {
		return tz
	}
	return LocalZoneName()
}

// backendName 返回上报给面板的后端名。nftables-only 架构下恒为 "nft"；
// 保留该字段是为了 /api/summary 的 schema 兼容（前端/外部脚本可能读它）。
func backendName(src LiveSource) string {
	if src != nil {
		return src.BackendName()
	}
	return firewall.BackendName
}
