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
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// 定时重置校验错误。
var (
	errInvalidResetDay  = errors.New("重置日必须在 1~30 之间")
	errInvalidResetTime = errors.New("重置时刻需为 HH:MM 或 HH:MM:SS")
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

	// 定时重置（每月固定日自动重置）：用户可开关。
	ResetEnabled bool `json:"reset_enabled"`
	// ResetDay 每月第几日（1~30）。
	ResetDay int `json:"reset_day"`
	// ResetTime "HH:MM:SS"（秒固定 00）。
	ResetTime string `json:"reset_time"`
	// ResetNextAt 下次重置 Unix 秒。
	ResetNextAt int64 `json:"reset_next_at"`
}

// Config 是持久化的策略配置（node_policy 表一行）。
type Config struct {
	NodeID             string
	QuotaEnabled       bool
	QuotaLimitBytes    int64
	QuotaResetBaseline int64
	IPLimitEnabled     bool
	IPLimitMax         int

	// 定时重置。
	ResetEnabled bool
	ResetDay     int
	ResetTime    string
	ResetNextAt  int64
}

// Service 是策略核心：读配置、算 used、追踪 IP slot、生成并应用 nft 规则。
// 并发模型：
//   - reconcile 由 runMu 串行化（Run goroutine 周期调用 + API 保存时同步调用，
//     两者可能并发 → 必须串行，否则并发写 map 会 fatal error 崩溃）；
//   - states/slots/online/applied 由 mu 保护（Snapshot/ActiveIPs 只读走 RLock）。
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
	//   slots  —— nodeID -> ip -> UDP lastSeen（UnixNano），只存 UDP，用于 TTL 判定；
	//   online —— nodeID -> ip -> lastSeen，当前「在线 IP」快照（TCP+UDP 合并，
	//             TCP 断开立即消失，UDP 靠 TTL），供 ActiveIPs / active 数 / nft 用。
	slots  map[string]map[string]int64
	online map[string]map[string]int64

	// 已应用的 enforcement 快照（避免每轮 reconcile 无谓重写 nft）。
	appliedQuota   map[string]bool
	appliedIPLimit map[string]map[string]bool // nodeID -> ip set

	udpTTL   time.Duration
	now      func() time.Time
	location *time.Location // 定时重置的本地时区（nil = time.Local）

	// nftApply 执行 nft 脚本（测试可替换为 no-op，规避 CI 无 nft 权限）。
	nftApply func(ctx context.Context, scriptPath string) error

	// remoteIPs 读取各节点 TCP/UDP 活跃 IP（测试可注入，默认读 /proc）。
	remoteIPs func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)
}

// New 构造策略服务。
func New(db *sql.DB, appDir, nftConf string) *Service {
	return &Service{
		db:             db,
		appDir:         appDir,
		nftConf:        nftConf,
		states:         map[string]State{},
		slots:          map[string]map[string]int64{},
		online:         map[string]map[string]int64{},
		appliedQuota:   map[string]bool{},
		appliedIPLimit: map[string]map[string]bool{},
		udpTTL:         120 * time.Second,
		now:            time.Now,
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

// SetUDPTTL 覆盖 UDP slot 释放 TTL（测试用）。
func (s *Service) SetUDPTTL(d time.Duration) { s.udpTTL = d }

// SetLocation 覆盖定时重置的本地时区（测试用）。
func (s *Service) SetLocation(loc *time.Location) { s.location = loc }

// Now 返回当前时间（注入时钟）。
func (s *Service) Now() time.Time { return s.now() }

// ValidateReset 校验定时重置配置合法性。
// 关闭时无约束；开启时要求日 1~30、时刻为 HH:MM 或 HH:MM:SS。
func (s *Service) ValidateReset(c Config) error {
	if !c.ResetEnabled {
		return nil
	}
	if !ValidResetDay(c.ResetDay) {
		return errInvalidResetDay
	}
	if ParseResetTime(c.ResetTime) < 0 {
		return errInvalidResetTime
	}
	return nil
}

// SetRemoteIPs 注入 TCP/UDP 活跃 IP 读取函数（测试用）。
func (s *Service) SetRemoteIPs(fn func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error)) {
	s.remoteIPs = fn
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

// ActiveIPs 返回某节点当前在线公网 IP 列表（读 online 快照，含 TCP+UDP，按最后活跃时间倒序）。
func (s *Service) ActiveIPs(nodeID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.online[nodeID]
	type kv struct {
		ip   string
		last int64
	}
	arr := make([]kv, 0, len(m))
	for ip, last := range m {
		arr = append(arr, kv{ip, last})
	}
	// 稳定排序：lastSeen 倒序，同时间按 IP 字典序（确定性输出）。
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].last != arr[j].last {
			return arr[i].last > arr[j].last
		}
		return arr[i].ip < arr[j].ip
	})
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		out = append(out, it.ip)
	}
	return out
}

// loadConfigs 读全部策略配置。
func (s *Service) loadConfigs(ctx context.Context) (map[string]Config, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT node_id,quota_enabled,quota_limit_bytes,quota_reset_baseline,"+
			"ip_limit_enabled,ip_limit_max,reset_enabled,reset_day,reset_time,reset_next_at "+
			"FROM node_policy")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Config{}
	for rows.Next() {
		var c Config
		var qe, ile, re int
		if err := rows.Scan(&c.NodeID, &qe, &c.QuotaLimitBytes, &c.QuotaResetBaseline,
			&ile, &c.IPLimitMax, &re, &c.ResetDay, &c.ResetTime, &c.ResetNextAt); err != nil {
			return nil, err
		}
		c.QuotaEnabled = qe != 0
		c.IPLimitEnabled = ile != 0
		c.ResetEnabled = re != 0
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
	var qe, ile, re int
	err := s.db.QueryRowContext(ctx,
		"SELECT node_id,quota_enabled,quota_limit_bytes,quota_reset_baseline,"+
			"ip_limit_enabled,ip_limit_max,reset_enabled,reset_day,reset_time,reset_next_at "+
			"FROM node_policy WHERE node_id=?",
		nodeID).Scan(&c.NodeID, &qe, &c.QuotaLimitBytes, &c.QuotaResetBaseline,
		&ile, &c.IPLimitMax, &re, &c.ResetDay, &c.ResetTime, &c.ResetNextAt)
	if err == sql.ErrNoRows {
		return Config{NodeID: nodeID}, nil
	}
	if err != nil {
		return Config{}, err
	}
	c.QuotaEnabled = qe != 0
	c.IPLimitEnabled = ile != 0
	c.ResetEnabled = re != 0
	return c, nil
}

// UpsertConfig 写回（或更新）节点策略配置。
func (s *Service) UpsertConfig(ctx context.Context, c Config) error {
	qe, ile, re := 0, 0, 0
	if c.QuotaEnabled {
		qe = 1
	}
	if c.IPLimitEnabled {
		ile = 1
	}
	if c.ResetEnabled {
		re = 1
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO node_policy(node_id,quota_enabled,quota_limit_bytes,"+
			"quota_reset_baseline,ip_limit_enabled,ip_limit_max,"+
			"reset_enabled,reset_day,reset_time,reset_next_at) "+
			"VALUES(?,?,?,?,?,?,?,?,?,?) "+
			"ON CONFLICT(node_id) DO UPDATE SET quota_enabled=excluded.quota_enabled,"+
			"quota_limit_bytes=excluded.quota_limit_bytes,"+
			"quota_reset_baseline=excluded.quota_reset_baseline,"+
			"ip_limit_enabled=excluded.ip_limit_enabled,"+
			"ip_limit_max=excluded.ip_limit_max,"+
			"reset_enabled=excluded.reset_enabled,"+
			"reset_day=excluded.reset_day,"+
			"reset_time=excluded.reset_time,"+
			"reset_next_at=excluded.reset_next_at",
		c.NodeID, qe, c.QuotaLimitBytes, c.QuotaResetBaseline, ile, c.IPLimitMax,
		re, c.ResetDay, c.ResetTime, c.ResetNextAt)
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

// DeleteNode 清理某节点的全部策略状态（配置 + slot + nft 对象）。
func (s *Service) DeleteNode(ctx context.Context, nodeID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM node_policy WHERE node_id=?", nodeID); err != nil {
		return err
	}
	// 与 reconcile 串行：直接改内存 map 必须与周期 reconcile 互斥。
	s.runMu.Lock()
	s.mu.Lock()
	delete(s.states, nodeID)
	delete(s.slots, nodeID)
	delete(s.online, nodeID)
	delete(s.appliedQuota, nodeID)
	delete(s.appliedIPLimit, nodeID)
	s.mu.Unlock()
	s.runMu.Unlock()
	// reconcile 会重建 nft（该节点已不在 list，规则自然清除）。
	return s.reconcile(ctx)
}
