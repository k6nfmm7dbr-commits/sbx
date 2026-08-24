package firewall

import (
	"fmt"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// GenNFT 逐字节复刻旧 gen_nft：幂等建表（先 delete 再建）+ epoch/system/节点
// 计数器 + sbx_in/sbx_out 两条 hook priority 300 的计数链（lo 直接 RETURN）。
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
	b.WriteString("}\n")
	return b.String()
}

// GenIPTables 逐字节复刻旧 gen_iptables：自包含脚本（apply|clear），
// 计数链插在 INPUT/OUTPUT 首位（与旧行为一致），epoch 以注释标记。
func GenIPTables(list []nodes.Node, epoch uint64) string {
	sh := []string{
		"#!/bin/sh",
		"# 由 sbx 自动生成，请勿手工编辑",
		"# 用法: sh iptables.sh apply|clear",
		"IN=" + IptChainIn,
		"OUT=" + IptChainOut,
		"",
		"clear_one() {",
		"  B=\"$1\"",
		"  \"$B\" -w -D INPUT -j \"$IN\" 2>/dev/null",
		"  \"$B\" -w -D OUTPUT -j \"$OUT\" 2>/dev/null",
		"  \"$B\" -w -F \"$IN\" 2>/dev/null; \"$B\" -w -X \"$IN\" 2>/dev/null",
		"  \"$B\" -w -F \"$OUT\" 2>/dev/null; \"$B\" -w -X \"$OUT\" 2>/dev/null",
		"}",
		"",
		"apply_one() {",
		"  B=\"$1\"",
		"  command -v \"$B\" >/dev/null 2>&1 || return 0",
		"  clear_one \"$B\"",
		"  \"$B\" -w -N \"$IN\"  2>/dev/null",
		"  \"$B\" -w -N \"$OUT\" 2>/dev/null",
		"  \"$B\" -w -I INPUT 1 -j \"$IN\"",
		"  \"$B\" -w -I OUTPUT 1 -j \"$OUT\"",
		fmt.Sprintf("  \"$B\" -w -A \"$IN\"  -m comment --comment \"sbx:epoch:%d\"", epoch),
		"  \"$B\" -w -A \"$IN\"  -i lo -j RETURN",
		"  \"$B\" -w -A \"$OUT\" -o lo -j RETURN",
		"  \"$B\" -w -A \"$IN\"  -m comment --comment \"sbx:sys:i\"",
		"  \"$B\" -w -A \"$OUT\" -m comment --comment \"sbx:sys:o\"",
	}
	for _, n := range list {
		for _, r := range nodes.ParsePorts(n) {
			pspec := fmt.Sprintf("%d", r[0])
			if r[0] != r[1] {
				pspec = fmt.Sprintf("%d:%d", r[0], r[1])
			}
			for _, proto := range nodes.Protocols(n) {
				id := nodes.IDString(n)
				sh = append(sh,
					fmt.Sprintf("  \"$B\" -w -A \"$IN\"  -p %s --dport %s -m comment --comment \"sbx:n%s:i\"", proto, pspec, id),
					fmt.Sprintf("  \"$B\" -w -A \"$OUT\" -p %s --sport %s -m comment --comment \"sbx:n%s:o\"", proto, pspec, id))
			}
		}
	}
	sh = append(sh,
		"}",
		"",
		"case \"${1:-apply}\" in",
		"  apply) apply_one iptables; apply_one ip6tables ;;",
		"  clear) clear_one iptables; clear_one ip6tables ;;",
		"  *) echo \"用法: $0 apply|clear\" >&2; exit 2 ;;",
		"esac")
	return strings.Join(sh, "\n") + "\n"
}
