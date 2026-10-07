// Package policy 实现节点级策略：节点暂停、同时在线公网 IP 上限与限速。
// 所有策略由 Web 面板管理（不进 sbx CLI 菜单）。
//
// 数据流：IPLimit = conntrack /proc 活动 → IP slot 集合 → nft allow/drop；
// RateLimit = 每节点 Mbps → nft policer；Paused = 持久暂停状态 → nft ingress/egress drop。
// 统计由独立 traffic collector 继续采集。暂停只拦该节点的端口，不停 sing-box，
// 不删节点配置；恢复时使用原配置和策略。
// IP Limit 的 slot 语义（关键）：
//   - 每节点维护 Slots[nodeID] = ip -> admission 状态（已建立或 provisional）；
//   - 面板在线 IP 数只统计非 provisional slot，不等于当前 socket 数；
//   - 新 IP 出现且容量允许才授予 slot；否则拒绝（nft drop）；
//   - 半开/字节静默 flow 在 60s idle grace 内保留；flow 消失或 idle 超时后释放；
//   - 仅 SYN 候选的 provisional slot 默认 10s 未建立即释放；
//   - 降低 max 不立即踢在线 IP，只在自然释放后收紧（符合产品要求）。
package policy

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// State 是单个节点的策略状态快照（面向 API / UI）。
type State struct {
	Paused       bool   `json:"paused"`
	IPLimitOn    bool   `json:"ip_limit_enabled"`
	IPLimitMax   int    `json:"ip_limit_max"`
	ActiveIPs    int    `json:"active_ip_count"`
	IPLimitState string `json:"ip_limit_state"` // unlimited / ok / exceeded
	// 限速（单向 Mbps，输入/输出各自独立限到此值）。0/false 表示不限速。
	RateLimitOn   bool `json:"rate_limit_enabled"`
	RateLimitMbps int  `json:"rate_limit_mbps"`
}

// Config 是持久化的节点运行/策略配置（node_policy 表一行）。
type Config struct {
	NodeID           string
	Paused           bool
	IPLimitEnabled   bool
	IPLimitMax       int
	RateLimitEnabled bool
	RateLimitMbps    int
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

	// version 是每次发布新快照就自增的单调版本号（mu 保护）。
	// 用途：让 API 层的缓存能**精确**判断"策略数据是否变过"——key 里带上它，
	// 策略一变 key 就变，无需依赖 TTL 猜什么时候该失效。
	version uint64

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
	appliedPaused  map[string]bool
	appliedIPLimit map[string]map[string]bool // nodeID -> ip set
	appliedRate    map[string]int             // nodeID -> mbps（限速）

	now func() time.Time

	// IP 判活与 admission 参数。
	ipIdle         time.Duration // 无流量/消失后判离线窗口（grace = 同一窗口）
	rejectedTTL    time.Duration // 拒绝记录保留时长
	provisionalTTL time.Duration // 候选 slot 超时（未建立则释放）
	conntrack      func(path string) connection.ConntrackResult
	remoteIPs      func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)

	// 节点端口归属索引：nodes.json 未变化时跨 reconcile 复用，避免每秒重新
	// ParsePorts + 构造 port→node map。activityNodes 指向 strict loader 的共享
	// 不可变 slice；原子替换得到新 slice 时指针变化，自动失效。
	activityNodes    []nodes.Node
	activityPortNode map[int]string

	// 节点 ID→端口形态摘要缓存（nodesShape）；身份标记独立于 activityNodes，
	// 两个缓存不能互相覆盖失效判断。仅 reconcile/runMu 路径访问。
	shapeNodes []nodes.Node
	shapeCache string

	// flow tracker：conntrack flow 状态，runMu 保护。
	flows     map[flowKey]flowState
	flowEpoch uint64 // buildActivity 每轮递增，代替每轮分配 currentFlowKeys map

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
	// 暂停状态翻转 / 受限节点集合变化 / 节点端口形态变化仍立即应用。
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
	// enforcementInitialized 记录本进程是否已和内核策略表同步过一次（runMu 保护）。
	// 启动时若持久 nft 表存在而数据库已没有任何策略，需生成空表清除旧暂停端口。
	enforcementInitialized bool

	// nftApply 执行 nft 脚本（测试可替换为 no-op，规避 CI 无 nft 权限）。
	nftApply func(ctx context.Context, scriptPath string) error

	// done 用于宿主服务优雅退出时等待策略 goroutine 完全停止。若只 cancel
	// context 后立刻关闭共享 SQLite，reconcile 仍可能在 Query/Exec 中途使用已
	// 关闭的数据库句柄，造成退出竞态和噪声错误。
	doneOnce sync.Once
	done     chan struct{}
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
		appliedPaused:      map[string]bool{},
		appliedIPLimit:     map[string]map[string]bool{},
		appliedRate:        map[string]int{},
		now:                time.Now,
		ipIdle:             ipIdleTimeout,
		rejectedTTL:        rejectedTTL,
		provisionalTTL:     provisionalTTL,
		conntrack:          connection.ReadConntrack,
		flows:              map[flowKey]flowState{},
		subs:               map[chan struct{}]struct{}{},
		enforceMinInterval: enforceMinInterval,
		nftApply:           nil, // nil 表示用真实 nft 执行
		localAddrs:         connection.LocalIPs,
		done:               make(chan struct{}),
	}
}

// DefaultPolicyConf 返回默认策略脚本路径（与计数规则 nft.conf 分离）。
func DefaultPolicyConf(appDir string) string { return appDir + "/policy.nft" }

// Done 在 Run 返回后关闭。Serve 退出时应等待此通道，确保共享数据库不会
// 在策略 reconcile 仍运行时被关闭；零值 Service（仅测试构造）返回 nil。
func (s *Service) Done() <-chan struct{} { return s.done }

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
// 节点状态翻转 / 受限节点集合变化 / 端口形态变化不受此限（立即应用）。
const enforceMinInterval = 3 * time.Second

// SetEnforceMinInterval 覆盖仅 allowSet 内容变化时的节流间隔（测试用；0 = 不节流）。
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

// Version 返回策略快照版本号：每次 reconcile 发布新快照时自增。
// 调用方可用它做缓存失效判据（版本未变即可安全复用上一次结果）。
func (s *Service) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
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
// 生产路径由 buildNodeSnapshots 单遍构造；此包装保留给测试/诊断。
func buildActiveIPsFromState(st *NodeIPState) []string {
	_, ips := buildNodeSnapshots("", st)
	return ips
}

// flowKey 标识一条 conntrack flow；结构化字段避免每轮字符串拼接分配。
type flowKey struct {
	nodeID  string
	srcIP   string
	srcPort int
}

// flowState 是 conntrack 单条流（node + ip + sport）的判活状态。
type flowState struct {
	Bytes     int64
	LastSeen  time.Time
	SeenEpoch uint64 // 最近一次在 conntrack 快照里出现的轮次
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

// buildNodeSnapshots 单次遍历 Slots/Observed，同时生成 API snapshot 与 active IP
// 列表。此前两者分别构造，各自扫描 Slots、查 Observed、做排序；合并后减少
// 每个节点每个 reconcile 的重复 map 遍历与排序准备。
func buildNodeSnapshots(nodeID string, st *NodeIPState) (NodeIPSnapshot, []string) {
	snap := NodeIPSnapshot{NodeID: nodeID, IPs: []IPEntry{}, Rejected: []IPEntry{}}
	if st == nil {
		return snap, []string{}
	}
	snap.Limited = st.MaxIPs > 0
	snap.MaxIPs = st.MaxIPs
	activeIPs := make([]string, 0, len(st.Slots))
	var firstActive time.Time
	sameActiveTime := true
	for ip, slot := range st.Slots {
		if slot.Provisional {
			continue
		}
		snap.Granted++
		e := IPEntry{IP: ip, Granted: true}
		if o, ok := st.Observed[ip]; ok {
			e.TCP = o.TCPSessions
			e.UDP = o.UDPSessions
		}
		snap.IPs = append(snap.IPs, e)
		activeIPs = append(activeIPs, ip)
		last := slot.LastSeen
		if slot.LastTraffic.After(last) {
			last = slot.LastTraffic
		}
		if len(activeIPs) == 1 {
			firstActive = last
		} else if !last.Equal(firstActive) {
			sameActiveTime = false
		}
	}
	for ip := range st.Rejected {
		e := IPEntry{IP: ip, Granted: false}
		if o, ok := st.Observed[ip]; ok {
			e.TCP = o.TCPSessions
			e.UDP = o.UDPSessions
		}
		snap.Rejected = append(snap.Rejected, e)
	}
	cmpIP := func(a, b IPEntry) int {
		if a.IP < b.IP {
			return -1
		}
		if a.IP > b.IP {
			return 1
		}
		return 0
	}
	slices.SortFunc(snap.IPs, cmpIP)
	slices.SortFunc(snap.Rejected, cmpIP)
	if sameActiveTime {
		slices.Sort(activeIPs)
		return snap, activeIPs
	}
	type kv struct {
		ip   string
		last time.Time
	}
	arr := make([]kv, 0, len(activeIPs))
	for _, ip := range activeIPs {
		slot := st.Slots[ip]
		last := slot.LastSeen
		if slot.LastTraffic.After(last) {
			last = slot.LastTraffic
		}
		arr = append(arr, kv{ip: ip, last: last})
	}
	slices.SortFunc(arr, func(a, b kv) int {
		if !a.last.Equal(b.last) {
			if a.last.After(b.last) {
				return -1
			}
			return 1
		}
		if a.ip < b.ip {
			return -1
		}
		if a.ip > b.ip {
			return 1
		}
		return 0
	})
	for i := range activeIPs {
		activeIPs[i] = arr[i].ip
	}
	return snap, activeIPs
}

// NodeIPSnapshot 返回单个节点的在线 IP 快照（读已发布的不可变快照）。
func (s *Service) NodeIPSnapshot(nodeID string) NodeIPSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if snap, ok := s.ipSnaps[nodeID]; ok {
		snap.IPs = append([]IPEntry(nil), snap.IPs...)
		snap.Rejected = append([]IPEntry(nil), snap.Rejected...)
		return snap
	}
	return NodeIPSnapshot{NodeID: nodeID, IPs: []IPEntry{}, Rejected: []IPEntry{}}
}

// IPStateSnapshot 返回所有节点的在线 IP 快照（SSE 首次完整 snapshot）。
// 返回值及其中的切片均为独立副本：调用方可安全修改，不会破坏服务内部
// 的已发布快照，也不会与并发 SSE 序列化产生数据竞态。
func (s *Service) IPStateSnapshot() map[string]NodeIPSnapshot {
	out, _ := s.IPStateSnapshotVersion()
	return out
}

// IPStateSnapshotVersion 原子取得「快照内容 + 版本号」。
// SSE 初始化必须同时拿到这两个值：如果先读 Version、再读快照，reconcile
// 可能恰好在中间发布新状态，导致旧快照被错误标记成新版本，客户端漏掉一次
// 更新。这里在同一把 mu 下复制二者，调用方可以可靠地做一致性校验。
func (s *Service) IPStateSnapshotVersion() (map[string]NodeIPSnapshot, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]NodeIPSnapshot, len(s.ipSnaps))
	for id, snap := range s.ipSnaps {
		snap.IPs = append([]IPEntry(nil), snap.IPs...)
		snap.Rejected = append([]IPEntry(nil), snap.Rejected...)
		out[id] = snap
	}
	return out, s.version
}

// loadConfigs 读全部策略配置。
func (s *Service) loadConfigs(ctx context.Context) (map[string]Config, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT node_id,paused,ip_limit_enabled,ip_limit_max,rate_limit_enabled,rate_limit_mbps FROM node_policy")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Config{}
	for rows.Next() {
		var c Config
		var paused, ile, rle int
		if err := rows.Scan(&c.NodeID, &paused, &ile, &c.IPLimitMax, &rle, &c.RateLimitMbps); err != nil {
			return nil, err
		}
		c.Paused = paused != 0
		c.IPLimitEnabled = ile != 0
		c.RateLimitEnabled = rle != 0
		out[c.NodeID] = c
	}
	return out, rows.Err()
}

// GetConfig 读单个节点策略配置（不存在时返回未暂停、无限制）。
func (s *Service) GetConfig(ctx context.Context, nodeID string) (Config, error) {
	var c Config
	var paused, ile, rle int
	err := s.db.QueryRowContext(ctx,
		"SELECT node_id,paused,ip_limit_enabled,ip_limit_max,rate_limit_enabled,rate_limit_mbps FROM node_policy WHERE node_id=?",
		nodeID).Scan(&c.NodeID, &paused, &ile, &c.IPLimitMax, &rle, &c.RateLimitMbps)
	if err == sql.ErrNoRows {
		return Config{NodeID: nodeID}, nil
	}
	if err != nil {
		return Config{}, err
	}
	c.Paused = paused != 0
	c.IPLimitEnabled = ile != 0
	c.RateLimitEnabled = rle != 0
	return c, nil
}

// UpsertConfig 写回（或更新）节点暂停、IP limit 与 rate limit。
func (s *Service) UpsertConfig(ctx context.Context, c Config) error {
	paused, ile, rle := 0, 0, 0
	if c.Paused {
		paused = 1
	}
	if c.IPLimitEnabled {
		ile = 1
	}
	if c.RateLimitEnabled {
		rle = 1
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO node_policy(node_id,paused,ip_limit_enabled,ip_limit_max,rate_limit_enabled,rate_limit_mbps) "+
			"VALUES(?,?,?,?,?,?) "+
			"ON CONFLICT(node_id) DO UPDATE SET paused=excluded.paused,"+
			"ip_limit_enabled=excluded.ip_limit_enabled,ip_limit_max=excluded.ip_limit_max,"+
			"rate_limit_enabled=excluded.rate_limit_enabled,rate_limit_mbps=excluded.rate_limit_mbps",
		c.NodeID, paused, ile, c.IPLimitMax, rle, c.RateLimitMbps)
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
	// flow tracker 按结构化 nodeID 键逐条清除。
	for k := range s.flows {
		if k.nodeID == nodeID {
			delete(s.flows, k)
		}
	}
	delete(s.appliedPaused, nodeID)
	delete(s.appliedIPLimit, nodeID)
	delete(s.appliedRate, nodeID)
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
