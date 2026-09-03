package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// selfIPsTTL 是本机地址集合的缓存时长（网卡增删/DHCP 换址后自动跟上）。
const selfIPsTTL = 30 * time.Second

// reconcile 执行一轮策略同步：
//  1. 读节点列表（严格）与策略配置；
//  2. 算每个节点的 quota used（单条 totals 查询）；
//  3. 读 conntrack（主）与 /proc（回退）采集客户端 IP 活动；
//  4. 更新 Slot Manager（observed → active → granted/rejected），严格 admission；
//  5. 用 granted 集合生成 nft allow set（Rejected 绝不进入）；
//  6. 在 mu 下发布不可变快照（states / ipSnaps / activeIPs）。
//
// 并发安全（v3.0.6）：
//   - runMu 串行化所有 reconcile 调用，并且是 ipStates / flows 的唯一守卫；
//   - 读侧只看第 6 步发布的不可变快照，绝不遍历 ipStates。
//
// fail-closed：nodes.json 损坏时**保持上一轮 enforcement 不动**并返回错误，
// 绝不以「零节点」重写策略表（那会解除所有配额/IP 阻断，而 sing-box 仍在服务）。
func (s *Service) reconcile(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	nodeList, err := nodes.LoadPanelNodesStrict(s.nodesPath())
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
	if s.remoteIPs != nil {
		procSplit, procPartial, err = s.remoteIPs(nodeList)
	} else {
		procSplit, procPartial, err = connection.NodeRemoteIPsSplit(nodeList, nil)
	}
	if err != nil {
		return err
	}

	// 采集结果「不完整」（conntrack 读失败 / Err 或 /proc partial）→ fail-safe：
	// 本轮不释放已有 slot。
	partial := procPartial || cr.Partial || cr.Err != nil

	s.refreshSelfIPs(now)
	active, candidates := s.buildActivity(nodeList, cr, procSplit, now)

	// 单条查询取全部节点 lifetime（旧实现每节点一次 SELECT）。
	lifetimes, err := s.lifetimeBytesAll(ctx)
	if err != nil {
		return err
	}

	newStates := map[string]State{}
	newSnaps := map[string]NodeIPSnapshot{}
	newActiveIPs := map[string][]string{}
	newActiveTCP := map[string]int{}
	quotaBlocked := map[string]bool{}
	ipBlocked := map[string]map[string]bool{}

	for _, n := range nodeList {
		id := nodes.IDString(n)
		cfg := cfgs[id]

		life := lifetimes[id]
		// 自愈（defense-in-depth，主修复在 service.Reset 的同事务清零）：
		// totals 只会单调增长（commitTick 全是 rx=rx+delta），因此
		// baseline > lifetime 只可能是统计被清空过。此时历史已丢，唯一诚实的
		// 口径是「把 totals 里现有的量全算作已用」→ 基线归零。
		//
		// 不能校正为 lifetime：那会把 reset 之后已经跑掉的流量一并抹掉，
		// 配额继续失效；归零则偏向「多算用量、配额更早生效」，方向正确。
		if cfg.QuotaResetBaseline > life {
			slog.Info("配额基线高于累计流量(统计被重置?), 已将基线归零",
				"node", id, "baseline", cfg.QuotaResetBaseline, "lifetime", life)
			if err := s.setResetBaseline(ctx, id, 0); err != nil {
				return err
			}
			cfg.QuotaResetBaseline = 0
			cfgs[id] = cfg
		}
		used := life - cfg.QuotaResetBaseline
		if used < 0 {
			used = 0
		}

		st := State{
			QuotaEnabled: cfg.QuotaEnabled,
			QuotaLimit:   cfg.QuotaLimitBytes,
			QuotaUsed:    used,
			QuotaState:   "unlimited",
			IPLimitOn:    cfg.IPLimitEnabled,
			IPLimitMax:   cfg.IPLimitMax,
			IPLimitState: "unlimited",
		}
		if cfg.QuotaEnabled {
			st.QuotaState = "ok"
			if cfg.QuotaLimitBytes > 0 && used >= cfg.QuotaLimitBytes {
				st.QuotaState = "exceeded"
				quotaBlocked[id] = true
			}
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
		allowSet, hasRejected := ipState.Reconcile(nodeActive, nodeCandidates, maxIPs, now, s.ipIdle, s.rejectedTTL, s.provisionalTTL)

		// 「在线 IP」= 已建立（非 provisional）的 granted 数量。
		st.ActiveIPs = ipState.activeGrantedCount()
		newActiveTCP[id] = activeTCPCount(nodeActive)

		if cfg.IPLimitEnabled {
			if hasRejected {
				st.IPLimitState = "exceeded"
			} else {
				st.IPLimitState = "ok"
			}
			// 只有 granted（allowSet，含 provisional）进入 nft；Rejected 永不进入。
			ipBlocked[id] = allowSet
		}

		newStates[id] = st
		newSnaps[id] = buildNodeIPSnapshot(id, ipState)
		newActiveIPs[id] = buildActiveIPsFromState(ipState)
	}

	// 清理已删除节点的运行时状态（flows 由 buildActivity 的 GC 兜底）。
	for id := range s.ipStates {
		if !alive[id] {
			delete(s.ipStates, id)
		}
	}

	// 生成并应用 nft 规则（只影响达限节点）。
	// enforceErr 不阻断状态发布：iptables-only 主机上策略无法执行，但面板
	// 必须照常显示真实用量，并把「不支持」如实呈现，而不是每秒失败一次
	// 导致 states 永远为空、UI 全显示「不限」。
	enforceErr := s.applyEnforcement(ctx, quotaBlocked, ipBlocked, nodeList)

	s.mu.Lock()
	s.states = newStates
	s.ipSnaps = newSnaps
	s.activeIPs = newActiveIPs
	s.activeTCP = newActiveTCP
	s.ready = true
	if enforceErr != nil {
		s.lastErr = enforceErr.Error()
	} else {
		s.lastErr = ""
	}
	s.mu.Unlock()

	s.signalNotify()

	// ErrEnforceUnsupported 不上抛：它是「后端能力缺失」的稳态提示，
	// 已经经 lastErr 完整呈现给面板；上抛会让 Run 循环每秒刷一条 WARN，
	// 且让「保存策略」类 API 在配置已落库、状态已发布的情况下仍返回 500。
	// 其余 enforcement 错误（nft 执行失败等瞬态故障）继续上抛，
	// 调用方需要知道本轮应用没有成功。
	if errors.Is(enforceErr, ErrEnforceUnsupported) {
		return nil
	}
	return enforceErr
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
func (s *Service) buildActivity(nodeList []nodes.Node, cr connection.ConntrackResult, procSplit map[string]connection.RemoteIPSet, now time.Time) (map[string]map[string]IPActivity, map[string]map[string]IPActivity) {
	portNode := map[int]string{}
	for _, n := range nodeList {
		id := nodes.IDString(n)
		for _, r := range nodes.ParsePorts(n) {
			for p := int(r[0]); p <= int(r[1]); p++ {
				portNode[p] = id
			}
		}
	}

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

	currentFlowKeys := map[string]bool{}

	if cr.Available {
		// 全局探测：仅用于提示用户开启 sysctl（判活本身是逐流的，见下）。
		relevant, withBytes := 0, 0
		for _, f := range cr.Flows {
			if portNode[f.DstPort] == "" || s.selfIPs[f.SrcIP] {
				continue
			}
			relevant++
			if f.Bytes != 0 {
				withBytes++
			}
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

		for _, f := range cr.Flows {
			nodeID := portNode[f.DstPort]
			if nodeID == "" {
				continue
			}
			// 规则 1：本机自身发起的出站流不是客户端。
			if s.selfIPs[f.SrcIP] {
				continue
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
			fkey := nodeID + "\x00" + f.SrcIP + ":" + strconv.Itoa(f.SrcPort)
			currentFlowKeys[fkey] = true
			traffic := false
			switch {
			case f.Bytes == 0:
				// 规则 2：这条流没有计费数据 → 无从判断流量增减，
				// conntrack 仍在跟踪就视为活跃（宁可多留，不误踢在用连接）。
				s.flows[fkey] = &flowState{Bytes: f.Bytes, LastSeen: now}
				traffic = true
			default:
				prev := s.flows[fkey]
				switch {
				case prev == nil:
					s.flows[fkey] = &flowState{Bytes: f.Bytes, LastSeen: now}
					traffic = true
				case f.Bytes != prev.Bytes:
					s.flows[fkey] = &flowState{Bytes: f.Bytes, LastSeen: now}
					traffic = true
				case now.Sub(prev.LastSeen) <= s.ipIdle:
					// 静默但仍在 grace → 活跃
				default:
					continue // 死连接：整条流不活跃
				}
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

	// flow tracker GC：不在本轮且超空闲的流清理，防 map 无限增长。
	for k, fs := range s.flows {
		if !currentFlowKeys[k] && now.Sub(fs.LastSeen) > s.ipIdle {
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
