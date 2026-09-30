package policy

import (
	"sort"
	"strings"
	"time"
)

// IPActivity 是单轮采集得到的某个客户端 IP 的活动摘要（TCP/UDP 合并）。
type IPActivity struct {
	IP          string
	TCPSessions int
	UDPSessions int
	Traffic     bool // 本轮判定有流量（conntrack 字节有增量）
}

// IPSlot 已获得使用资格（granted）的 IP：既可能是已确认在线（ESTABLISHED），
// 也可能是候选（刚发 SYN 尚未完成握手）的 provisional 授予。
type IPSlot struct {
	IP          string
	GrantedAt   time.Time
	LastSeen    time.Time
	LastTraffic time.Time
	TCP         bool
	UDP         bool
	// Provisional 为 true 表示该 slot 是「候选 → 临时授予」，握手尚未完成；
	// 若超过 provisionalTTL 仍未建立，将被释放。
	Provisional bool
	CandidateAt time.Time
}

// ObservedIP 采集层观察到的 IP。
type ObservedIP struct {
	IP          string
	FirstSeen   time.Time
	LastSeen    time.Time
	LastTraffic time.Time
	TCPSessions int
	UDPSessions int
}

// NodeIPState 单节点 IP 状态：
//
//	Slots    = Granted（合法使用资格，allow set 的权威来源）
//	Observed = 发现过（含候选/扫描）
//	Rejected = 因超出上限被拒绝
type NodeIPState struct {
	Slots    map[string]*IPSlot
	Observed map[string]*ObservedIP
	Rejected map[string]time.Time
	MaxIPs   int
}

func newIPState() *NodeIPState {
	return &NodeIPState{
		Slots:    map[string]*IPSlot{},
		Observed: map[string]*ObservedIP{},
		Rejected: map[string]time.Time{},
	}
}

func lastActive(lastSeen, lastTraffic time.Time) time.Time {
	if lastTraffic.After(lastSeen) {
		return lastTraffic
	}
	return lastSeen
}

// Reconcile 执行一轮严格 admission（原子：检查容量 + 授予 + 更新内存在同一调用内完成）。
//
// active 是「已建立并判活在线」的 IP；candidates 是「发起握手但尚未 ESTABLISHED」的 IP。
// maxIPs<=0 表示不限制。
//
//   - 已持有 slot 优先保留；
//   - active 优先于 candidate；
//   - 候选在有名额时 provisional 授予（进 allow set，让握手完成）；
//   - 候选超过 provisionalTTL 仍未建立 → 释放 provisional slot；
//   - 超出上限的（actives 与 candidates）→ Rejected，绝不进 allow set。
//
// 返回 allowSet（所有 slot，含 provisional）与是否有新拒绝。
func (st *NodeIPState) Reconcile(active, candidates map[string]IPActivity, maxIPs int, now time.Time, idle, rejectedTTL, provisionalTTL time.Duration) (allowSet map[string]bool, hasRejected bool) {
	st.MaxIPs = maxIPs

	type want struct {
		ip     string
		active bool
	}
	all := make([]want, 0, len(active)+len(candidates))

	// 1) 更新 Observed（active 与 candidate 都算「见过」）。active/candidates
	// 本身已经是去重 map；再额外构造一个 seen map 只为记录同样的 membership
	// 会多一次每 IP 的写入与整张临时 map。候选重叠直接查 active，slot 保留则查
	// 两个源 map，消除该临时集合而不改变去重/释放语义。
	for ip, a := range active {
		stableIP := st.touchObserved(ip, a, now)
		all = append(all, want{ip: stableIP, active: true})
	}
	for ip, a := range candidates {
		if _, ok := active[ip]; ok {
			continue // 已在 active 里
		}
		stableIP := st.touchObserved(ip, a, now)
		all = append(all, want{ip: stableIP, active: false})
	}

	// 2) Rejected TTL 清理。
	for ip, t := range st.Rejected {
		if now.Sub(t) > rejectedTTL {
			delete(st.Rejected, ip)
		}
	}

	// 3) 释放 slot：本轮不在 active（conntrack 已无 ESTABLISHED 流 = 确认关闭/FIN/RST/超时）
	// 立即释放；grace/dead 由 buildActivity 的字节增量判活承担（半开 ESTABLISHED 仍保留）。
	// released 记录本轮刚释放的 IP，admission 不得在同一轮立刻 re-grant。
	released := map[string]bool{}
	for ip, slot := range st.Slots {
		_, isActive := active[ip]
		_, isCandidate := candidates[ip]
		if !isActive && !isCandidate {
			delete(st.Slots, ip)
			released[ip] = true
			continue
		}
		if slot.Provisional && now.Sub(slot.CandidateAt) > provisionalTTL {
			delete(st.Slots, ip)
			released[ip] = true
		}
	}

	// 4) 只有出现「没有 slot、且非本轮刚释放」的新 IP 时才需要排序。
	// 稳态每秒重复看到的 IP 已全部持有 slot，admission 顺序不会改变任何结果，
	// 原先仍对所有 IP 排序（50 个在线 IP/node × 50 节点 → 每秒约 15k 次
	// comparator/map lookup）。现在稳态跳过 sort；发生新 IP admission 时则保留
	// 完全相同的优先级与 FirstSeen/IP tie-break 语义。
	needsAdmission := false
	for i := range all {
		if _, has := st.Slots[all[i].ip]; !has && !released[all[i].ip] {
			needsAdmission = true
			break
		}
	}
	if needsAdmission {
		// 排序元数据只在有新 IP 时临时分配，稳态路径的 want slice 保持紧凑。
		type rankedWant struct {
			want
			priority  uint8
			firstSeen time.Time
		}
		ranked := make([]rankedWant, len(all))
		for i, w := range all {
			entry := rankedWant{want: w}
			slot, has := st.Slots[w.ip]
			switch {
			case has && !slot.Provisional && w.active:
				entry.priority = 0
			case w.active:
				entry.priority = 1
			case has && slot.Provisional:
				entry.priority = 2
			default:
				entry.priority = 3
			}
			if o := st.Observed[w.ip]; o != nil {
				entry.firstSeen = o.FirstSeen
			}
			ranked[i] = entry
		}
		sort.Slice(ranked, func(i, j int) bool {
			a, b := ranked[i], ranked[j]
			if a.priority != b.priority {
				return a.priority < b.priority
			}
			if !a.firstSeen.Equal(b.firstSeen) {
				return a.firstSeen.Before(b.firstSeen)
			}
			return a.ip < b.ip
		})
		for i := range all {
			all[i] = ranked[i].want
		}
	}

	// 5) Admission。
	for _, w := range all {
		if released[w.ip] {
			continue // 本轮刚释放，不 re-grant
		}
		a := active[w.ip]
		if !w.active {
			a = candidates[w.ip]
		}
		if slot, ok := st.Slots[w.ip]; ok {
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
			if w.active {
				// 成功建立 → 不再是 provisional。
				slot.Provisional = false
				slot.CandidateAt = time.Time{}
			}
			continue
		}
		hasRoom := maxIPs <= 0 || len(st.Slots) < maxIPs
		// 名额已满但本 IP 已完成握手 → 抢占一个 provisional slot。
		// provisional 只是「让 SYN 能过 nft」的临时预留，不代表在用连接；
		// 真实客户端的优先级必须高于它，否则只发 SYN 的陌生 IP 能占死名额。
		// 被抢占者进 Rejected（其 SYN 本轮起被拦，下轮可重新竞争）。
		if !hasRoom && w.active {
			if victim := oldestProvisional(st); victim != "" {
				delete(st.Slots, victim)
				st.Rejected[victim] = now
				hasRejected = true
				hasRoom = true
			}
		}
		if hasRoom {
			slot := &IPSlot{
				IP:        w.ip,
				GrantedAt: now,
				LastSeen:  now,
				TCP:       a.TCPSessions > 0,
				UDP:       a.UDPSessions > 0,
			}
			if w.active {
				if a.Traffic {
					slot.LastTraffic = now
				}
			} else {
				slot.Provisional = true
				slot.CandidateAt = now
			}
			st.Slots[w.ip] = slot
		} else {
			st.Rejected[w.ip] = now
			hasRejected = true
		}
	}

	// 6) allow set = 所有 slot（含 provisional，保证候选手握能通过 nft）。
	allowSet = make(map[string]bool, len(st.Slots))
	for ip := range st.Slots {
		allowSet[ip] = true
	}

	// 7) Observed GC：无 slot、无拒绝、久未活跃的观察项清理。
	for ip, o := range st.Observed {
		if _, ok := st.Slots[ip]; ok {
			continue
		}
		if _, ok := st.Rejected[ip]; ok {
			continue
		}
		if now.Sub(lastActive(o.LastSeen, o.LastTraffic)) > idle {
			delete(st.Observed, ip)
		}
	}

	return allowSet, hasRejected
}

// oldestProvisional 返回最早授予的 provisional slot 的 IP（无则空串）。
// 用于「真实客户端抢占仅 SYN 候选占用的名额」，按 CandidateAt 最早、
// 同刻按 IP 升序，保证确定性。
func oldestProvisional(st *NodeIPState) string {
	best := ""
	var bestAt time.Time
	for ip, slot := range st.Slots {
		if !slot.Provisional {
			continue
		}
		if best == "" || slot.CandidateAt.Before(bestAt) ||
			(slot.CandidateAt.Equal(bestAt) && ip < best) {
			best, bestAt = ip, slot.CandidateAt
		}
	}
	return best
}

func (st *NodeIPState) touchObserved(ip string, a IPActivity, now time.Time) string {
	o, ok := st.Observed[ip]
	if !ok {
		// IP 字符串常来自 conntrack 的 unsafe.String 子串；持久 map key 若直接
		// 保存它，会把整张 conntrack 文件缓冲区保活到 IP idle GC。只在新 IP
		// 首次落入长期状态时克隆这几字节，避免每 tick 保留整份大文件。
		ip = strings.Clone(ip)
		o = &ObservedIP{IP: ip, FirstSeen: now}
		st.Observed[ip] = o
	}
	o.LastSeen = now
	o.TCPSessions = a.TCPSessions
	o.UDPSessions = a.UDPSessions
	if a.Traffic {
		o.LastTraffic = now
	}
	return o.IP
}

// grantedCount 返回持有 slot 的 IP 数（含 provisional）。
func (st *NodeIPState) grantedCount() int { return len(st.Slots) }

// activeGrantedCount 返回已建立（非 provisional）的 granted IP 数，即「在线 IP」。
func (st *NodeIPState) activeGrantedCount() int {
	n := 0
	for _, s := range st.Slots {
		if !s.Provisional {
			n++
		}
	}
	return n
}
