package traffic

import (
	"database/sql"
	"sort"
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
//
// StatusLite 是 Snapshot 的无分配版本（只含标量字段，不拷贝连接数 map）：
// 缓存版本号与 /healthz 这类每个请求都要走的路径只用它，避免每次请求深拷贝
// 整张连接数 map。
type LiveSource interface {
	Snapshot() Status
	StatusLite() Status
	BackendName() string
}

// RateSource 是可选的速率缓存接口。Collector 实现它；测试 fake 或 CLI
// 数据源不实现时，调用方回退到原有 QRate 查询，保持接口兼容。
type RateSource interface {
	RateSnapshot() (map[string]Rate, bool, error)
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

func resolveRates(db *sql.DB, interval int, nowUnix int64, src LiveSource) (map[string]Rate, error) {
	if rs, ok := src.(RateSource); ok {
		rates, ready, err := rs.RateSnapshot()
		if err != nil {
			return nil, err
		}
		if ready {
			return rates, nil
		}
	}
	return QRate(db, interval, nowUnix)
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
	Paused        bool   `json:"paused,omitempty"`
	IPLimitOn     bool   `json:"ip_limit_enabled,omitempty"`
	IPLimitMax    int    `json:"ip_limit_max,omitempty"`
	ActiveIPs     int    `json:"active_ip_count,omitempty"`
	IPLimitState  string `json:"ip_limit_state,omitempty"`
	RateLimitOn   bool   `json:"rate_limit_enabled,omitempty"`
	RateLimitMbps int    `json:"rate_limit_mbps,omitempty"`
}

// HistoryNode 是已从 nodes.json 移除、但 daily 表里仍有流量的节点。
//
// 存在的理由：删除节点默认保留历史（见 api 层的 deleteNode）。没有这份清单，
// 保留下来的数据虽然还在库里，却在面板上无处可选、无处可看——
// 那"保留"就只做了一半。
type HistoryNode struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	DayFirst string   `json:"day_first"`
	DayLast  string   `json:"day_last"`
	Days     int      `json:"days"`
	Total    Counters `json:"total"`
}

// Summary 对齐 build_summary 返回结构。
type Summary struct {
	Now        int64         `json:"now"`
	Day        string        `json:"day"`
	TZ         string        `json:"tz"`
	Interval   int           `json:"interval"`
	Backend    string        `json:"backend"`
	Healthy    bool          `json:"healthy"`
	Error      string        `json:"error"`
	LastSample int64         `json:"last_sample"`
	Nodes      []SummaryNode `json:"nodes"`
	// HistoryNodes 是已删除但仍有历史流量的节点，供面板「历史节点」入口使用。
	// omitempty：没有历史节点时整个键不出现，与既有 API shape 保持一致
	//（同 SummaryNode 的策略字段）；前端按 `|| []` 兜底。
	HistoryNodes  []HistoryNode `json:"history_nodes,omitempty"`
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

// nodeNameMetaKeyPrefix 与 api 层删除节点时的约定一致：把展示名存在
// meta 的 node_name:<id> 键里，供这里回显。
const nodeNameMetaKeyPrefix = "node_name:"

// qHistoryNodes 列出「不在当前 nodes.json 里、但 daily 中仍有流量」的节点。
//
// 只读 daily 与 meta，不碰任何写路径。
func qHistoryNodes(db *sql.DB, current []nodes.Node) ([]HistoryNode, error) {
	// 顺序要紧：先把名字取完，再开主查询的游标。
	//
	// database.DB 的连接池只有一条连接（SetMaxOpenConns(1)），而 database/sql 在
	// rows 关闭前不会归还连接——在外层 rows 仍开着时再发一条 Query 会死等连接，
	// 表现为整个 /api/summary 永久挂起。
	names := nodeNamesFromMeta(db)

	rows, err := db.Query(
		"SELECT scope, MIN(day), MAX(day), COUNT(*), " +
			"SUM(rx), SUM(tx), SUM(rx_pkts), SUM(tx_pkts) " +
			"FROM daily WHERE scope LIKE 'node:%' GROUP BY scope")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	live := make(map[string]struct{}, len(current))
	for _, n := range current {
		live["node:"+nodes.IDString(n)] = struct{}{}
	}

	out := []HistoryNode{}
	for rows.Next() {
		var scope, first, last string
		var days int
		var rx, tx, rxPkts, txPkts int64
		if err := rows.Scan(&scope, &first, &last, &days, &rx, &tx, &rxPkts, &txPkts); err != nil {
			return nil, err
		}
		if _, stillLive := live[scope]; stillLive {
			continue // 还在 nodes.json 里的节点由 Nodes 那份负责，不重复列出
		}
		id := strings.TrimPrefix(scope, "node:")
		name := names[id]
		if name == "" {
			// 早于"记名"功能删除的节点没有留档，退回显示编号。
			name = "节点 #" + id
		}
		out = append(out, HistoryNode{
			ID: id, Name: name,
			DayFirst: first, DayLast: last, Days: days,
			Total: Counters{Rx: rx, Tx: tx, RxPkts: rxPkts, TxPkts: txPkts},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 最近还有流量的排前面，便于查找。
	sort.Slice(out, func(i, j int) bool {
		if out[i].DayLast != out[j].DayLast {
			return out[i].DayLast > out[j].DayLast
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// nodeNamesFromMeta 读回已删除节点的展示名。读不到就返回空表——
// 名字只是锦上添花，调用方会退回显示编号。
func nodeNamesFromMeta(db *sql.DB) map[string]string {
	out := map[string]string{}
	// 用 GLOB 而不是 LIKE：键名前缀里的下划线在 LIKE 中是通配符，
	// 会把 nodeXname: 之类的键一并匹配进来。
	rows, err := db.Query("SELECT k,v FROM meta WHERE k GLOB ?", nodeNameMetaKeyPrefix+"*")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return out
		}
		if v.Valid {
			out[strings.TrimPrefix(k, nodeNameMetaKeyPrefix)] = v.String
		}
	}
	return out
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
	rates, err := resolveRates(db, cfg.Interval, nowT.Unix(), src)
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
	todayRows, err := QDailyByDay(db, today)
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

	// 已删除节点的历史。删除默认保留数据，这份清单是让它"看得见"的入口。
	if s.HistoryNodes, err = qHistoryNodes(db, list); err != nil {
		return nil, err
	}

	for _, n := range list {
		// id/scope 每节点只拼接一次（旧实现把 "node:"+IDString 拼了两次，
		// 且 IDString 也重复调用）。
		id := nodes.IDString(n)
		scope := "node:" + id
		t := todayRows[scope]
		a := totals[scope]
		c := conns[id]
		sn := SummaryNode{
			ID:       n["id"],
			Name:     nodes.DisplayName(n),
			Type:     nodes.DisplayType(n),
			Port:     n["port"],
			Today:    countersOf(t),
			Total:    countersOf(a),
			Rate:     rates[scope],
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
	Paused     bool `json:"paused,omitempty"`
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
	nowT := TimeNow()
	rates, err := resolveRates(db, cfg.Interval, nowT.Unix(), src)
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
		Now:        nowT.Unix(),
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
		id := nodes.IDString(n)
		c := conns[id]
		l.Nodes = append(l.Nodes, LiveNode{
			ID:       n["id"],
			Rate:     rates["node:"+id],
			ConnsTCP: c.TCP,
			ConnsUDP: c.UDP,
		})
	}
	return l, nil
}

// QDailyByDay 返回某天所有 scope 的行。
//
// 导出供 notify 包复用：推送需要在只读路径上取同一天的原始行，
// 若各写一份查询，"面板显示"与"异地留痕"就可能在口径上悄悄分叉。
func QDailyByDay(db *sql.DB, day string) (map[string]TotalsRow, error) {
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
