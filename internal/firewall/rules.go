package firewall

import (
	"fmt"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// GenNFT 生成 nftables 计数规则脚本：幂等建表（先 delete 再建）+ epoch/system/节点
// 计数器 + sbx_in/sbx_out 两条 hook priority 300 的计数链（lo 直接 RETURN）
// + sbx_ct conntrack 激活链。
//
// 这是 SBX 唯一的规则生成器（nftables-only 架构）。
func GenNFT(list []nodes.Node, epoch uint64) string {
	var b strings.Builder
	w := b.WriteString
	w("#!/usr/sbin/nft -f\n")
	w("# 由 sbx 自动生成，请勿手工编辑\n")
	fmt.Fprintf(&b, "table inet %s\n", NFTTable)
	fmt.Fprintf(&b, "delete table inet %s\n", NFTTable)
	fmt.Fprintf(&b, "table inet %s {\n", NFTTable)

	counters := []string{fmt.Sprintf("sbx_epoch_%d", epoch), "sbx_sys_i", "sbx_sys_o"}
	for _, n := range list {
		id := nodes.IDString(n)
		counters = append(counters, "sbx_n"+id+"_i", "sbx_n"+id+"_o")
	}
	for _, c := range counters {
		fmt.Fprintf(&b, "    counter %s { }\n", c)
	}

	portSet := func(ranges [][2]int64) string {
		parts := make([]string, 0, len(ranges))
		for _, r := range ranges {
			if r[0] == r[1] {
				parts = append(parts, fmt.Sprintf("%d", r[0]))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
			}
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}

	type chainDef struct {
		chain   string
		hook    string
		prio    int
		ifaceKw string
		dirKw   string
		suffix  string
	}
	for _, cd := range []chainDef{
		{"sbx_in", "input", 300, "iifname", "dport", "i"},
		{"sbx_out", "output", 300, "oifname", "sport", "o"},
	} {
		fmt.Fprintf(&b, "    chain %s {\n", cd.chain)
		fmt.Fprintf(&b, "        type filter hook %s priority %d; policy accept;\n", cd.hook, cd.prio)
		fmt.Fprintf(&b, "        %s \"lo\" return\n", cd.ifaceKw)
		fmt.Fprintf(&b, "        counter name sbx_sys_%s\n", cd.suffix)
		for _, n := range list {
			ranges := nodes.ParsePorts(n)
			if len(ranges) == 0 {
				continue
			}
			for _, proto := range nodes.Protocols(n) {
				fmt.Fprintf(&b, "        %s %s %s counter name sbx_n%s_%s\n",
					proto, cd.dirKw, portSet(ranges), nodes.IDString(n), cd.suffix)
			}
		}
		b.WriteString("    }\n")
	}

	// conntrack 激活链（v3.0.8）：内核只在**存在引用 conntrack 的 netfilter 规则**时
	// 才真正为连接建立 conntrack 条目。全新的 Debian/Ubuntu 机器若没有任何防火墙规则，
	// nf_conntrack 模块虽已加载、/proc/net/nf_conntrack 也存在，但内容恒为空。
	//
	// 后果（真机实测）：面板的「在线 IP」判活以 conntrack 为主数据源，且刻意不在
	// conntrack「可用」时回退 /proc（避免已断开的 socket 残留复活成在线 IP）——
	// 于是 conntrack 恒空 ⇒ 在线 IP 恒为 0，而连接数（直接读 /proc/net/tcp）却正常，
	// 表现为「有连接数但在线 IP 显示 0」。
	//
	// 这里挂一条只做 `ct state new` 计数的链：它不 drop、不 accept 决策
	// （policy accept，counter 是终结动作之外的观察动作），因此对放行行为零影响，
	// 唯一作用是让内核为流量建立 conntrack 条目。优先级 -150（早于 filter，
	// 与 conntrack 的 defrag/prerouting 语义无冲突），仅 input 方向即可激活跟踪。
	fmt.Fprintf(&b, "    counter %s { }\n", CTActivateCounter)
	b.WriteString("    chain sbx_ct {\n")
	b.WriteString("        type filter hook input priority -150; policy accept;\n")
	fmt.Fprintf(&b, "        ct state new counter name %s\n", CTActivateCounter)
	b.WriteString("    }\n")

	b.WriteString("}\n")
	return b.String()
}
