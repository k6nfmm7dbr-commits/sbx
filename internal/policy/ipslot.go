package policy

import (
	"sort"
	"time"
)

// IPActivity 是单轮采集得到的某个客户端 IP 的活跃摘要（TCP/UDP 合并）。
type IPActivity struct {
	IP          string
	TCPSessions int
	UDPSessions int
	Traffic     bool // 本轮判定有流量（conntrack 字节有增量）
}

// IPSlot 已获得使用资格（granted）的 IP，允许通过防火墙使用节点。
type IPSlot struct {
	IP          string
	GrantedAt   time.Time
	LastSeen    time.Time
	LastTraffic time.Time
	TCP         bool
	UDP         bool
}

// ObservedIP 采集层观察到的 IP（可能未获得 slot，也可能已被拒绝）。
type ObservedIP struct {
	IP          string
	FirstSeen   time.Time
	LastSeen    time.Time
	LastTraffic time.Time
	TCPSessions int
	UDPSessions int
}

// NodeIPState 单节点 IP 状态：Slots=Granted，Observed=发现过，Rejected=被拒。
type NodeIPState struct {
	Slots    map[string]*IPSlot
	Observed map[string]*ObservedIP
	Rejected map[string]time.Time // ip -> 拒绝时刻
	MaxIPs   int
}

func newIPState() *NodeIPState {
	return &NodeIPState{
		Slots:    map[string]*IPSlot{},
		Observed: map[string]*ObservedIP{},
		Rejected: map[string]time.Time{},
	}
}

func lastActive(ip string, lastSeen, lastTraffic time.Time) time.Time {
	if lastTraffic.After(lastSeen) {
		return lastTraffic
	}
	return lastSeen
}

// Reconcile 执行一轮严格 admission。
//
// active 是「本轮判活为在线」的 IP 集合（不含被 /proc partial 影响的误判）。
// maxIPs<=0 表示不限制（全部活跃 IP 授予 slot，但仅用于「在线 IP」显示，
// 不应用 allow set）。
//
// 规则：
//   - 已持有 slot 的 IP 优先保留并 refresh；
//   - 新 IP 仅在 len(granted) < maxIPs 时授予；
//   - 超出上限的 IP 标记 rejected，绝不进入 granted；
//   - 离线（本轮未出现 + 距上次活跃超过 idle）的 slot 释放，下轮才能补位。
//
// 返回 granted 集合（供 nft allow set）与是否有新拒绝。
func (st *NodeIPState) Reconcile(active map[string]IPActivity, maxIPs int, now time.Time, idle, rejectedTTL time.Duration) (granted map[string]bool, hasRejected bool) {
	st.MaxIPs = maxIPs

	// 1) 更新 Observed。
	seen := map[string]bool{}
	order := make([]string, 0, len(active))
	for ip, a := range active {
		o, ok := st.Observed[ip]
		if !ok {
			o = &ObservedIP{IP: ip, FirstSeen: now}
			st.Observed[ip] = o
		}
		o.LastSeen = now
		o.TCPSessions = a.TCPSessions
		o.UDPSessions = a.UDPSessions
		if a.Traffic {
			o.LastTraffic = now
		}
		seen[ip] = true
		order = append(order, ip)
	}

	// 2) 拒绝表 TTL 清理（防端口扫描造成无限增长）。
	for ip, t := range st.Rejected {
		if now.Sub(t) > rejectedTTL {
			delete(st.Rejected, ip)
		}
	}

	// 3) 释放离线 slot：本轮未出现 + 距上次活跃超过 idle。
	for ip, slot := range st.Slots {
		if seen[ip] {
			continue
		}
		if now.Sub(lastActive(ip, slot.LastSeen, slot.LastTraffic)) > idle {
			delete(st.Slots, ip)
		}
	}

	// 4) Admission：已持有优先，其余按 FirstSeen 先到先得（同轮内按 IP 字典序兜底保证确定性）。
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		_, aHas := st.Slots[a]
		_, bHas := st.Slots[b]
		if aHas != bHas {
			return aHas
		}
		fa, fb := st.Observed[a].FirstSeen, st.Observed[b].FirstSeen
		if !fa.Equal(fb) {
			return fa.Before(fb)
		}
		return a < b
	})

	for _, ip := range order {
		a := active[ip]
		if slot, ok := st.Slots[ip]; ok {
			slot.LastSeen = now
			if a.TCPSessions > 0 {
				slot.TCP = true
			}
			if a.UDPSessions > 0 {
				slot.UDP = true
			}
			if a.Traffic {
				slot.LastTraffic = now
			}
			continue
		}
		if maxIPs <= 0 || len(st.Slots) < maxIPs {
			st.Slots[ip] = &IPSlot{IP: ip, GrantedAt: now, LastSeen: now, TCP: a.TCPSessions > 0, UDP: a.UDPSessions > 0}
			if a.Traffic {
				st.Slots[ip].LastTraffic = now
			}
		} else {
			st.Rejected[ip] = now
			hasRejected = true
		}
	}

	// 5) granted = 所有仍持有的 slot（含 grace 中未释放的，保证断线重连不误踢）。
	granted = make(map[string]bool, len(st.Slots))
	for ip := range st.Slots {
		granted[ip] = true
	}

	// 6) Observed GC：无 slot、无拒绝、且久未活跃的观察项清理，防无限增长。
	for ip, o := range st.Observed {
		if _, ok := st.Slots[ip]; ok {
			continue
		}
		if _, ok := st.Rejected[ip]; ok {
			continue
		}
		if now.Sub(lastActive(ip, o.LastSeen, o.LastTraffic)) > idle {
			delete(st.Observed, ip)
		}
	}
	return granted, hasRejected
}

// grantedCount 返回当前持有 slot 的 IP 数。
func (st *NodeIPState) grantedCount() int { return len(st.Slots) }
