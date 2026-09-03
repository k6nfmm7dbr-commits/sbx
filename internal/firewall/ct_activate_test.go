package firewall

import "testing"

// sbx_ct_activate 是 conntrack 激活链的计数器，它必须**不**被识别为
// 流量计数器或 epoch 标记——否则采集器会把它当成一个 scope 入账，
// 污染统计（它统计的是「新连接数」，与字节流量无关）。
func TestCTActivateCounterNotParsedAsTraffic(t *testing.T) {
	if scope, dir, ok := ParseCounterName(CTActivateCounter); ok {
		t.Errorf("%s 不应被识别为流量计数器, got scope=%q dir=%q", CTActivateCounter, scope, dir)
	}
	if _, ok := ParseEpochName(CTActivateCounter); ok {
		t.Errorf("%s 不应被识别为 epoch 标记", CTActivateCounter)
	}
	// 带 @family 后缀（iptables 形态）同样不得命中
	if _, _, ok := ParseCounterName(CTActivateCounter + "@v4"); ok {
		t.Errorf("%s@v4 不应被识别为流量计数器", CTActivateCounter)
	}
}

// 生成的 nft 脚本必须包含 conntrack 激活链，且该链不改变放行决策
// （policy accept、只有 counter 动作，绝不出现 drop/reject）。
func TestGenNFTIncludesConntrackActivation(t *testing.T) {
	script := GenNFT(loadRuleNodes(), 42)

	for _, want := range []string{
		"chain sbx_ct {",
		"type filter hook input priority -150; policy accept;",
		"ct state new counter name " + CTActivateCounter,
		"counter " + CTActivateCounter + " { }",
	} {
		if !contains(script, want) {
			t.Errorf("nft 脚本缺少 conntrack 激活链片段: %q\n---\n%s", want, script)
		}
	}
	// 激活链绝不能引入任何拦截动作
	for _, bad := range []string{"drop", "reject"} {
		if contains(script, bad) {
			t.Errorf("计数规则不得包含 %q（会改变放行行为）:\n%s", bad, script)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
