package policy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// benchService 构造一个含 N 节点、每节点 M 个活跃客户端 IP 的策略服务，
// conntrack 注入为内存数据（不读 /proc），用于测 reconcile 每秒的真实工作量。
func benchService(tb testing.TB, nNodes, ipsPerNode int) *Service {
	tb.Helper()
	dir := tb.TempDir()
	s := newBenchServiceDB(tb, dir)
	s.nftApply = func(ctx context.Context, path string) error { return nil }
	s.tableProbe = func() bool { return true }
	s.enforceMinInterval = 0
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{"10.0.0.1": true}, nil })

	// 写 nodes.json
	list := make([]nodes.Node, 0, nNodes)
	for i := 1; i <= nNodes; i++ {
		list = append(list, nodes.Node{"id": int64(i), "type": "vless", "port": int64(10000 + i), "name": fmt.Sprintf("n%d", i)})
	}
	if err := nodes.SaveNodesFile(s.nodesPath(), list); err != nil {
		tb.Fatal(err)
	}

	// 写 node_policy（开启 IP 限制，让 admission 真正跑）
	ctx := context.Background()
	for i := 1; i <= nNodes; i++ {
		cfg := Config{NodeID: fmt.Sprint(i), IPLimitEnabled: true, IPLimitMax: ipsPerNode + 5}
		if err := s.UpsertConfig(ctx, cfg); err != nil {
			tb.Fatal(err)
		}
	}

	// 注入 conntrack：每节点 ipsPerNode 个 ESTABLISHED 流
	flows := make([]connection.ConntrackFlow, 0, nNodes*ipsPerNode)
	for i := 1; i <= nNodes; i++ {
		port := 10000 + i
		for j := 0; j < ipsPerNode; j++ {
			flows = append(flows, connection.ConntrackFlow{
				Proto: "tcp", State: "ESTABLISHED",
				SrcIP: fmt.Sprintf("203.0.113.%d", j%254+1), SrcPort: 40000 + j,
				DstPort: port, Bytes: int64(1000 + j),
			})
		}
	}
	s.SetConntrack(func(path string) connection.ConntrackResult {
		return connection.ConntrackResult{Flows: flows, Available: true, Entries: len(flows)}
	})
	return s
}

func newBenchServiceDB(tb testing.TB, dir string) *Service {
	tb.Helper()
	db, err := database.Open(dir + "/traffic.db")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return New(db.DB, dir, dir+"/policy.nft")
}

// BenchmarkReconcile 测每秒一轮 reconcile 的真实成本（含 conntrack 解析、
// SQL 两查、admission、nft 脚本生成，但 nftApply 为 no-op）。
func BenchmarkReconcile(b *testing.B) {
	for _, c := range []struct{ nodes, ips int }{{5, 5}, {50, 10}, {50, 50}} {
		b.Run(fmt.Sprintf("nodes=%d/ips=%d", c.nodes, c.ips), func(b *testing.B) {
			s := benchService(b, c.nodes, c.ips)
			ctx := context.Background()
			// 预热两轮（建立 flow 基线 + slot）
			s.SetClock(func() time.Time { return time.Now() })
			_ = s.reconcile(ctx)
			_ = s.reconcile(ctx)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.reconcile(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLoadConfigs 单独测 loadConfigs 的 SQL 成本。
func BenchmarkLoadConfigs(b *testing.B) {
	s := benchService(b, 50, 10)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.loadConfigs(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
