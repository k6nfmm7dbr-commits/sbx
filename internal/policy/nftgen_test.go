package policy

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// genPolicyNFT 曾遍历 map 生成规则行，相同输入产出不同脚本文本
// （规则不可 diff、无法 golden 测试、线上排查时顺序乱跳）。
func TestGenPolicyNFTIsDeterministic(t *testing.T) {
	mk := func(id, port int64) nodes.Node {
		return nodes.Node{
			"id":   json.Number(strconv.FormatInt(id, 10)),
			"type": "vless",
			"port": json.Number(strconv.FormatInt(port, 10)),
		}
	}
	list := []nodes.Node{mk(1, 443), mk(2, 8443), mk(3, 9443), mk(4, 10443), mk(5, 11443)}
	ipLimits := map[string]map[string]bool{
		"1": {"1.1.1.1": true, "2606:4700::1111": true},
		"2": {"2.2.2.2": true},
		"3": {"3.3.3.3": true},
		"4": {"4.4.4.4": true},
		"5": {"5.5.5.5": true},
	}
	quota := map[int64]bool{8443: true, 443: true}

	first := genPolicyNFT(quota, ipLimits, list)
	for i := 0; i < 200; i++ {
		if got := genPolicyNFT(quota, ipLimits, list); got != first {
			t.Fatalf("第 %d 次生成与首次不一致(map 迭代顺序泄漏到输出)\n--- want ---\n%s\n--- got ---\n%s",
				i, first, got)
		}
	}

	// 规则行必须按端口升序，便于人工核对
	var ports []int
	for _, line := range strings.Split(first, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "tcp dport ") || !strings.Contains(line, "ip saddr") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		p, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		ports = append(ports, p)
	}
	for i := 1; i < len(ports); i++ {
		if ports[i] < ports[i-1] {
			t.Errorf("IP limit 规则未按端口升序: %v", ports)
			break
		}
	}
}

// 策略脚本路径绝不能是计数规则文件：否则覆盖 sbx_traffic 计数表定义，
// 且 firewall.Nft.Repair 自愈时重放策略脚本，计数器永远建不回来。
func TestPolicyConfPathIsSeparateFromCounterRules(t *testing.T) {
	if got := DefaultPolicyConf("/etc/sbx"); got != "/etc/sbx/policy.nft" {
		t.Fatalf("默认策略脚本路径异常: %s", got)
	}
	if strings.HasSuffix(DefaultPolicyConf("/etc/sbx"), "/nft.conf") {
		t.Fatal("策略脚本路径不得与计数规则 nft.conf 相同")
	}
	// 空 policyConf 时也必须落到 policy.nft，不能回退成 nft.conf
	s := New(nil, "/tmp/sbxtest", "")
	if s.PolicyConfPath() != "/tmp/sbxtest/policy.nft" {
		t.Fatalf("空 policyConf 未回退到 policy.nft: %s", s.PolicyConfPath())
	}
}
