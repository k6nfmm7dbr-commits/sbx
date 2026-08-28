package policy

import (
	"context"
	"strconv"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// reconcile 执行一轮策略同步：
//  1. 读节点列表与策略配置；
//  2. 算每个节点的 quota used；
//  3. 读 conntrack（主）与 /proc（回退）采集客户端 IP 活动；
//  4. 更新 Slot Manager（observed → active → granted/rejected），严格 admission；
//  5. 用 granted 集合生成 nft allow set（Rejected 绝不进入）。
//
// 并发安全：runMu 串行化所有 reconcile 调用（Run goroutine 周期调用与
// API 保存/重置时的同步调用可能并发），否则并发写 map 会 fatal error。
func (s *Service) reconcile(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	nodeList := nodes.LoadPanelNodes(s.nodesPath())
	cfgs, err := s.loadConfigs(ctx)
	if err != nil {
		return err
	}

	now := s.now()

	// ---- IP 采集：conntrack 主 + /proc 回退 ----
	cr := connection.ConntrackResult{Available: false}
	if s.conntrack != nil {
		cr = s.conntrack("")
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

	active, candidates := s.buildActivity(nodeList, cr, procSplit, now)

	newStates := map[string]State{}
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
		st.ActiveTCPConn = activeTCPCount(nodeActive)

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
	}

	// 清理已删除节点的运行时状态（slot 由 flow tracker 的 GC 兜底；这里清 ipStates）。
	for id := range s.ipStates {
		if _, ok := newStates[id]; !ok {
			delete(s.ipStates, id)
		}
	}

	// 生成并应用 nft 规则（只影响达限节点）。
	if err := s.applyEnforcement(ctx, quotaBlocked, ipBlocked, nodeList); err != nil {
		return err
	}

	s.mu.Lock()
	s.states = newStates
	s.ready = true
	s.lastErr = ""
	s.appliedQuota = quotaBlocked
	s.appliedIPLimit = ipBlocked
	s.mu.Unlock()

	s.signalNotify()
	return nil
}

// buildActivity 产出每个节点的「活跃 IP」与「候选 IP」。
//
// conntrack 可用时：conntrack 是唯一 TCP 生命周期事实来源——ESTABLISHED/udp 流 →
// active；SYN_SENT/SYN_RECV 流 → candidate。绝不把 /proc 残留 ESTABLISHED 重新判活
// （否则断开的客户端会一直残留）。
//
// conntrack 不可用/读失败时：回退 /proc ESTABLISHED / connected。
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
		for _, f := range cr.Flows {
			nodeID := portNode[f.DstPort]
			if nodeID == "" {
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
				a := m[ip]
				a.IP = ip
				a.TCPSessions++
				m[ip] = a
			}
			for ip := range cur.UDP {
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
