package policy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// selfIPsTTL 是本机地址集合的缓存时长（网卡增删/DHCP 换址后自动跟上）。
const selfIPsTTL = 30 * time.Second

// procSource 是 conntrack 不可用时的 /proc 回退数据源。
//
// 抽成包级变量有两个用途：测试可注入（与 s.remoteIPs 并行），以及让测试能
// 断言「conntrack 可用时根本不读 /proc」——这正是 v3.0.10 修掉的 CPU 浪费点
// （每秒对整张 /proc 连接表做 O(总连接数) 的无谓解析与分配）。
var procSource = connection.NodeRemoteIPsSplit

// reconcile 执行一轮策略同步：
//  1. 读节点列表（严格）与策略配置；
//  2. 读 conntrack（主）与 /proc（回退）采集客户端 IP 活动；
//  3. 更新 Slot Manager（observed → active → granted/rejected），严格 admission；
//  4. 用 paused 状态、限速和 granted 集合生成 nft enforcement；
//  5. 在 mu 下发布不可变快照（states / ipSnaps / activeIPs）。
//
// 并发安全（v3.0.6）：
//   - runMu 串行化所有 reconcile 调用，并且是 ipStates / flows 的唯一守卫；
//   - 读侧只看第 6 步发布的不可变快照，绝不遍历 ipStates。
//
// fail-closed：nodes.json 损坏时**保持上一轮 enforcement 不动**并返回错误，
// 绝不以「零节点」重写策略表（那会解除所有暂停/IP 阻断，而 sing-box 仍在服务）。
func (s *Service) reconcile(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	nodeList, err := nodes.LoadPanelNodesStrict(s.nodesFile)
	if err != nil {
		// 关键：不调用 applyEnforcement，不清空 states —— 上一轮阻断继续有效。
		return fmt.Errorf("nodes.json 不可用, 已保持上一轮策略 enforcement 不变: %w", err)
	}
	cfgs, err := s.loadConfigs(ctx)
	if err != nil {
		return err
	}

	alive := make(map[string]bool, len(nodeList))
	for _, n := range nodeList {
		alive[nodes.IDString(n)] = true
	}
	// 节点删除走 nodes CLI 的 candidate/commit，不经过 DeleteNode → 清孤儿行。
	s.purgeOrphanConfigs(ctx, alive, cfgs)

	now := s.now()

	// ---- IP 采集：conntrack 主 + /proc 回退 ----
	cr := connection.ConntrackResult{Available: false}
	if s.conntrack != nil {
		cr = s.conntrack("")
	}
	// conntrack 未激活跟踪（文件可读但整表 0 条）：ReadConntrack 已把
	// Available 置 false 并置 Inactive，这里只负责在状态变化时提示一次原因，
	// 避免每秒刷屏。判活会自然走 /proc 回退分支。
	//
	// 为什么必须回退：干净机器上没有任何引用 ct 的 netfilter 规则时，内核
	// 虽已加载 nf_conntrack 但不建条目。若把它当「可用且 0 条流」处理，
	// 在线 IP 会恒为 0，而连接数（读 /proc/net/tcp）却正常，用户看到
	// 「有连接数但在线 IP 是 0」。v3.0.8 起 GenNFT 会挂 sbx_ct 链主动激活
	// conntrack；此处是对旧规则集/规则被外部清空场景的兜底。
	if cr.Inactive != s.ctInactive {
		if cr.Inactive {
			slog.Warn("conntrack 已加载但未跟踪任何连接(缺少引用 ct 的 netfilter 规则), " +
				"在线 IP 判活已降级为 /proc；执行 sbx --apply-firewall 可重建 conntrack 激活链")
		} else {
			slog.Info("conntrack 已恢复跟踪, 在线 IP 判活回到 conntrack 口径")
		}
		s.ctInactive = cr.Inactive
	}

	var procSplit map[string]connection.RemoteIPSet
	procPartial := false
	// conntrack 可用时 /proc 回退数据源完全不会被消费（buildActivity 仅在
	// !cr.Available 分支使用 procSplit），早期实现却无条件读 4 个 /proc 文件
	// 并全量解析——繁忙服务器上每秒白付 O(总连接数) 的 CPU 与 GC 成本
	// （真机实测 4.1ms + 1.16MB 分配/秒，纯浪费）。这里仅在确需回退时读取。
	// partial 口径不变：cr.Available 时 cr.Partial/cr.Err 恒为 false。
	if !cr.Available || s.remoteIPs != nil {
		if s.remoteIPs != nil {
			procSplit, procPartial, err = s.remoteIPs(nodeList)
		} else {
			procSplit, procPartial, err = procSource(nodeList, nil)
		}
		if err != nil {
			return err
		}
	}

	// 采集结果「不完整」（conntrack 读失败 / Err 或 /proc partial）→ fail-safe：
	// 本轮不释放已有 slot。
	partial := procPartial || cr.Partial || cr.Err != nil

	s.refreshSelfIPs(now)
	active, candidates := s.buildActivity(nodeList, cr, procSplit, now)

	newStates := map[string]State{}
	newSnaps := map[string]NodeIPSnapshot{}
	newActiveIPs := map[string][]string{}
	newActiveTCP := map[string]int{}
	pausedNodes := map[string]bool{}
	ipBlocked := map[string]map[string]bool{}
	rateLimited := map[string]int{} // nodeID -> mbps

	for _, n := range nodeList {
		id := nodes.IDString(n)
		cfg := cfgs[id]

		st, paused, ipBlk, rate, snap, activeIPs := s.processNode(
			id, cfg, active, candidates, partial, now,
		)
		newStates[id] = st
		newActiveTCP[id] = activeTCPCount(active[id])
		if paused {
			pausedNodes[id] = true
		}
		if ipBlk != nil {
			ipBlocked[id] = ipBlk
		}
		if rate > 0 {
			rateLimited[id] = rate
		}
		newSnaps[id] = snap
		newActiveIPs[id] = activeIPs
	}

	// 清理已删除节点的运行时状态（flows 由 buildActivity 的 GC 兜底）。
	for id := range s.ipStates {
		if !alive[id] {
			delete(s.ipStates, id)
		}
	}

	// 生成并应用 nft 规则（只影响达限节点）。
	// enforceErr 不阻断状态发布：nft 应用暂时失败（权限/瞬时错误）时面板
	// 必须照常显示真实用量，并把错误如实呈现（policy_error），
	// 而不是让 states 永远为空、UI 全显示「不限」。
	enforceErr := s.applyEnforcement(ctx, pausedNodes, ipBlocked, rateLimited, nodeList)

	newErr := ""
	if enforceErr != nil {
		newErr = enforceErr.Error()
	}

	s.mu.Lock()
	// 采集器每轮都会刷新内部 flow 的 LastSeen，但只要对外快照没有改变，
	// 就不应递增 version / 唤醒所有 SSE / 让 /api/live 的缓存全部失效。
	// 空闲节点时这能把“每秒全量序列化与广播”降为真正有状态变化时才发生。
	changed := !s.ready || s.lastErr != newErr ||
		!sameStates(s.states, newStates) ||
		!sameNodeIPSnapshots(s.ipSnaps, newSnaps) ||
		!sameStringSlices(s.activeIPs, newActiveIPs) ||
		!sameIntMap(s.activeTCP, newActiveTCP)
	s.states = newStates
	s.ipSnaps = newSnaps
	s.activeIPs = newActiveIPs
	s.activeTCP = newActiveTCP
	s.ready = true
	if changed {
		s.version++
	}
	s.lastErr = newErr
	s.mu.Unlock()

	if changed {
		s.signalNotify()
	}
	return enforceErr
}

// sameStates / sameNodeIPSnapshots 比较已发布快照的对外内容。reconcile 内部
// 的 flow LastSeen、Observed.LastTraffic 等变化不直接暴露给 API；只有这些函数
// 观察到的内容变化时，才需要递增 version 并唤醒 SSE 客户端。
func sameStates(a, b map[string]State) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		if bv, ok := b[id]; !ok || av != bv {
			return false
		}
	}
	return true
}

func sameNodeIPSnapshots(a, b map[string]NodeIPSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		bv, ok := b[id]
		if !ok || av.NodeID != bv.NodeID || av.Limited != bv.Limited ||
			av.MaxIPs != bv.MaxIPs || av.Granted != bv.Granted ||
			!slices.Equal(av.IPs, bv.IPs) || !slices.Equal(av.Rejected, bv.Rejected) {
			return false
		}
	}
	return true
}

func sameStringSlices(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		bv, ok := b[id]
		if !ok || !slices.Equal(av, bv) {
			return false
		}
	}
	return true
}

func sameIntMap(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		if bv, ok := b[id]; !ok || av != bv {
			return false
		}
	}
	return true
}

// processNode 处理单个节点的策略状态：State 构造、Slot Manager admission、
// 快照生成。从 reconcile 的 per-node 循环提取，使主 reconcile 流程更紧凑、
// 单节点逻辑可独立测试。
//
// 返回 (state, paused, ipBlocked, rateMbps, snap, activeIPs)。
func (s *Service) processNode(
	id string,
	cfg Config,
	active map[string]map[string]IPActivity,
	candidates map[string]map[string]IPActivity,
	partial bool,
	now time.Time,
) (State, bool, map[string]bool, int, NodeIPSnapshot, []string) {
	st := State{
		Paused:       cfg.Paused,
		IPLimitOn:    cfg.IPLimitEnabled,
		IPLimitMax:   cfg.IPLimitMax,
		IPLimitState: "unlimited",
		RateLimitOn:  cfg.RateLimitEnabled && cfg.RateLimitMbps > 0,
		RateLimitMbps: func() int {
			if cfg.RateLimitEnabled {
				return cfg.RateLimitMbps
			}
			return 0
		}(),
	}
	paused := cfg.Paused
	rate := 0
	if cfg.RateLimitEnabled && cfg.RateLimitMbps > 0 {
		rate = cfg.RateLimitMbps
	}

	// ---- Slot Manager admission ----
	ipState := s.ipStates[id]
	if ipState == nil {
		ipState = newIPState()
		s.ipStates[id] = ipState
	}
	nodeActive := active[id]
	if nodeActive == nil {
		nodeActive = map[string]IPActivity{}
	}
	nodeCandidates := candidates[id]
	if nodeCandidates == nil {
		nodeCandidates = map[string]IPActivity{}
	}
	// partial：把已持有的 slot IP 补齐进 active，避免「不完整结果」误踢在线用户。
	if partial {
		for ip := range ipState.Slots {
			if _, ok := nodeActive[ip]; !ok {
				nodeActive[ip] = IPActivity{IP: ip}
			}
		}
	}

	maxIPs := 0
	if cfg.IPLimitEnabled {
		maxIPs = cfg.IPLimitMax
	}
	allowSet, hasRejected := ipState.reconcile(nodeActive, nodeCandidates, maxIPs, now, s.ipIdle, s.rejectedTTL, s.provisionalTTL, cfg.IPLimitEnabled)

	if cfg.IPLimitEnabled {
		if hasRejected {
			st.IPLimitState = "exceeded"
		} else {
			st.IPLimitState = "ok"
		}
		// 只有 granted（allowSet，含 provisional）进入 nft；Rejected 永不进入。
		// 返回 allowSet（非 nil）供 reconcile 上层构建 ipBlocked map。
		_ = allowSet
	}

	snap, activeIPs := buildNodeSnapshots(id, ipState)
	st.ActiveIPs = snap.Granted

	return st, paused, allowSet, rate, snap, activeIPs
}

// refreshSelfIPs 周期刷新本机地址集合（用于排除服务器自身发起的出站流）。
func (s *Service) refreshSelfIPs(now time.Time) {
	if s.selfIPs != nil && now.Sub(s.selfIPsAt) < selfIPsTTL {
		return
	}
	if s.localAddrs == nil {
		return
	}
	ips, err := s.localAddrs()
	if err != nil {
		slog.Debug("读取本机地址失败, 出站流过滤本轮降级", "err", err)
		if s.selfIPs == nil {
			s.selfIPs = map[string]bool{}
		}
		s.selfIPsAt = now
		return
	}
	s.selfIPs = ips
	s.selfIPsAt = now
}

// buildActivity 产出每个节点的「活跃 IP」与「候选 IP」。
//
// conntrack 可用时：conntrack 是唯一 TCP 生命周期事实来源——ESTABLISHED/udp 流 →
// active；SYN_SENT/SYN_RECV 流 → candidate。绝不把 /proc 残留 ESTABLISHED 重新判活
// （否则断开的客户端会一直残留）。
//
// 两条必须遵守的过滤/降级规则（都曾是线上 bug）：
//
//  1. 排除本机出站流：conntrack 原方向元组是 src=本机 dst=远端 dport=远端端口。
//     若节点监听 443/8443 这类常见端口，服务器自己 curl https:// 的流会被
//     误判成「该节点的客户端」，占 slot、虚报在线 IP，IP 限制下挤掉真实用户。
//     判据：SrcIP ∈ 本机地址集合 → 跳过。
//
//  2. 无字节计费的流按「conntrack 在跟踪即在线」处理：内核
//     net.netfilter.nf_conntrack_acct=0 时（Debian/Ubuntu 默认）
//     /proc/net/nf_conntrack 不输出 bytes= → Bytes 恒为 0 → 字节增量判活
//     永远判不出「有流量」→ ipIdle 到点后把正在使用的连接判死并踢出 allow set。
//
//     判据是**逐流**的 `f.Bytes == 0`，而不是全局开关。原因：运行中执行
//     `sysctl -w nf_conntrack_acct=1` 只对新建流生效，此前建立的流终生没有
//     计数；真机验证过这种混合状态。若用「全局全零才降级」，混合时那些老流
//     会被判死。acct 开启时 ESTABLISHED 流的 bytes 必然 > 0（握手包也算），
//     所以 Bytes==0 可以可靠地判定「这条流没有计费数据」。
//
//     全局探测仍保留，但只用于打一次提示日志（告诉用户开 sysctl 更精确）。
//
// activityPortIndex 返回当前 nodes.json 的端口归属索引。strict loader 命中时会复用
// 同一个不可变 []Node 底层数组，因此可以用首元素地址 + 长度判断输入是否未变；
// 文件原子替换后 loader 产生新 slice，自动重建。该索引只由 runMu 下的 reconcile
// 调用，字段无需额外锁。
func (s *Service) activityPortIndex(list []nodes.Node) map[int]string {
	if len(list) == len(s.activityNodes) && (len(list) == 0 || &list[0] == &s.activityNodes[0]) {
		return s.activityPortNode
	}
	portNode := make(map[int]string, len(list))
	for _, n := range list {
		id := nodes.IDString(n)
		for _, r := range nodes.ParsePorts(n) {
			for p := int(r[0]); p <= int(r[1]); p++ {
				portNode[p] = id
			}
		}
	}
	s.activityNodes = list
	s.activityPortNode = portNode
	return portNode
}

// buildActivity 产出每个节点的活跃 IP 与候选 IP。
func (s *Service) buildActivity(nodeList []nodes.Node, cr connection.ConntrackResult, procSplit map[string]connection.RemoteIPSet, now time.Time) (map[string]map[string]IPActivity, map[string]map[string]IPActivity) {
	portNode := s.activityPortIndex(nodeList)

	active := map[string]map[string]IPActivity{}
	candidates := map[string]map[string]IPActivity{}
	agg := func(dst map[string]map[string]IPActivity, nodeID string) map[string]IPActivity {
		m := dst[nodeID]
		if m == nil {
			m = map[string]IPActivity{}
			dst[nodeID] = m
		}
		return m
	}

	// 用持久 flowState.SeenEpoch 标记本轮出现的 flow，代替每轮分配
	// currentFlowKeys（2500 flow 下约数千条临时 map entry）。
	s.flowEpoch++
	if s.flowEpoch == 0 { // uint64 回绕（理论上 5840 亿年）；避免旧 marker 碰撞
		for k, fs := range s.flows {
			fs.SeenEpoch = 0
			s.flows[k] = fs
		}
		s.flowEpoch = 1
	}
	flowEpoch := s.flowEpoch

	if cr.Available {
		// 全局计费提示与 flow 判活共用同一遍遍历，避免稳态每秒重复扫整个
		// conntrack 表（高并发机可有数万行）。计数过滤口径与原探测一致：
		// 只统计目标是节点端口、且源 IP 不是本机的 flow。
		relevant, withBytes := 0, 0
		for _, f := range cr.Flows {
			nodeID := portNode[f.DstPort]
			if nodeID == "" {
				continue
			}
			// 规则 1：本机自身发起的出站流不是客户端。
			if s.selfIPs[f.SrcIP] {
				continue
			}
			relevant++
			if f.Bytes != 0 {
				withBytes++
			}
			// 候选：TCP 握手尚未完成。
			if f.Proto == "tcp" && (f.State == "SYN_SENT" || f.State == "SYN_RECV") {
				m := agg(candidates, nodeID)
				a := m[f.SrcIP]
				a.IP = f.SrcIP
				a.TCPSessions++
				m[f.SrcIP] = a
				continue
			}

			// 活跃流：tcp ESTABLISHED 或 udp。
			// 结构体 key 避免每轮拼接 nodeID + IP + port 字符串；只在首次
			// 插入持久 flow map 时 Clone IP，避免保存到 flowKey 的 IP 子串
			// 将整个 conntrack 文件缓冲区一并保活。
			fkey := flowKey{nodeID: nodeID, srcIP: f.SrcIP, srcPort: f.SrcPort}
			prev, had := s.flows[fkey]
			traffic := false
			switch {
			case f.Bytes == 0:
				// 无字节计费：conntrack 仍跟踪就视为活跃，并刷新 LastSeen。
				if !had {
					fkey.srcIP = strings.Clone(f.SrcIP)
				}
				s.flows[fkey] = flowState{Bytes: f.Bytes, LastSeen: now, SeenEpoch: flowEpoch}
				traffic = true
			case !had || f.Bytes != prev.Bytes:
				if !had {
					fkey.srcIP = strings.Clone(f.SrcIP)
				}
				s.flows[fkey] = flowState{Bytes: f.Bytes, LastSeen: now, SeenEpoch: flowEpoch}
				traffic = true
			case now.Sub(prev.LastSeen) <= s.ipIdle:
				// 静默但仍在 grace → 活跃，沿用原始 LastSeen（不刷新 grace）。
				prev.SeenEpoch = flowEpoch
				s.flows[fkey] = prev
			default:
				// flow 仍存在于 conntrack，但字节静默超过 idle：仍保留 tracker，
				// 与旧 currentFlowKeys 语义一致，只不把它计入 active。
				prev.SeenEpoch = flowEpoch
				s.flows[fkey] = prev
				continue
			}

			m := agg(active, nodeID)
			a := m[f.SrcIP]
			a.IP = f.SrcIP
			if f.Proto == "tcp" {
				a.TCPSessions++
			} else {
				a.UDPSessions++
			}
			if traffic {
				a.Traffic = true
			}
			m[f.SrcIP] = a
		}
		if relevant > 0 {
			acctOff := withBytes == 0
			if acctOff != s.acctDisabled {
				if acctOff {
					slog.Warn("检测到 nf_conntrack 未开启字节计费(nf_conntrack_acct=0)，" +
						"已降级为「ESTABLISHED 即在线」；建议执行 " +
						"sysctl -w net.netfilter.nf_conntrack_acct=1 以恢复精确判活")
				} else {
					slog.Info("nf_conntrack 字节计费已可用，恢复字节增量判活")
				}
			}
			s.acctDisabled = acctOff
		}
	} else if procSplit != nil {
		// conntrack 不可用：回退 /proc。
		for _, n := range nodeList {
			id := nodes.IDString(n)
			cur, ok := procSplit[id]
			if !ok {
				continue
			}
			m := agg(active, id)
			for ip := range cur.TCP {
				if s.selfIPs[ip] {
					continue
				}
				a := m[ip]
				a.IP = ip
				a.TCPSessions++
				m[ip] = a
			}
			for ip := range cur.UDP {
				if s.selfIPs[ip] {
					continue
				}
				a := m[ip]
				a.IP = ip
				a.UDPSessions++
				m[ip] = a
			}
		}
	}

	// flow tracker GC：conntrack 快照中本轮未出现且超空闲的 flow 清理。
	for k, fs := range s.flows {
		if fs.SeenEpoch != flowEpoch && now.Sub(fs.LastSeen) > s.ipIdle {
			delete(s.flows, k)
		}
	}

	return active, candidates
}

// Reconcile 公开同步入口：API 保存策略后立即调用，使 nft 生效。
func (s *Service) Reconcile(ctx context.Context) error { return s.reconcile(ctx) }

// activeTCPCount 统计某节点「已建立」的活跃 TCP 会话数（不含候选 SYN）。
func activeTCPCount(active map[string]IPActivity) int {
	n := 0
	for _, a := range active {
		n += a.TCPSessions
	}
	return n
}
