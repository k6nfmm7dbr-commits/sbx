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
	portRanges := map[int64][][2]int64{}
	for _, n := range list {
		id := nodes.IDString(n)
		for _, r := range nodes.ParsePorts(n) {
			for p := r[0]; p <= r[1]; p++ {
				portToID[p] = id
				portRanges[p] = append(portRanges[p], [2]int64{p, p})
			}
		}
	}

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
		for id, ips := range ipLimits {
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
	for p, id := range portToID {
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
// 失败返回错误，绝不静默吞掉（调用方据此让 API 保存失败）。
func (s *Service) applyEnforcement(ctx context.Context, quotaBlocked map[string]bool,
	ipBlocked map[string]map[string]bool, list []nodes.Node) error {

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
	if sameQuota(s.appliedQuota, quotaBlocked) && sameIPLimits(s.appliedIPLimit, ipBlocked) {
		return nil
	}

	script := genPolicyNFT(quotaPorts, ipBlocked, list)
	if err := os.MkdirAll(s.appDir, 0o755); err != nil {
		return err
	}
	path := s.nftConf
	if path == "" {
		path = s.appDir + "/policy.nft"
	}
	if err := fsx.WriteFileAtomic(path, []byte(script), 0o644); err != nil {
		return err
	}
	if s.nftApply != nil {
		return s.nftApply(ctx, path)
	}
	rc, _, errMsg := firewall.RunCmd(ctx, "nft", "-f", path)
	if rc != 0 {
		return fmt.Errorf("nft 策略规则应用失败: %s", strings.TrimSpace(errMsg))
	}
	return nil
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
