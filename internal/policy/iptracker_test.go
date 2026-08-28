package policy

import (
	"context"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

func procResult(tcp, udp map[string]bool, partial bool) func([]nodes.Node) (map[string]connection.RemoteIPSet, bool, error) {
	return func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error) {
		m := map[string]connection.RemoteIPSet{}
		for _, n := range list {
			m[nodes.IDString(n)] = connection.RemoteIPSet{TCP: tcp, UDP: udp}
		}
		return m, partial, nil
	}
}

// conntrack Available=true 且 0 flows：禁止 fallback 到 /proc 残留 ESTABLISHED（不复活已死 IP）。
func TestConntrackAvailableButEmptyDoesNotFallbackToProc(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetConntrack(func(path string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: nil}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"9.9.9.9": true}, nil, false))
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 0 {
		t.Fatalf("conntrack 可用但 0 flow + /proc 残留 ESTABLISHED 不应被判在线, got %d", st["1"].ActiveIPs)
	}
}

// conntrack 不可用：/proc ESTABLISHED 即在线（保守）。
func TestConntrackUnavailableFallback(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetConntrack(func(path string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: false}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"8.8.8.8": true}, nil, false))
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("conntrack 不可用应回退 /proc, got %d", st["1"].ActiveIPs)
	}
}

// partial=true：本轮不完整结果不释放已有 slot（fail-safe，不误踢）。
func TestPartialProcDoesNotReleaseSlot(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetIPIdle(30 * time.Second)

	first := procResult(map[string]bool{"9.9.9.9": true}, nil, false)
	s.SetRemoteIPs(first)
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 1 {
		t.Fatal("第一轮应有 1 在线 IP")
	}

	// 第二轮 partial=true，且只采到空（不完整）—— 不得释放 9.9.9.9。
	s.SetRemoteIPs(procResult(nil, nil, true))
	base := s.now()
	s.SetClock(func() time.Time { return base.Add(1 * time.Second) })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 1 {
		t.Fatalf("partial 结果不应释放已有 slot, got %d", st["1"].ActiveIPs)
	}
}

// 节点删除：slot / observed / rejected / flow tracker 全部清空。
func TestDeleteNodeCleansIPState(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", DstPort: 443, SrcIP: "9.9.9.9", SrcPort: 1111, Bytes: 10},
		}}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"9.9.9.9": true}, nil, false))
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.flows) == 0 {
		t.Fatal("应有 flow 状态")
	}
	// 模拟节点真正被删除：先从 nodes.json 移除，再清理 policy 状态。
	if err := nodes.SaveNodesFile(s.nodesPath(), []nodes.Node{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ipStates["1"]; ok {
		t.Fatal("删除节点后 ipStates 应清空")
	}
	for k := range s.flows {
		if len(k) >= 2 && k[:2] == "1\x00" {
			t.Fatalf("删除节点后 flow tracker 应清空, still %q", k)
		}
	}
}

// Flow GC：大批 flow 出现又消失，超过 idle 后 map 回落，不无限增长。
func TestFlowTrackerGC(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetIPIdle(10 * time.Second)
	base := time.Now()
	s.SetClock(func() time.Time { return base })

	flows := make([]connection.ConntrackFlow, 200)
	for i := 0; i < 200; i++ {
		flows[i] = connection.ConntrackFlow{Proto: "tcp", DstPort: 443, SrcIP: "10.0.0.1", SrcPort: 1000 + i, Bytes: int64(i + 1)}
	}
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: flows}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"10.0.0.1": true}, nil, false))
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.flows) < 200 {
		t.Fatalf("应有 >=200 flow 状态, got %d", len(s.flows))
	}

	// flow 全部消失 + 超过 idle → GC
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: nil}
	})
	base = base.Add(11 * time.Second)
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.flows) != 0 {
		t.Fatalf("flow 全部消失且超 idle 后应被 GC, got %d", len(s.flows))
	}
}

// 服务级严格 admission：max=2 出现 3 个 IP，只授予 2，第 3 个 rejected。
func TestServiceAdmissionRejects(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 2}); err != nil {
		t.Fatal(err)
	}
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(map[string]bool{"A": true, "B": true, "C": true}, nil, false))
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 2 {
		t.Fatalf("max=2 出现 3 IP 应只授予 2, got %d", st["1"].ActiveIPs)
	}
	if st["1"].IPLimitState != "exceeded" {
		t.Fatalf("超限应 exceeded, got %s", st["1"].IPLimitState)
	}
	snap := s.NodeIPSnapshot("1")
	if len(snap.IPs) != 2 {
		t.Fatalf("granted IPs 应为 2, got %d", len(snap.IPs))
	}
	if len(snap.Rejected) != 1 {
		t.Fatalf("rejected 应为 1, got %d", len(snap.Rejected))
	}
}

// conntrack 上一轮有 A，本轮 conntrack 已无 A 但 /proc 仍 ESTABLISHED → A 必须离线。
func TestProcStaleTCPDoesNotResurrectIP(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetRemoteIPs(procResult(map[string]bool{"9.9.9.9": true}, nil, false))

	// 第一轮：conntrack 有 A
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: "9.9.9.9", SrcPort: 1111, Bytes: 100},
		}}
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 1 {
		t.Fatalf("第一轮应在线, got %d", st["1"].ActiveIPs)
	}

	// 第二轮：conntrack 已无 A（0 flow），但 /proc 仍残留 ESTABLISHED A → 不得复活。
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: nil}
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 0 {
		t.Fatalf("conntrack 无 A 后 /proc 残留不得复活, got %d", st["1"].ActiveIPs)
	}
}

// A/B/C 在线，A/B 断开，下一轮 snapshot 只含 C。
func TestDisconnectedIPRemovedFromSnapshot(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetRemoteIPs(procResult(nil, nil, false))
	mk := func(ips ...string) connection.ConntrackResult {
		var fs []connection.ConntrackFlow
		for i, ip := range ips {
			fs = append(fs, connection.ConntrackFlow{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: ip, SrcPort: 1000 + i, Bytes: 100})
		}
		return connection.ConntrackResult{Available: true, Flows: fs}
	}
	s.SetConntrack(func(string) connection.ConntrackResult { return mk("A", "B", "C") })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	snap := s.NodeIPSnapshot("1")
	if len(snap.IPs) != 3 {
		t.Fatalf("应有 3 在线, got %d", len(snap.IPs))
	}
	// A/B 断开
	s.SetConntrack(func(string) connection.ConntrackResult { return mk("C") })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	snap = s.NodeIPSnapshot("1")
	if len(snap.IPs) != 1 || snap.IPs[0].IP != "C" {
		t.Fatalf("断开后 snapshot 应只含 C, got %+v", snap.IPs)
	}
}

// TCP 在线 IP 整条链：conntrack TCP 流 → Active State → Live Snapshot。
func TestActiveIPsIncludesTCP(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: "9.9.9.9", SrcPort: 1111, Bytes: 100},
			{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: "8.8.8.8", SrcPort: 2222, Bytes: 200},
		}}
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	ips := s.ActiveIPs("1")
	if len(ips) != 2 {
		t.Fatalf("TCP 在线 IP 应有 2 个, got %v", ips)
	}
	snap := s.NodeIPSnapshot("1")
	if len(snap.IPs) != 2 || snap.IPs[0].TCP <= 0 {
		t.Fatalf("snapshot 应含 2 个 TCP IP, got %+v", snap.IPs)
	}
}

// 半开连接（异常断网/无 FIN，conntrack 仍 ESTABLISHED 但无流量）：idle 内保持在线，超 idle 释放。
func TestHalfOpenTCPStaysOnlineUntilIdle(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetIPIdle(10 * time.Second)
	base := time.Now()
	s.SetClock(func() time.Time { return base })

	flow := connection.ConntrackFlow{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: "9.9.9.9", SrcPort: 1111, Bytes: 100}
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{flow}}
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	base = base.Add(5 * time.Second)
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 1 {
		t.Fatalf("半开但 idle 内应仍在线, got %d", st["1"].ActiveIPs)
	}

	base = base.Add(6 * time.Second)
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 0 {
		t.Fatalf("半开超 idle 应释放, got %d", st["1"].ActiveIPs)
	}
}
