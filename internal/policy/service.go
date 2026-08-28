// Package policy 实现节点级策略：独立流量配额（Quota）与同时在线公网 IP 上限
// （IP Limit）。两者都只在 Web 面板管理（不进 sbx CLI 菜单）。
//
// 数据流：
//
//	Quota:  内核 counter → Collector → SQLite totals（lifetime）→ 减 reset 基线 → used
//	IPLimit: /proc/net 连接状态 → IP Tracker → active IP slot 集合 → nft set 执行 allow/drop
//
// 达限由 nftables 在内核执行，只针对目标节点；绝不停 sing-box、不删节点、不改凭据。
//
// IP Limit 的 slot 语义（关键）：
//   - 每节点维护 slots[nodeID] = ip -> lastSeen；
//   - active = len(slots)（已获 slot 的 IP 数，非当前 /proc 里的连接数）；
//   - 新 IP 出现且 len(slots) < max 才授予 slot；否则被拒（nft drop）；
//   - slot IP 离线超过 TTL（默认 120s）释放，之后新 IP 才能补位；
//   - 降低 max 不立即踢在线 IP，只在自然离线后收紧（符合产品要求）。
package policy

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// State 是单个节点的策略状态快照（面向 API / UI）。
type State struct {
	QuotaEnabled bool   `json:"quota_enabled"`
	QuotaLimit   int64  `json:"quota_limit_bytes"`
	QuotaUsed    int64  `json:"quota_used_bytes"`
	QuotaState   string `json:"quota_state"` // unlimited / ok / exceeded
	IPLimitOn    bool   `json:"ip_limit_enabled"`
	IPLimitMax   int    `json:"ip_limit_max"`
	ActiveIPs    int    `json:"active_ip_count"`
	IPLimitState string `json:"ip_limit_state"` // unlimited / ok / exceeded

	// ActiveTCPConn 是节点「活跃」TCP 连接数：有近期流量（conntrack 字节增判）的
	// 连接才计入；conntrack 不可用时回退为 /proc ESTABLISHED 数。UDP 型节点为 0。
	ActiveTCPConn int `json:"active_tcp_conns,omitempty"`
}

// Config 是持久化的策略配置（node_policy 表一行）。
type Config struct {
	NodeID             string
	QuotaEnabled       bool
	QuotaLimitBytes    int64
	QuotaResetBaseline int64
	IPLimitEnabled     bool
	IPLimitMax         int
}

// Service 是策略核心：读配置、算 used、追踪 IP slot、生成并应用 nft 规则。
// 并发模型：
//   - reconcile 由 runMu 串行化（Run goroutine 周期调用 + API 保存时同步调用，
//     两者可能并发 → 必须串行，否则并发写 map 会 fatal error 崩溃）；
//   - states/ipStates/applied 由 mu 保护（Snapshot / 快照 只读走 RLock）。
type Service struct {
	db      *sql.DB
	appDir  string
	nftConf string

	runMu   sync.Mutex   // 串行化 reconcile
	mu      sync.RWMutex // 保护下列内存态
	states  map[string]State
	ready   bool
	lastErr string

	// IP Tracker 运行态（都在 mu 保护下）：
	//   ipStates —— nodeID -> NodeIPState（Slots=Granted / Observed / Rejected）。
	//   这是「观察到的 IP」「获准使用的 IP」「被拒绝的 IP」三者分离的唯一事实源。
	ipStates map[string]*NodeIPState

	// 已应用的 enforcement 快照（避免每轮 reconcile 无谓重写 nft）。
	appliedQuota   map[string]bool
	appliedIPLimit map[string]map[string]bool // nodeID -> ip set

	now func() time.Time

	// IP 判活与 admission 参数。
	ipIdle      time.Duration // 无流量/消失后判离线窗口（grace = 同一窗口）
	rejectedTTL time.Duration // 拒绝记录保留时长
	conntrack   func(path string) connection.ConntrackResult
	remoteIPs   func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)

	// flow tracker：conntrack 字节增量判活状态（key: node\x00ip:sport）。
	// reconcile 串行访问，runMu 保护；DeleteNode 清理。
	flows map[string]*flowState

	// notify 在每次 reconcile 发布新状态后非阻塞 signal，供 SSE 广播层唤醒。
	notify chan struct{}

	// nftApply 执行 nft 脚本（测试可替换为 no-op，规避 CI 无 nft 权限）。
	nftApply func(ctx context.Context, scriptPath string) error
}

// New 构造策略服务。
func New(db *sql.DB, appDir, nftConf string) *Service {
	return &Service{
		db:             db,
		appDir:         appDir,
		nftConf:        nftConf,
		states:         map[string]State{},
		ipStates:       map[string]*NodeIPState{},
		appliedQuota:   map[string]bool{},
		appliedIPLimit: map[string]map[string]bool{},
		now:            time.Now,
		ipIdle:         ipIdleTimeout,
		rejectedTTL:    rejectedTTL,
		conntrack:      connection.ReadConntrack,
		flows:          map[string]*flowState{},
		notify:         make(chan struct{}, 1),
		nftApply:       nil, // nil 表示用真实 nft 执行
	}
}

func (s *Service) nodesPath() string { return s.appDir + "/nodes.json" }

// SetClock 注入时钟（测试用）。
func (s *Service) SetClock(fn func() time.Time) { s.now = fn }

// SetNFTApply 注入 nft 脚本执行函数（测试用，规避无 netlink 权限的环境）。
func (s *Service) SetNFTApply(fn func(ctx context.Context, scriptPath string) error) {
	s.nftApply = fn
}

// ipIdleTimeout 是「无流量 / 消失后判离线」的窗口（同时充当 grace 时长）。
// 移动端切网、TCP reconnect、QUIC、UDP NAT 短暂消失不会瞬间释放 slot。
const ipIdleTimeout = 60 * time.Second

// rejectedTTL 是「被拒绝 IP」记录的保留时长（防端口扫描无限增长）。
const rejectedTTL = 60 * time.Second

// SetIPIdle 覆盖判活/grace 窗口（测试用）。
func (s *Service) SetIPIdle(d time.Duration) { s.ipIdle = d }

// SetRejectedTTL 覆盖拒绝记录 TTL（测试用）。
func (s *Service) SetRejectedTTL(d time.Duration) { s.rejectedTTL = d }

// SetConntrack 注入 conntrack 读取函数（测试用）。
func (s *Service) SetConntrack(fn func(path string) connection.ConntrackResult) { s.conntrack = fn }

// SetRemoteIPs 注入 TCP/UDP 活跃 IP 读取函数（/proc 回退数据源，测试可注入）。
func (s *Service) SetRemoteIPs(fn func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)) {
	s.remoteIPs = fn
}

// Notify 返回 reconcile 发布新状态时被 signal 的只读通道（SSE 广播层等待它）。
func (s *Service) Notify() <-chan struct{} { return s.notify }

// Snapshot 返回策略状态快照。ready=false 表示尚未完成首次 reconcile。
func (s *Service) Snapshot() (map[string]State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]State, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out, s.ready
}

// LastError 返回最近一次 reconcile 错误。
func (s *Service) LastError() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// ActiveIPs 返回某节点当前已获 slot（granted）的公网 IP 列表，按活跃时间倒序。
// 未开启限制时，所有活跃 IP 都被授予 slot，因此等价于「当前在线 IP」。
func (s *Service) ActiveIPs(nodeID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.ipStates[nodeID]
	if st == nil {
		return []string{}
	}
	type kv struct {
		ip   string
		last time.Time
	}
	arr := make([]kv, 0, len(st.Slots))
	for ip, slot := range st.Slots {
		last := slot.LastSeen
		if slot.LastTraffic.After(last) {
			last = slot.LastTraffic
		}
		arr = append(arr, kv{ip, last})
	}
	sort.Slice(arr, func(i, j int) bool {
		if !arr[i].last.Equal(arr[j].last) {
			return arr[i].last.After(arr[j].last)
		}
		return arr[i].ip < arr[j].ip
	})
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		out = append(out, it.ip)
	}
	return out
}

// flowState 是 conntrack 单条流（node + ip + sport）的判活状态。
type flowState struct {
	Bytes    int64
	LastSeen time.Time
}

func (s *Service) signalNotify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// IPEntry 是单个客户端 IP 的展示快照。
type IPEntry struct {
	IP      string `json:"ip"`
	TCP     int    `json:"tcp"`
	UDP     int    `json:"udp"`
	Granted bool   `json:"granted"`
}

// NodeIPSnapshot 是单个节点的在线 IP 快照（供 /api/nodes/:id/ip-state 与 SSE）。
type NodeIPSnapshot struct {
	NodeID   string    `json:"node_id"`
	Limited  bool      `json:"limited"`
	MaxIPs   int       `json:"max_ips"`
	Granted  int       `json:"granted_count"`
	IPs      []IPEntry `json:"ips"`
	Rejected []IPEntry `json:"rejected"`
}

func (s *Service) nodeIPSnapshotLocked(nodeID string) NodeIPSnapshot {
	snap := NodeIPSnapshot{NodeID: nodeID, IPs: []IPEntry{}, Rejected: []IPEntry{}}
	st := s.ipStates[nodeID]
	if st == nil {
		return snap
	}
	snap.Limited = st.MaxIPs > 0
	snap.MaxIPs = st.MaxIPs
	snap.Granted = len(st.Slots)
	for ip := range st.Slots {
		e := IPEntry{IP: ip, Granted: true}
		if o, ok := st.Observed[ip]; ok {
			e.TCP = o.TCPSessions
			e.UDP = o.UDPSessions
		}
		snap.IPs = append(snap.IPs, e)
	}
	for ip := range st.Rejected {
		e := IPEntry{IP: ip, Granted: false}
		if o, ok := st.Observed[ip]; ok {
			e.TCP = o.TCPSessions
			e.UDP = o.UDPSessions
		}
		snap.Rejected = append(snap.Rejected, e)
	}
	sort.Slice(snap.IPs, func(i, j int) bool { return snap.IPs[i].IP < snap.IPs[j].IP })
	sort.Slice(snap.Rejected, func(i, j int) bool { return snap.Rejected[i].IP < snap.Rejected[j].IP })
	return snap
}

// NodeIPSnapshot 返回单个节点的在线 IP 快照。
func (s *Service) NodeIPSnapshot(nodeID string) NodeIPSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodeIPSnapshotLocked(nodeID)
}

// IPStateSnapshot 返回所有节点的在线 IP 快照（SSE 首次完整 snapshot）。
func (s *Service) IPStateSnapshot() map[string]NodeIPSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]NodeIPSnapshot, len(s.states))
	for id := range s.states {
		out[id] = s.nodeIPSnapshotLocked(id)
	}
	return out
}

// loadConfigs 读全部策略配置。
func (s *Service) loadConfigs(ctx context.Context) (map[string]Config, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT node_id,quota_enabled,quota_limit_bytes,quota_reset_baseline,"+
			"ip_limit_enabled,ip_limit_max FROM node_policy")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Config{}
	for rows.Next() {
		var c Config
		var qe, ile int
		if err := rows.Scan(&c.NodeID, &qe, &c.QuotaLimitBytes, &c.QuotaResetBaseline,
			&ile, &c.IPLimitMax); err != nil {
			return nil, err
		}
		c.QuotaEnabled = qe != 0
		c.IPLimitEnabled = ile != 0
		out[c.NodeID] = c
	}
	return out, rows.Err()
}

// lifetimeBytes 读某节点历史累计流量（rx+tx，来自 totals 权威统计）。
func (s *Service) lifetimeBytes(ctx context.Context, nodeID string) (int64, error) {
	var rx, tx sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT rx,tx FROM totals WHERE scope=?", "node:"+nodeID).Scan(&rx, &tx)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rx.Int64 + tx.Int64, nil
}

// GetConfig 读单个节点策略配置（不存在时返回默认「全不限」）。
func (s *Service) GetConfig(ctx context.Context, nodeID string) (Config, error) {
	var c Config
	var qe, ile int
	err := s.db.QueryRowContext(ctx,
		"SELECT node_id,quota_enabled,quota_limit_bytes,quota_reset_baseline,"+
			"ip_limit_enabled,ip_limit_max FROM node_policy WHERE node_id=?",
		nodeID).Scan(&c.NodeID, &qe, &c.QuotaLimitBytes, &c.QuotaResetBaseline,
		&ile, &c.IPLimitMax)
	if err == sql.ErrNoRows {
		return Config{NodeID: nodeID}, nil
	}
	if err != nil {
		return Config{}, err
	}
	c.QuotaEnabled = qe != 0
	c.IPLimitEnabled = ile != 0
	return c, nil
}

// UpsertConfig 写回（或更新）节点策略配置。
func (s *Service) UpsertConfig(ctx context.Context, c Config) error {
	qe, ile := 0, 0
	if c.QuotaEnabled {
		qe = 1
	}
	if c.IPLimitEnabled {
		ile = 1
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO node_policy(node_id,quota_enabled,quota_limit_bytes,"+
			"quota_reset_baseline,ip_limit_enabled,ip_limit_max) "+
			"VALUES(?,?,?,?,?,?) "+
			"ON CONFLICT(node_id) DO UPDATE SET quota_enabled=excluded.quota_enabled,"+
			"quota_limit_bytes=excluded.quota_limit_bytes,"+
			"quota_reset_baseline=excluded.quota_reset_baseline,"+
			"ip_limit_enabled=excluded.ip_limit_enabled,"+
			"ip_limit_max=excluded.ip_limit_max",
		c.NodeID, qe, c.QuotaLimitBytes, c.QuotaResetBaseline, ile, c.IPLimitMax)
	return err
}

// ResetQuota 把该节点 quota 基线重置到当前 lifetime，使 used 归零，
// 但不删除历史累计流量。返回新的 reset 基线值。
func (s *Service) ResetQuota(ctx context.Context, nodeID string) (int64, error) {
	life, err := s.lifetimeBytes(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	c, err := s.GetConfig(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	c.QuotaResetBaseline = life
	if err := s.UpsertConfig(ctx, c); err != nil {
		return 0, err
	}
	// 立即重算，避免 API 返回旧 used 值。
	if err := s.reconcile(ctx); err != nil {
		return 0, err
	}
	return life, nil
}

// DeleteNode 清理某节点的全部策略状态（配置 + slot + observed + rejected + flow + nft 对象）。
func (s *Service) DeleteNode(ctx context.Context, nodeID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM node_policy WHERE node_id=?", nodeID); err != nil {
		return err
	}
	// 与 reconcile 串行：直接改内存 map 必须与周期 reconcile 互斥。
	s.runMu.Lock()
	s.mu.Lock()
	delete(s.states, nodeID)
	delete(s.ipStates, nodeID)
	delete(s.appliedQuota, nodeID)
	delete(s.appliedIPLimit, nodeID)
	s.mu.Unlock()
	// flow tracker 键前缀 nodeID，需整体清除。
	prefix := nodeID + "\x00"
	for k := range s.flows {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(s.flows, k)
		}
	}
	s.runMu.Unlock()
	// reconcile 会重建 nft（该节点已不在 list，规则自然清除）。
	return s.reconcile(ctx)
}
