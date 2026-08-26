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

	// IP Tracker：读 /proc 提取各节点当前活跃公网源 IP（TCP + UDP）。
	curIPs, _, err := connection.NodeRemoteIPs(nodeList, nil)
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
		slots := s.slots[id]
		if slots == nil {
			slots = map[string]int64{}
			s.slots[id] = slots
		}
		nowNs := s.now().UnixNano()
		cur := curIPs[id]

		// 1) touch：仍在线（在 /proc 里）的 slot IP 刷新 lastSeen。
		for ip := range cur {
			if _, ok := slots[ip]; ok {
				slots[ip] = nowNs
			}
		}
		// 2) purge：离线超过 TTL 的 slot 释放。
		for ip, last := range slots {
			if cur[ip] {
				continue
			}
			if nowNs-last > s.udpTTL.Nanoseconds() {
				delete(slots, ip)
			}
		}
		// 3) admit：新 IP 在 max 允许范围内才授予 slot。
		max := cfg.IPLimitMax
		if cfg.IPLimitEnabled && max >= 1 {
			for ip := range cur {
				if _, ok := slots[ip]; ok {
					continue
				}
				if len(slots) < max {
					slots[ip] = nowNs
				}
			}
		} else {
			// 未启用 IP limit：所有当前 IP 都视为活跃（仅作展示，不限制）。
			// 这里仍把 cur 里的 IP 放进 slots 以便 ActiveIPs 展示，但无 enforcement。
			for ip := range cur {
				if _, ok := slots[ip]; !ok {
					slots[ip] = nowNs
				}
			}
		}

		active := len(slots)
		st.ActiveIPs = active

		if cfg.IPLimitEnabled {
			st.IPLimitState = "ok"
			if max >= 1 && active >= max {
				st.IPLimitState = "exceeded"
				ipBlocked[id] = cloneIPSet(slots)
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

func cloneIPSet(m map[string]int64) map[string]bool {
	out := make(map[string]bool, len(m))
	for ip := range m {
		out[ip] = true
	}
	return out
}
