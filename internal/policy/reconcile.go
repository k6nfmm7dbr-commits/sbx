package policy

import (
	"context"
	"strconv"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// reconcile 执行一轮策略同步：
//  1. 读节点列表与策略配置；
//  2. 算每个节点的 quota used；
//  3. 读 /proc 更新 IP slot 与在线 IP 快照（TCP 立即、UDP TTL）；
//  4. 决定需 enforcement 的节点集合；
//  5. 生成并应用 nft 规则（变化时才重写）。
//
// 并发安全：runMu 串行化所有 reconciler 调用（Run goroutine 周期调用与
// API 保存/重置时的同步调用可能并发），否则并发写 map 会 fatal error。
func (s *Service) reconcile(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	nodeList := nodes.LoadPanelNodes(s.nodesPath())
	cfgs, err := s.loadConfigs(ctx)
	if err != nil {
		return err
	}

	// IP Tracker：读 /proc 提取各节点当前活跃公网源 IP（TCP/UDP 分离）。
	var curSplit map[string]connection.RemoteIPSet
	if s.remoteIPs != nil {
		curSplit, _, err = s.remoteIPs(nodeList)
	} else {
		curSplit, _, err = connection.NodeRemoteIPsSplit(nodeList, nil)
	}
	if err != nil {
		return err
	}

	nowNs := s.now().UnixNano()
	udpTTLNs := s.udpTTL.Nanoseconds()

	// conntrack 读一次，用于 TCP 连活判定（有近期流量才算在线/算连接数）。
	var flows []connection.ConntrackFlow
	if s.conntrack != nil {
		flows = s.conntrack("")
	}
	// conntrack 不可用时回退 /proc ESTABLISHED 数作为「活跃连接数」。
	var procConns map[string]connection.Conns
	if len(flows) == 0 {
		if cr, cerr := connection.CountForNodes(nodeList); cerr == nil {
			procConns = cr.Conns
		} else {
			procConns = map[string]connection.Conns{}
		}
	}

	newStates := map[string]State{}
	newOnline := map[string]map[string]int64{}
	quotaBlocked := map[string]bool{}
	ipBlocked := map[string]map[string]bool{}

	for _, n := range nodeList {
		id := nodes.IDString(n)
		cfg := cfgs[id]

		life, err := s.lifetimeBytes(ctx, id)
		if err != nil {
			return err
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

		// ---- IP slot 更新 ----
		// slots 只存 UDP lastSeen；TCP 在线由「conntrack 字节增判」决定：
		// 有近期流量的 ESTABLISHED 连接才算在线，客户端异常断开后无流量即可回落。
		if s.slots[id] == nil {
			s.slots[id] = map[string]int64{}
		}
		slots := s.slots[id]
		cur := curSplit[id]

		// 1) touch：UDP 活跃的 IP 刷新 lastSeen。
		for ip := range cur.UDP {
			slots[ip] = nowNs
		}
		// 2) purge：UDP lastSeen 超 TTL 的释放。
		for ip, last := range slots {
			if nowNs-last > udpTTLNs {
				delete(slots, ip)
			}
		}

		// 3) 本节点端口集合 + TCP 连活判定。
		nodePorts := map[int]bool{}
		ranges := nodes.ParsePorts(n)
		for _, r := range ranges {
			for p := int(r[0]); p <= int(r[1]); p++ {
				nodePorts[p] = true
			}
		}

		activeTCP := map[string]bool{}
		activeConn := 0
		hasFlow := map[string]bool{} // cur.TCP 中「有 conntrack 流」的 IP

		if len(flows) > 0 {
			// 只统计「命中本节点监听端口、且源 IP 确实在 /proc ESTABLISHED 里」的流。
			// 用 SrcIP ∈ cur.TCP 排除服务器自身发起的 outbound 流（其源 IP 是本机，不在客户端集合里）。
			for _, f := range flows {
				if f.Proto != "tcp" || !nodePorts[f.DstPort] || !cur.TCP[f.SrcIP] {
					continue
				}
				hasFlow[f.SrcIP] = true
				fkey := id + "\x00" + f.SrcIP + ":" + strconv.Itoa(f.SrcPort)
				if last, ok := s.flowBytes[fkey]; ok {
					if f.Bytes != last {
						// 字节变化：有流量（或断线重连导致计数重置）→ 活跃
						s.flowBytes[fkey] = f.Bytes
						s.flowSeen[fkey] = nowNs
					} else if nowNs-s.flowSeen[fkey] > s.tcpIdle.Nanoseconds() {
						// 长期无流量 → 死连接，不计入在线
						continue
					}
				} else {
					s.flowBytes[fkey] = f.Bytes
					s.flowSeen[fkey] = nowNs
				}
				activeTCP[f.SrcIP] = true
				activeConn++
			}
			// cur.TCP 里没有 conntrack 流的 IP（本地连接 / 未走 conntrack）保守视为在线。
			for ip := range cur.TCP {
				if !hasFlow[ip] {
					activeTCP[ip] = true
				}
			}
		} else {
			// conntrack 不可用：回退 ESTABLISHED 即在线。
			for ip := range cur.TCP {
				activeTCP[ip] = true
			}
			if c, ok := procConns[id]; ok && c.TCP != nil {
				activeConn = *c.TCP
			}
		}

		// 4) 构建 online 快照 = 活跃 TCP IP ∪ 未超 TTL 的 UDP。
		online := map[string]int64{}
		for ip := range activeTCP {
			online[ip] = nowNs
		}
		for ip, last := range slots {
			online[ip] = last
		}
		newOnline[id] = online

		active := len(online)
		st.ActiveIPs = active
		st.ActiveTCPConn = activeConn

		if cfg.IPLimitEnabled {
			st.IPLimitState = "ok"
			if cfg.IPLimitMax >= 1 && active >= cfg.IPLimitMax {
				st.IPLimitState = "exceeded"
				ipBlocked[id] = cloneIPSets(online)
			}
		}

		newStates[id] = st
	}

	// 清理已删除节点的运行时状态。
	for id := range s.slots {
		if _, ok := newStates[id]; !ok {
			delete(s.slots, id)
		}
	}
	for id := range s.online {
		if _, ok := newStates[id]; !ok {
			delete(s.online, id)
		}
	}

	// 生成并应用 nft 规则（只影响达限节点）。
	if err := s.applyEnforcement(ctx, quotaBlocked, ipBlocked, nodeList); err != nil {
		return err
	}

	s.mu.Lock()
	s.states = newStates
	s.online = newOnline
	s.ready = true
	s.lastErr = ""
	s.appliedQuota = quotaBlocked
	s.appliedIPLimit = ipBlocked
	s.mu.Unlock()

	return nil
}

// Reconcile 公开同步入口：API 保存策略后立即调用，使 nft 生效。
func (s *Service) Reconcile(ctx context.Context) error { return s.reconcile(ctx) }

// cloneIPSets 把 online 快照（ip -> lastSeen）转成 allow set（ip -> true）。
func cloneIPSets(m map[string]int64) map[string]bool {
	out := make(map[string]bool, len(m))
	for ip := range m {
		out[ip] = true
	}
	return out
}
