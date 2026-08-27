package policy

import (
	"context"

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

	newStates := map[string]State{}
	newOnline := map[string]map[string]int64{}
	quotaBlocked := map[string]bool{}
	ipBlocked := map[string]map[string]bool{}

	for _, n := range nodeList {
		id := nodes.IDString(n)
		cfg := cfgs[id]

		// ---- 定时重置：到期立即把 baseline 抬到当前 lifetime，并推进下次时间 ----
		// 重置依附于配额：配额关闭时不做自动重置；day 非法（旧数据/异常配置）也跳过，
		// 宁可不动也不乱归零。
		if cfg.QuotaEnabled && cfg.ResetEnabled && cfg.ResetNextAt > 0 && ValidResetDay(cfg.ResetDay) && nowNs/1e9 >= cfg.ResetNextAt {
			tod := ParseResetTime(cfg.ResetTime)
			if tod < 0 {
				tod = 0
			}
			life, lerr := s.lifetimeBytes(ctx, id)
			if lerr != nil {
				return lerr
			}
			cfg.QuotaResetBaseline = life
			cfg.ResetNextAt = AdvanceNextPast(cfg.ResetNextAt, cfg.ResetDay, tod, s.location, nowNs/1e9)
			if uerr := s.UpsertConfig(ctx, cfg); uerr != nil {
				return uerr
			}
		}

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
			ResetEnabled: cfg.ResetEnabled,
			ResetDay:     cfg.ResetDay,
			ResetTime:    cfg.ResetTime,
			ResetNextAt:  cfg.ResetNextAt,
		}
		if cfg.QuotaEnabled {
			st.QuotaState = "ok"
			if cfg.QuotaLimitBytes > 0 && used >= cfg.QuotaLimitBytes {
				st.QuotaState = "exceeded"
				quotaBlocked[id] = true
			}
		}

		// ---- IP slot 更新 ----
		// slots 只存 UDP lastSeen；TCP 在线由本轮 cur.TCP 直接决定（断开即消失）。
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

		// 3) 构建 online 快照 = 本轮 TCP ∪ 未超 TTL 的 UDP。
		online := map[string]int64{}
		for ip := range cur.TCP {
			online[ip] = nowNs
		}
		for ip, last := range slots {
			online[ip] = last
		}
		newOnline[id] = online

		active := len(online)
		st.ActiveIPs = active

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
