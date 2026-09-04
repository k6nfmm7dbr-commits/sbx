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
	"log/slog"
	"sort"
	"strings"
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
//
// 并发模型（v3.0.6 修正，之前存在 fatal error: concurrent map read and map write）：
//
//   - runMu 串行化 reconcile（Run goroutine 周期调用 + API 保存时同步调用）。
//   - ipStates / flows 是 reconcile 的**私有可变工作区**，只允许在持有 runMu 时
//     访问，绝不暴露给读侧。
//   - 读侧（Snapshot / NodeIPSnapshot / IPStateSnapshot / ActiveIPs）只读
//     reconcile 末尾在 mu 下发布的不可变快照（states / ipSnaps / activeIPs）。
//
// 关键教训：旧实现里 reconcile 只持 runMu 就直接改 ipStates 及其内部 map，
// 而 SSE/HTTP 读侧只持 mu.RLock 就遍历同一批 map——两把锁互不相干，
// 1s 一次的 reconcile 与每秒一次的 SSE 快照在多核上必然并发读写 map 而崩溃。
// 因此：**任何新增字段都必须明确归属「reconcile 私有」或「mu 保护的已发布快照」**，
// 不允许出现第三种状态。
type Service struct {
	db         *sql.DB
	appDir     string
	policyConf string
	nodesFile  string // 节点文件路径；默认 appDir/nodes.json，须与 panel.json 的 nodes_file 一致

	runMu   sync.Mutex   // 串行化 reconcile；同时保护 ipStates / flows
	mu      sync.RWMutex // 保护下列「已发布」内存态
	states  map[string]State
	ready   bool
	lastErr string

	// ipSnaps / activeIPs 是 reconcile 末尾发布的不可变快照（每轮整体替换，
	// 发布后绝不原地修改），读侧可安全并发读取。
	ipSnaps   map[string]NodeIPSnapshot
	activeIPs map[string][]string

	// activeTCP 是每节点「活跃 TCP 连接数」（conntrack 字节增判口径），
	// 与 states 同一时机发布。不进入任何 JSON API：/proc 回退路径下它退化为
	// 唯一 IP 数（NAT 下多条连接被压成 1），语义与面板展示的全局 socket 计数
	// 不同，贸然下发会误导；保留它是因为 conntrack 路径下它是验证
	// 「本机出站过滤 / acct=0 降级」行为的最直接观测量（测试使用）。
	activeTCP map[string]int

	// ipStates 是 reconcile 私有工作区（runMu 保护，绝不给读侧）：
	//   nodeID -> NodeIPState（Slots=Granted / Observed / Rejected）。
	//   这是「观察到的 IP」「获准使用的 IP」「被拒绝的 IP」三者分离的唯一事实源。
	ipStates map[string]*NodeIPState

	// 已应用的 enforcement 快照（避免每轮 reconcile 无谓重写 nft）。
	appliedQuota   map[string]bool
	appliedIPLimit map[string]map[string]bool // nodeID -> ip set

	now func() time.Time

	// IP 判活与 admission 参数。
	ipIdle         time.Duration // 无流量/消失后判离线窗口（grace = 同一窗口）
	rejectedTTL    time.Duration // 拒绝记录保留时长
	provisionalTTL time.Duration // 候选 slot 超时（未建立则释放）
	conntrack      func(path string) connection.ConntrackResult
	remoteIPs      func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)

	// flow tracker：conntrack 字节增量判活状态（key: node\x00ip:sport）。
	// reconcile 私有，runMu 保护；DeleteNode 清理。
	flows map[string]*flowState

	// acctDisabled 记录「conntrack 存在流但全部 bytes=0」——即内核
	// net.netfilter.nf_conntrack_acct=0（Debian/Ubuntu 默认）。
	// 仅用于打一次提示日志；判活降级是**逐流**判断 f.Bytes==0（见 buildActivity），
	// 因为运行中开启 sysctl 只对新流生效，混合状态下全局开关会误踢老流。
	// runMu 保护。
	acctDisabled bool

	// ctInactive 记录「conntrack 可读但整表 0 条」——内核没在真正跟踪连接
	// （缺少引用 ct 的 netfilter 规则）。仅用于状态变化时打一次日志，
	// 避免每秒刷屏。runMu 保护。
	ctInactive bool

	// selfIPs 是本机地址集合（含回环），用于排除「服务器自身发起的出站流」
	// 被误判成节点客户端。runMu 保护，周期刷新。
	selfIPs    map[string]bool
	selfIPsAt  time.Time
	localAddrs func() (map[string]bool, error)

	// subs 是 SSE 订阅者注册表：每个订阅者持有独立带缓冲 chan，
	// reconcile 发布新状态后向所有订阅者非阻塞广播（扇出）。
	// 旧实现是单一共享 chan——一个 signal 只唤醒一个订阅者，
	// 多客户端时其余只能等 5s fallback，更新延迟被放大。
	subsMu sync.Mutex
	subs   map[chan struct{}]struct{}

	// 策略 nft 应用节流：仅 allow set 内容变化（slot 授予/释放）时，
	// 距上次应用不足 enforceMinInterval 则合并到后续轮次，
	// 避免扫描者用 SYN churn 诱发每秒一次整表 nft -f 重写。
	// quota 状态翻转 / 受限节点集合变化 / 节点端口形态变化仍立即应用。
	enforceMinInterval time.Duration
	lastEnforceAt      time.Time // runMu 保护（reconcile 私有）
	// appliedShape 是上次应用时「节点 id→端口」形态的规范化摘要，
	// 用于发现「端口变了但 allow set 没变」这种 applied 比较看不出来的漂移。
	appliedShape string // runMu 保护

	// tableProbe 探测内核策略表是否存在（防外部删除后 applied 快照不再重写）。
	// lastProbeAt/lastProbeOK 是探测节流与缓存（runMu 保护）。
	tableProbe  func() bool
	lastProbeAt time.Time
	lastProbeOK bool

	// nftApply 执行 nft 脚本（测试可替换为 no-op，规避 CI 无 nft 权限）。
	nftApply func(ctx context.Context, scriptPath string) error
}

// New 构造策略服务。
//
// policyConf 是**策略专属**的 nft 脚本路径，绝不能复用计数规则文件
// （cfg.NftConf / nft.conf）——否则策略脚本会覆盖计数表定义，
// 且 firewall.Nft.Repair 自愈时重放策略脚本，计数器永远建不回来。
func New(db *sql.DB, appDir, policyConf string) *Service {
	if policyConf == "" {
		policyConf = appDir + "/policy.nft"
	}
	return &Service{
		db:                 db,
		appDir:             appDir,
		policyConf:         policyConf,
		nodesFile:          appDir + "/nodes.json",
		states:             map[string]State{},
		ipSnaps:            map[string]NodeIPSnapshot{},
		activeIPs:          map[string][]string{},
		activeTCP:          map[string]int{},
		ipStates:           map[string]*NodeIPState{},
		appliedQuota:       map[string]bool{},
		appliedIPLimit:     map[string]map[string]bool{},
		now:                time.Now,
		ipIdle:             ipIdleTimeout,
		rejectedTTL:        rejectedTTL,
		provisionalTTL:     provisionalTTL,
		conntrack:          connection.ReadConntrack,
		flows:              map[string]*flowState{},
		subs:               map[chan struct{}]struct{}{},
		enforceMinInterval: enforceMinInterval,
		nftApply:           nil, // nil 表示用真实 nft 执行
		localAddrs:         connection.LocalIPs,
	}
}

// DefaultPolicyConf 返回默认策略脚本路径（与计数规则 nft.conf 分离）。
func DefaultPolicyConf(appDir string) string { return appDir + "/policy.nft" }

// SetLocalAddrs 注入本机地址读取函数（测试用）。
func (s *Service) SetLocalAddrs(fn func() (map[string]bool, error)) { s.localAddrs = fn }

// PolicyConfPath 返回策略脚本落盘路径（供诊断/测试）。
func (s *Service) PolicyConfPath() string { return s.policyConf }

func (s *Service) nodesPath() string { return s.nodesFile }

// SetNodesFile 覆盖节点文件路径。serve 必须把 panel.json 的 nodes_file
// 传进来——否则自定义 nodes_file 时策略层与面板读的不是同一个文件。
func (s *Service) SetNodesFile(p string) {
	if p != "" {
		s.nodesFile = p
	}
}

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

// provisionalTTL 是候选（SYN 未建立）slot 的保留时长：超过仍未 ESTABLISHED 即释放，
// 避免端口扫描/失败握手占用名额。
const provisionalTTL = 10 * time.Second

// enforceMinInterval 是「仅 allow set 内容变化」时两次 nft 整表应用的最小间隔。
// quota 翻转 / 受限节点集合变化 / 端口形态变化不受此限（立即应用）。
const enforceMinInterval = 3 * time.Second

// SetEnforceMinInterval 覆盖应用节流间隔（测试用；0 = 不节流）。
func (s *Service) SetEnforceMinInterval(d time.Duration) { s.enforceMinInterval = d }

// SetIPIdle 覆盖判活/grace 窗口（测试用）。
func (s *Service) SetIPIdle(d time.Duration) { s.ipIdle = d }

// SetRejectedTTL 覆盖拒绝记录 TTL（测试用）。
func (s *Service) SetRejectedTTL(d time.Duration) { s.rejectedTTL = d }

// SetProvisionalTTL 覆盖候选 slot 保留时长（测试用）。
func (s *Service) SetProvisionalTTL(d time.Duration) { s.provisionalTTL = d }

// SetConntrack 注入 conntrack 读取函数（测试用）。
func (s *Service) SetConntrack(fn func(path string) connection.ConntrackResult) { s.conntrack = fn }

// SetRemoteIPs 注入 TCP/UDP 活跃 IP 读取函数（/proc 回退数据源，测试可注入）。
func (s *Service) SetRemoteIPs(fn func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)) {
	s.remoteIPs = fn
}

// Subscribe 注册一个 reconcile 发布通知订阅者（SSE 广播层使用）。
// 返回的 channel 带缓冲 1：发布时若订阅者尚未取走上一条 signal，
// 新 signal 直接丢弃（合并唤醒，订阅者醒来总会拉最新快照，不会丢状态）。
// 返回的取消函数必须在订阅结束（连接断开）时调用，防止注册表泄漏。
func (s *Service) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.subsMu.Lock()
			delete(s.subs, ch)
			s.subsMu.Unlock()
		})
	}
}

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
//
// 只读 reconcile 已发布的不可变快照（绝不触碰 ipStates，那是 runMu 下的私有工作区）。
func (s *Service) ActiveIPs(nodeID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ips := s.activeIPs[nodeID]
	if len(ips) == 0 {
		return []string{}
	}
	out := make([]string, len(ips))
	copy(out, ips)
	return out
}

// buildActiveIPsLocked 在 runMu 下由 reconcile 调用，生成某节点的在线 IP 有序列表
// （非 provisional 的 granted slot，按最近活跃时间倒序、同刻按 IP 升序）。
func buildActiveIPsFromState(st *NodeIPState) []string {
	if st == nil {
		return []string{}
	}
	type kv struct {
		ip   string
		last time.Time
	}
	arr := make([]kv, 0, len(st.Slots))
	for ip, slot := range st.Slots {
		if slot.Provisional {
			continue // 候选尚未建立，不算「在线」
		}
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
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // 订阅者繁忙：合并唤醒（它醒来总会拉最新快照）
		}
	}
}

// activeTCPConn 读已发布的活跃 TCP 连接数快照（测试观测用；不进任何 JSON API）。
func (s *Service) activeTCPConn(nodeID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeTCP[nodeID]
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

// buildNodeIPSnapshot 由 reconcile 在 runMu 下调用，把私有 NodeIPState
// 转成不可变展示快照。切片/字段一律新建，发布后绝不再修改。
func buildNodeIPSnapshot(nodeID string, st *NodeIPState) NodeIPSnapshot {
	snap := NodeIPSnapshot{NodeID: nodeID, IPs: []IPEntry{}, Rejected: []IPEntry{}}
	if st == nil {
		return snap
	}
	snap.Limited = st.MaxIPs > 0
	snap.MaxIPs = st.MaxIPs
	snap.Granted = st.activeGrantedCount()
	for ip, slot := range st.Slots {
		if slot.Provisional {
			continue // 候选（尚未 ESTABLISHED）不算在线，不进主列表
		}
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

// NodeIPSnapshot 返回单个节点的在线 IP 快照（读已发布的不可变快照）。
func (s *Service) NodeIPSnapshot(nodeID string) NodeIPSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if snap, ok := s.ipSnaps[nodeID]; ok {
		return snap
	}
	return NodeIPSnapshot{NodeID: nodeID, IPs: []IPEntry{}, Rejected: []IPEntry{}}
}

// IPStateSnapshot 返回所有节点的在线 IP 快照（SSE 首次完整 snapshot）。
// 直接复制已发布的不可变快照 map，不做任何计算，也不触碰 ipStates。
func (s *Service) IPStateSnapshot() map[string]NodeIPSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]NodeIPSnapshot, len(s.ipSnaps))
	for id, snap := range s.ipSnaps {
		out[id] = snap
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

// lifetimeBytesAll 一次性读出所有节点的 lifetime（rx+tx）。
// 旧实现在 reconcile 里对每个节点各发一条 SELECT（N 次查询 / 秒，
// MaxOpenConns=1 下与采集写入争用同一连接），这里合并为单条查询。
func (s *Service) lifetimeBytesAll(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT scope,rx,tx FROM totals WHERE scope LIKE 'node:%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var scope string
		var rx, tx sql.NullInt64
		if err := rows.Scan(&scope, &rx, &tx); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(scope, "node:")] = rx.Int64 + tx.Int64
	}
	return out, rows.Err()
}

// setResetBaseline 只更新单个节点的配额基线（不触碰其它字段）。
// 用于「统计被 reset 后基线高于 lifetime」的自愈校正。
func (s *Service) setResetBaseline(ctx context.Context, nodeID string, baseline int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE node_policy SET quota_reset_baseline=? WHERE node_id=?", baseline, nodeID)
	return err
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

// ClearBaselineTx 在给定事务内把节点配额基线清零。供 `sbx-core reset <scope>`
// 在清空 daily/totals/samples 的**同一事务**中调用。
//
// 为什么必需：used = lifetime(totals) - quota_reset_baseline 并 clamp 到 0。
// reset 删掉 totals 行后 lifetime 归零，若基线仍停在旧高水位（例如用户点过
// 「归零本期用量」后 baseline=100GiB），配额要重新跑满 100GiB 才恢复生效——
// 期间限额完全失效。
func ClearBaselineTx(tx *sql.Tx, scope string) error {
	if scope == "" {
		_, err := tx.Exec("UPDATE node_policy SET quota_reset_baseline=0")
		return err
	}
	if !strings.HasPrefix(scope, "node:") {
		return nil // system 等非节点 scope 与策略基线无关
	}
	_, err := tx.Exec("UPDATE node_policy SET quota_reset_baseline=0 WHERE node_id=?",
		strings.TrimPrefix(scope, "node:"))
	return err
}

// DeleteNode 清理某节点的全部策略状态（配置 + slot + observed + rejected + flow + nft 对象）。
func (s *Service) DeleteNode(ctx context.Context, nodeID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM node_policy WHERE node_id=?", nodeID); err != nil {
		return err
	}
	// ipStates / flows 是 reconcile 私有工作区 → 必须在 runMu 下改；
	// states / ipSnaps / activeIPs 是已发布快照 → 必须在 mu 下改。
	s.runMu.Lock()
	delete(s.ipStates, nodeID)
	// flow tracker 键前缀 nodeID，需整体清除。
	prefix := nodeID + "\x00"
	for k := range s.flows {
		if strings.HasPrefix(k, prefix) {
			delete(s.flows, k)
		}
	}
	delete(s.appliedQuota, nodeID)
	delete(s.appliedIPLimit, nodeID)
	s.mu.Lock()
	delete(s.states, nodeID)
	delete(s.ipSnaps, nodeID)
	delete(s.activeIPs, nodeID)
	delete(s.activeTCP, nodeID)
	s.mu.Unlock()
	s.runMu.Unlock()
	// reconcile 会重建 nft（该节点已不在 list，规则自然清除）。
	return s.reconcile(ctx)
}

// PurgeOrphanConfigs 删除 node_policy 中已不存在节点的孤儿行。
// 节点删除走 nodes CLI 的 candidate/commit 流程，不经过 DeleteNode，
// 因此需要在 reconcile 时按当前节点集合做一次清理（NextID 单调不复用，
// 孤儿行不会串到新节点，但会无界累积）。
func (s *Service) purgeOrphanConfigs(ctx context.Context, alive map[string]bool, cfgs map[string]Config) {
	for id := range cfgs {
		if alive[id] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			"DELETE FROM node_policy WHERE node_id=?", id); err != nil {
			slog.Warn("清理孤儿策略配置失败", "node", id, "err", err)
			continue
		}
		delete(cfgs, id)
		slog.Info("已清理孤儿策略配置", "node", id)
	}
}
