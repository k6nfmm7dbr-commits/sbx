package policy

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// PolicyTable 是策略 enforcement 的独立 nft 表名，与计数表 sbx_traffic 分离，
// 绝不 flush 用户自己的 nftables ruleset。
const PolicyTable = "sbx_policy"

// policyPriority 是策略 drop 链的优先级：必须早于计数链（300），
// 这样被 quota/ip-limit 拒绝的包不会进入计数（不制造「有效代理流量」额度）。
const policyPriority = 200

// genPolicyNFT 生成策略 enforcement 的 nft 脚本。
// quotaPorts: 达 quota 限节点的端口集合；ipLimits: nodeID -> 允许的 IP 集合（allow set）。
// IP limit 用 allow set：达限后只放行已获 slot 的 IP，其余 drop（不随机踢旧 IP）。
func genPolicyNFT(quotaPorts map[int64]bool, ipLimits map[string]map[string]bool, list []nodes.Node) string {
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

	hasAny := false
	if len(quotaPorts) > 0 || len(ipLimits) > 0 {
		hasAny = true
	}
	if hasAny {
		// quota 达限端口集合
		if len(quotaPorts) > 0 {
			ports := make([]int64, 0, len(quotaPorts))
			for p := range quotaPorts {
				ports = append(ports, p)
			}
			sortInt64(ports)
			fmt.Fprintf(&b, "    set quota_ports {\n        type inet_service\n        flags interval\n        elements = { %s }\n    }\n",
				joinPorts(ports))
		}
		// IP limit 达限节点：每个节点固定创建 v4/v6 两个 allow set（允许空集），
		// 规则统一引用它们，避免「set 不存在」导致 nft 脚本失败。
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

	// input 链：早于计数链，只 drop 达限流量
	fmt.Fprintf(&b, "    chain policy_in {\n        type filter hook input priority %d; policy accept;\n", policyPriority)
	if len(quotaPorts) > 0 {
		b.WriteString("        tcp dport @quota_ports drop\n")
		b.WriteString("        udp dport @quota_ports drop\n")
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
	b.WriteString("    }\n")

	// output 链：quota 达限时也需要阻断出站（否则下载方向仍能放行）。
	fmt.Fprintf(&b, "    chain policy_out {\n        type filter hook output priority %d; policy accept;\n", policyPriority)
	if len(quotaPorts) > 0 {
		b.WriteString("        tcp sport @quota_ports drop\n")
		b.WriteString("        udp sport @quota_ports drop\n")
	}
	b.WriteString("    }\n")

	b.WriteString("}\n")
	return b.String()
}

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

// applyEnforcement 生成策略 nft 脚本并应用。无任何达限节点时清空策略表。
//
// 返回错误不再阻断 reconcile 的状态发布（见 reconcile 注释）：调用方把它
// 记进 lastErr 并继续发布状态，这样 iptables-only 主机上面板仍显示真实用量。
//
// 后端限制：策略 enforcement 只实现了 nft（allow set + ct state 规则在
// iptables 上没有等价的低成本实现）。非 nft 后端时直接返回
// ErrEnforceUnsupported，不去执行必然失败的 nft -f，避免每秒刷一条 WARN。
//
// 应用节流（v3.0.7）：
//   - 立即应用：quota 达限集合变化 / IP 受限节点集合变化 / 节点端口形态变化
//     （这些改变「谁被限」，延迟意味着该拦的没拦或误拦）；
//   - 节流合并：仅 allow set 的 IP 内容变化（slot 授予/释放/provisional 超时）。
//     此类变化在名额未满的受限节点上可被扫描者用 SYN churn 诱发成
//     「每秒一次整表 nft -f 重写」；合并到 enforceMinInterval 窗口后，
//     最坏情况是已被拒的 IP 多等一个窗口才重试、新 grant 的 IP 多等一个窗口
//     才完全生效（其 SYN 本就靠 provisional 放行），语义可接受。
//     合并不是丢弃：applied 与目标持续不一致，间隔一到下一轮立即应用，收敛。
func (s *Service) applyEnforcement(ctx context.Context, quotaBlocked map[string]bool,
	ipBlocked map[string]map[string]bool, list []nodes.Node) error {

	// 无需阻断任何东西时，即使后端不支持也不算错误（不打扰用户）。
	needEnforce := len(quotaBlocked) > 0 || len(ipBlocked) > 0
	if s.enforceBackend != nil && s.enforceBackend() != "nft" && needEnforce {
		return ErrEnforceUnsupported
	}

	quotaPorts := map[int64]bool{}
	for id := range quotaBlocked {
		for _, n := range list {
			if nodes.IDString(n) == id {
				for _, r := range nodes.ParsePorts(n) {
					for p := r[0]; p <= r[1]; p++ {
						quotaPorts[p] = true
					}
				}
			}
		}
	}

	// 与上次应用状态比较，无变化则跳过（幂等，避免每次 reconcile 重写 nft）。
	quotaChanged := !sameQuota(s.appliedQuota, quotaBlocked)
	ipKeysChanged := !sameIPLimitKeys(s.appliedIPLimit, ipBlocked)
	ipContentChanged := !sameIPLimits(s.appliedIPLimit, ipBlocked)
	// 端口形态比较：节点改端口但 allow set 不变时，appliedQuota/appliedIPLimit
	// 比较完全看不出来（规则按端口生成，比较键却是节点 id）——必须单独跟踪，
	// 否则 nft 里会留下指向旧端口的死规则、新端口失去 enforcement。
	shape := nodesShape(list)
	// 只有「当前有需要 enforcement 的规则」或「上次应用过非空规则」时，
	// 端口漂移才有意义；两边都为空时端口怎么变都不需要重写。
	shapeChanged := shape != s.appliedShape &&
		(needEnforce || len(s.appliedQuota) > 0 || len(s.appliedIPLimit) > 0)
	if !quotaChanged && !ipContentChanged && !shapeChanged {
		return nil
	}

	// 仅 IP 内容变化时走节流窗口；其余（谁被限变了/端口变了）立即应用。
	now := s.now()
	if ipContentChanged && !quotaChanged && !ipKeysChanged && !shapeChanged &&
		s.enforceMinInterval > 0 && !s.lastEnforceAt.IsZero() &&
		now.Sub(s.lastEnforceAt) < s.enforceMinInterval {
		return nil // 合并到后续轮次：目标态持续不一致，间隔一到下一轮即应用
	}

	script := genPolicyNFT(quotaPorts, ipBlocked, list)
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
	s.appliedQuota = quotaBlocked
	s.appliedIPLimit = ipBlocked
	s.appliedShape = shape
	s.lastEnforceAt = now
	return nil
}

// nodesShape 生成「节点 id→端口集合」的规范化摘要（排序、确定输出），
// 用于感知节点端口变化——规则文本按端口生成，但 applied 比较的键是节点 id，
// 不看端口就会在节点改端口后跳过应用，留下指向旧端口的死规则。
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

func sameQuota(a map[string]bool, b map[string]bool) bool {
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
