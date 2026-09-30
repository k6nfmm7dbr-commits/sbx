package policy

import (
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

func TestAcctProbeSharesFlowScanAndPreservesFilterSemantics(t *testing.T) {
	s := newTestService(t)
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{"10.0.0.1": true}, nil })
	s.refreshSelfIPs(time.Now())
	list := []nodes.Node{{"id": 1, "type": "vless", "port": 443}}

	// node 端口上无 bytes 的客户端流 → 检测 acct=0；非节点端口和本机出站
	// bytes=0 流都不计入 global probe（与旧的独立扫描口径一致）。
	cr := connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
		{Proto: "tcp", State: "ESTABLISHED", DstPort: 9999, SrcIP: "8.8.8.8", SrcPort: 1, Bytes: 0},
		{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: "10.0.0.1", SrcPort: 2, Bytes: 0},
		{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: "9.9.9.9", SrcPort: 3, Bytes: 0},
	}}
	s.buildActivity(list, cr, nil, time.Now())
	if !s.acctDisabled {
		t.Fatal("仅相关节点客户端 flow 的 bytes 全零，应判定 acct disabled")
	}

	// 有一个相关客户端 flow bytes 非零 → acct 恢复。
	cr.Flows[2].Bytes = 128
	s.buildActivity(list, cr, nil, time.Now())
	if s.acctDisabled {
		t.Fatal("相关 flow 出现 bytes，应判定 acct enabled")
	}

	// 无相关节点客户端流时保留上次探测状态（与原逻辑一致）。
	cr.Flows = cr.Flows[:2]
	s.buildActivity(list, cr, nil, time.Now())
	if s.acctDisabled {
		t.Fatal("无相关 flow 时不得重置历史 acct 状态")
	}
}
