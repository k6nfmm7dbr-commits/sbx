package policy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// PolicyTable 是策略 enforcement 的独立 nft 表名，与计数表 sbx_traffic 分离，
// 绝不 flush 用户自己的 nftables ruleset。
const PolicyTable = "sbx_policy"

// policyPriority 是策略 drop 链的优先级：必须早于计数链（300），
// 这样被 pause/IP limit 拒绝的包不会进入计数。
const policyPriority = 200

// genPolicyNFT 生成策略 enforcement 的 nft 脚本。
// pausedPorts: 已暂停节点端口；ipLimits: nodeID -> 允许的 IP 集合；
// rateLimitPorts: 端口 -> 限速值（Mbps，每方向独立），仅含 mbps>0 的端口。
// 暂停对 input dport 和 output sport 都 drop，优先级早于流量计数链，覆盖已建立连接。
// IP limit 用 allow set：达限后只放行已获 slot 的 IP，其余 drop（不随机踢旧 IP）。
//
// 限速用 nftables `limit rate over ... drop`（policer，丢弃超额包）实现，不引入
// tc/qdisc 等额外子系统——与「nftables-only、单二进制、绝不 flush 用户规则」的
// 架构一致。据实说明其性质：这是**限速器（policing，丢超额包）而非整形器
// （shaping，排队延迟）**；对 TCP 仍能有效限流（丢包触发拥塞控制回退），
// 但不如 tc HTB 平滑、会有少量重传开销。Mbps 按 10^6 bit/s 计：
// 每秒字节数 = mbps × 125000。
func genPolicyNFT(pausedPorts map[int64]bool, ipLimits map[string]map[string]bool, rateLimitPorts map[int64]int, list []nodes.Node) string {
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("# 由 sbx 策略层自动生成，请勿手工编辑\n")
	fmt.Fprintf(&b, "table inet %s\n", PolicyTable)
	fmt.Fprintf(&b, "delete table inet %s\n", PolicyTable)
	fmt.Fprintf(&b, "table inet %s {\n", PolicyTable)

	// 端口 -> 节点 id 映射（IP limit 规则需要按端口写）
	portToID := map[int64]string{}
	for _, n := range list {
		id := nodes.IDString(n)
		for _, r := range nodes.ParsePorts(n) {
			for p := r[0]; p <= r[1]; p++ {
				portToID[p] = id
			}
		}
	}
	// 输出必须确定性：map 迭代顺序随机会让相同输入产出不同脚本文本，
	// 导致规则不可 diff、无法 golden 测试、线上排查时规则顺序乱跳。
	orderedPorts := make([]int64, 0, len(portToID))
	for p := range portToID {
		orderedPorts = append(orderedPorts, p)
	}
	sortInt64(orderedPorts)

	orderedIPLimitIDs := make([]string, 0, len(ipLimits))
	for id := range ipLimits {
		orderedIPLimitIDs = append(orderedIPLimitIDs, id)
	}
	sort.Strings(orderedIPLimitIDs)

	// 限速端口按升序输出（确定性）。
	orderedRatePorts := make([]int64, 0, len(rateLimitPorts))
	for p := range rateLimitPorts {
		if rateLimitPorts[p] > 0 {
			orderedRatePorts = append(orderedRatePorts, p)
		}
	}
	sortInt64(orderedRatePorts)

	hasAny := len(pausedPorts) > 0 || len(ipLimits) > 0 || len(orderedRatePorts) > 0
	if hasAny {
		if len(pausedPorts) > 0 {
			ports := make([]int64, 0, len(pausedPorts))
			for p := range pausedPorts {
				ports = append(ports, p)
			}
			sortInt64(ports)
			fmt.Fprintf(&b, "    set paused_ports {\n        type inet_service\n        flags interval\n        elements = { %s }\n    }\n",
				joinPorts(ports))
		}
		// IP limit 节点固定创建 v4/v6 两个 allow set（空集也合法），保证规则引用始终有效。
		for _, id := range orderedIPLimitIDs {
			ips := ipLimits[id]
			v4 := []string{}
			v6 := []string{}
			for ip := range ips {
				if strings.Contains(ip, ":") {
					v6 = append(v6, ip)
				} else {
					v4 = append(v4, ip)
				}
			}
			writeIPSet(&b, "ip_allow_"+id+"_v4", "ipv4_addr", v4)
			writeIPSet(&b, "ip_allow_"+id+"_v6", "ipv6_addr", v6)
		}
	}

	// input 链：暂停规则先于 IP allow 和 rate policing，并早于计数链，
	// 因此暂停节点的 TCP/UDP 包（含既有连接）全被丢弃且不计入其流量。
	fmt.Fprintf(&b, "    chain policy_in {\n        type filter hook input priority %d; policy accept;\n", policyPriority)
	if len(pausedPorts) > 0 {
		b.WriteString("        tcp dport @paused_ports drop\n")
		b.WriteString("        udp dport @paused_ports drop\n")
	}
	for _, p := range orderedPorts {
		id := portToID[p]
		if _, ok := ipLimits[id]; !ok {
			continue
		}
		// 达 IP 限的节点：只放行 allow set 内的源 IP。
		// 关键：只 drop「已建立（ct state established）」的连接，放行 SYN（new）——
		// 否则第二个 IP 的 SYN 会被直接丢弃，连握手都起不来，conntrack 也看不到候选，
		// 导致「严格 allow set」出现鸡生蛋死锁（第二个 IP 永远拿不到 slot）。
		// 放行 SYN 后，握手可完成、conntrack 能看到 SYN_RECV 候选，Slot Manager
		// 才会临时授予并把它加进 allow set，随后其数据包放行。
		fmt.Fprintf(&b, "        tcp dport %d ct state established ip saddr != @ip_allow_%s_v4 drop\n", p, id)
		fmt.Fprintf(&b, "        udp dport %d ct state established ip saddr != @ip_allow_%s_v4 drop\n", p, id)
		fmt.Fprintf(&b, "        tcp dport %d ct state established ip6 saddr != @ip_allow_%s_v6 drop\n", p, id)
		fmt.Fprintf(&b, "        udp dport %d ct state established ip6 saddr != @ip_allow_%s_v6 drop\n", p, id)
	}
	// 限速（入站方向 = 客户端上传）：丢弃超过 mbps 的包。
	for _, p := range orderedRatePorts {
		bps := rateBytesPerSec(rateLimitPorts[p])
		fmt.Fprintf(&b, "        tcp dport %d limit rate over %d bytes/second burst %d bytes drop\n", p, bps, bps)
		fmt.Fprintf(&b, "        udp dport %d limit rate over %d bytes/second burst %d bytes drop\n", p, bps, bps)
	}
	b.WriteString("    }\n")

	// output 链：暂停节点的回包也必须阻断；限速同样包含下载方向。
	fmt.Fprintf(&b, "    chain policy_out {\n        type filter hook output priority %d; policy accept;\n", policyPriority)
	if len(pausedPorts) > 0 {
		b.WriteString("        tcp sport @paused_ports drop\n")
		b.WriteString("        udp sport @paused_ports drop\n")
	}
	// 限速（出站方向 = 客户端下载）：与入站独立各自限到 mbps。
	for _, p := range orderedRatePorts {
		bps := rateBytesPerSec(rateLimitPorts[p])
		fmt.Fprintf(&b, "        tcp sport %d limit rate over %d bytes/second burst %d bytes drop\n", p, bps, bps)
		fmt.Fprintf(&b, "        udp sport %d limit rate over %d bytes/second burst %d bytes drop\n", p, bps, bps)
	}
	b.WriteString("    }\n")

	b.WriteString("}\n")
	return b.String()
}

// rateBytesPerSec 把 Mbps（10^6 bit/s）换算为每秒字节数，供 nft limit rate 使用。
func rateBytesPerSec(mbps int) int64 { return int64(mbps) * 125000 }

func sortInt64(a []int64) { sort.Slice(a, func(i, j int) bool { return a[i] < a[j] }) }

func joinPorts(ports []int64) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d", p))
	}
	return strings.Join(parts, ", ")
}

// writeIPSet 写一个 nft set；空列表时省略 elements（空 set 合法且可被规则引用）。
func writeIPSet(b *strings.Builder, name, typ string, elements []string) {
	sort.Strings(elements)
	fmt.Fprintf(b, "    set %s {\n        type %s\n", name, typ)
	if len(elements) > 0 {
		fmt.Fprintf(b, "        elements = { %s }\n", strings.Join(elements, ", "))
	}
	b.WriteString("    }\n")
}

// applyEnforcement 生成策略 nft 脚本并应用。暂停节点、IP limit 和 rate limit 共用独立策略表。
//
// 返回错误不阻断 reconcile 的状态发布（见 reconcile 注释）：调用方把它记进
// lastErr 并继续发布状态，这样 nft 应用暂时失败时面板仍显示真实用量，
// 且用户能从 policy_error 看到「阻断未生效」。
//
// 后端：nftables-only。策略 enforcement 用 allow set + ct state 规则实现，
// 与流量统计共用同一套 nftables 基础设施（表分离：sbx_policy / sbx_traffic）。
//
// 应用节流：
//   - 立即应用：节点暂停翻转 / IP 受限节点集合变化 / 节点端口形态变化；
//   - 节流合并：仅 allow set 的 IP 内容变化（slot 授予/释放/provisional 超时）。
//     此类变化在名额未满的受限节点上可被扫描者用 SYN churn 诱发成
//     「每秒一次整表 nft -f 重写」；合并到 enforceMinInterval 窗口后，
//     最坏情况是已被拒的 IP 多等一个窗口才重试、新 grant 的 IP 多等一个窗口
//     才完全生效（其 SYN 本就靠 provisional 放行），语义可接受。
//     合并不是丢弃：applied 与目标持续不一致，间隔一到下一轮立即应用，收敛。
func (s *Service) applyEnforcement(ctx context.Context, pausedNodes map[string]bool,
	ipBlocked map[string]map[string]bool, rateLimited map[string]int, list []nodes.Node) error {

	needEnforce := len(pausedNodes) > 0 || len(ipBlocked) > 0 || len(rateLimited) > 0

	// 与上次应用状态比较，无变化则跳过（幂等，避免每次 reconcile 重写 nft）。
	pausedChanged := !sameNodeSet(s.appliedPaused, pausedNodes)
	ipKeysChanged := !sameIPLimitKeys(s.appliedIPLimit, ipBlocked)
	ipContentChanged := !sameIPLimits(s.appliedIPLimit, ipBlocked)
	rateChanged := !sameRate(s.appliedRate, rateLimited)
	// 节点端口变化即使 applied nodeID 集合不变也必须重写 nft，否则旧端口仍有死规则。
	// nodesShape 只在节点文件 slice 身份变化时重新计算，避免每轮重复解析端口/拼接。
	shape := s.cachedNodesShape(list)
	shapeChanged := shape != s.appliedShape &&
		(needEnforce || len(s.appliedPaused) > 0 || len(s.appliedIPLimit) > 0 || len(s.appliedRate) > 0)
	if !pausedChanged && !ipContentChanged && !rateChanged && !shapeChanged {
		if !needEnforce {
			// 新进程首次 reconcile 时检查残留策略表：此前暂停后进程停止、节点在
			// 停止期间被删除，SQLite 已无 paused 行但内核表仍可能保留旧 dport drop。
			// 若表存在，继续生成空策略表并应用；若不存在则无需创建。
			if s.enforcementInitialized || !s.policyTablePresent() {
				s.enforcementInitialized = true
				return nil
			}
			slog.Warn("启动时发现残留策略表, 正在清除已无对应配置的规则")
		} else if s.policyTablePresent() {
			return nil
		} else {
			slog.Warn("检测到策略表被外部移除, 正在重建 enforcement")
		}
	}

	// 仅 IP allow-set 内容变化可节流。暂停/恢复必须立即生效。
	now := s.now()
	if ipContentChanged && !pausedChanged && !ipKeysChanged && !rateChanged && !shapeChanged &&
		s.enforceMinInterval > 0 && !s.lastEnforceAt.IsZero() &&
		now.Sub(s.lastEnforceAt) < s.enforceMinInterval {
		return nil
	}

	// 端口映射只在确定本轮确实要应用时才展开，并合并为一次节点列表遍历。
	pausedPorts := make(map[int64]bool, len(pausedNodes))
	ratePorts := make(map[int64]int, len(rateLimited))
	for _, n := range list {
		id := nodes.IDString(n)
		_, paused := pausedNodes[id]
		mbps, rate := rateLimited[id]
		if !paused && (!rate || mbps <= 0) {
			continue
		}
		for _, r := range nodes.ParsePorts(n) {
			for p := r[0]; p <= r[1]; p++ {
				if paused {
					pausedPorts[p] = true
				}
				if rate && mbps > 0 {
					ratePorts[p] = mbps
				}
			}
		}
	}
	script := genPolicyNFT(pausedPorts, ipBlocked, ratePorts, list)
	if err := os.MkdirAll(s.appDir, 0o755); err != nil {
		return err
	}
	// 必须是策略专属路径：复用计数规则文件（nft.conf）会覆盖 sbx_traffic
	// 计数表定义，且 firewall.Nft.Repair 自愈时重放策略脚本，计数器再也建不回来。
	path := s.policyConf
	if path == "" {
		path = DefaultPolicyConf(s.appDir)
	}
	if err := fsx.WriteFileAtomic(path, []byte(script), 0o644); err != nil {
		return err
	}
	apply := s.nftApply
	if apply == nil {
		apply = func(ctx context.Context, p string) error {
			rc, _, errMsg := firewall.RunCmd(ctx, "nft", "-f", p)
			if rc != 0 {
				return fmt.Errorf("nft 策略规则应用失败: %s", strings.TrimSpace(errMsg))
			}
			return nil
		}
	}
	if err := apply(ctx, path); err != nil {
		return err
	}
	// 只有真正应用成功才记账，否则下一轮会因「无变化」而跳过重试。
	s.appliedPaused = pausedNodes
	s.appliedIPLimit = ipBlocked
	s.appliedRate = rateLimited
	s.appliedShape = shape
	s.lastEnforceAt = now
	s.lastProbeOK = true // 刚应用成功，表必然存在
	s.enforcementInitialized = true
	return nil
}

// nodesShape 生成「节点 id→端口集合」的规范化摘要（排序、确定输出），
// 用于感知节点端口变化——规则文本按端口生成，但 applied 比较的键是节点 id，
// 不看端口就会在节点改端口后跳过应用，留下指向旧端口的死规则。
//
// cachedNodesShape 是它的 memo 化包装：节点文件没变（strict loader 返回同一
// 个不可变 slice）时直接复用上次结果，避免每轮 reconcile 白付一次字符串拼接
// 与多次中间分配。
//
// 注意：身份标记 shapeNodes 与 activityPortIndex 的 activityNodes 相互独立——
// 两个缓存各自记账，先算谁都不会把另一个的缓存错标成「仍有效」。
func (s *Service) cachedNodesShape(list []nodes.Node) string {
	if len(list) == len(s.shapeNodes) &&
		(len(list) == 0 || &list[0] == &s.shapeNodes[0]) && s.shapeCache != "" {
		return s.shapeCache
	}
	s.shapeNodes = list
	s.shapeCache = nodesShape(list)
	return s.shapeCache
}

func nodesShape(list []nodes.Node) string {
	parts := make([]string, 0, len(list))
	for _, n := range list {
		ports := make([]string, 0, 2)
		for _, r := range nodes.ParsePorts(n) {
			ports = append(ports, fmt.Sprintf("%d-%d", r[0], r[1]))
		}
		sort.Strings(ports)
		parts = append(parts, nodes.IDString(n)+"="+strings.Join(ports, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func sameIPLimitKeys(a, b map[string]map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// probeInterval 是策略表存在性探测的间隔（每次探测是一次 nft exec，
// 不能每秒做；被外部删除后最坏一个间隔内自愈，方向仍是 fail-closed）。
const probeInterval = 10 * time.Second

// policyTablePresent 探测内核里策略表是否真实存在（有节流与结果缓存）。
// 默认实现用 `nft list table`；测试可经 SetTableProbe 注入。
// 探测本身失败（无 nft 命令 / 无权限）按「存在」处理，不据此重写规则。
func (s *Service) policyTablePresent() bool {
	now := s.now()
	if now.Sub(s.lastProbeAt) < probeInterval {
		return s.lastProbeOK
	}
	s.lastProbeAt = now
	probe := s.tableProbe
	if probe == nil {
		probe = func() bool {
			rc, _, _ := firewall.RunCmd(context.Background(), "nft", "list", "table", "inet", PolicyTable)
			return rc == 0
		}
	}
	ok := probe()
	if ok {
		s.lastProbeOK = true
	} else {
		// 缺失结论不节流：配合上层「缺失即重建」，下一轮立即重探，
		// 重建成功后回到节流探测节奏。
		s.lastProbeAt = time.Time{}
		s.lastProbeOK = false
	}
	return ok
}

// SetTableProbe 注入策略表存在性探测函数（测试用）。
func (s *Service) SetTableProbe(fn func() bool) { s.tableProbe = fn }

func sameNodeSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// sameRate 比较两个「nodeID -> mbps」限速映射是否完全一致。
func sameRate(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for id, v := range a {
		if bv, ok := b[id]; !ok || bv != v {
			return false
		}
	}
	return true
}

func sameIPLimits(a map[string]map[string]bool, b map[string]map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id, ips := range a {
		other, ok := b[id]
		if !ok || len(ips) != len(other) {
			return false
		}
		for ip := range ips {
			if !other[ip] {
				return false
			}
		}
	}
	return true
}
