package policy

import (
	"context"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// reconcile 执行一轮策略同步：
//  1. 读节点列表与策略配置；
//  2. 算每个节点的 quota used；
//  3. 读 /proc 更新 IP slot（授予新 slot、TTL 释放离线 slot）；
//  4. 决定需 enforcement 的节点集合；
//  5. 生成并应用 nft 规则（变化时才重写）。
func (s *Service) reconcile(ctx context.Context) error {
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

		// ---- IP slot 更新 ----
		// slots 只存 UDP lastSeen（UnixNano）。TCP 在线状态完全由本轮
		// cur.TCP 决定——ESTABLISHED 断开即从 /proc 消失，无需 TTL，
		// 因此 TCP IP 绝不写进 slots（否则会被误当作 UDP 活跃而滞留 120s）。
		slots := s.slots[id]
		if slots == nil {
			slots = map[string]int64{}
			s.slots[id] = slots
		}
		nowNs := s.now().UnixNano()
		udpTTLNs := s.udpTTL.Nanoseconds()
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

		// 3) 在线集合 = 本轮 TCP ∪ 未超 TTL 的 UDP。
		online := map[string]bool{}
		for ip := range cur.TCP {
			online[ip] = true
		}
		for ip := range slots {
			online[ip] = true
		}

		active := len(online)
		st.ActiveIPs = active

		if cfg.IPLimitEnabled {
			st.IPLimitState = "ok"
			if cfg.IPLimitMax >= 1 && active >= cfg.IPLimitMax {
				st.IPLimitState = "exceeded"
				ipBlocked[id] = online
			}
		}

		newStates[id] = st
	}

	// 清理已删除节点的 slot。
	for id := range s.slots {
		if _, ok := newStates[id]; !ok {
			delete(s.slots, id)
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

	return nil
}

// Reconcile 公开同步入口：API 保存策略后立即调用，使 nft 生效。
func (s *Service) Reconcile(ctx context.Context) error { return s.reconcile(ctx) }
