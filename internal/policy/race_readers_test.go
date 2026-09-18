package policy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
)

// 审计项「policy 并发安全」要求覆盖「采集线程与 API 线程同时调用」。
//
// 实现侧的并发模型（见 Service 类型注释）已经明确：runMu 串行化所有 reconcile
// 并独占 ipStates/flows；读侧只读 reconcile 末尾在 mu 下发布的不可变快照。
// 本测试的作用是把这个模型钉死在 -race 下——任何人把 ipStates 挪到读侧、
// 或忘记在 mu 下发布快照，都会立刻被检测到。
func TestReconcileConcurrentWithReaders(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "shadowsocks", 30118)
	seedNode(t, s, 2, "vless", 30119)
	// 稳定的假数据源：让 reconcile 走 conntrack 主路径，且每轮都有真实状态变化。
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{
			Available: true,
			Entries:   4,
			Flows: []connection.ConntrackFlow{
				{Proto: "tcp", State: "ESTABLISHED", SrcIP: "203.0.113.9", SrcPort: 40001, DstPort: 30118, Bytes: 1024},
				{Proto: "tcp", State: "SYN_RECV", SrcIP: "203.0.113.10", SrcPort: 40002, DstPort: 30118},
				{Proto: "udp", State: "udp", SrcIP: "198.51.100.4", SrcPort: 40003, DstPort: 30119, Bytes: 2048},
			},
		}
	})
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	const reconciles = 40
	const readers = 8

	var stop atomic.Bool
	var wg sync.WaitGroup

	// 写侧：模拟 1Hz 采集线程调用 reconcile（API 保存路径也走同一个 Reconcile）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < reconciles; i++ {
			_ = s.Reconcile(context.Background())
		}
		stop.Store(true)
	}()

	// 读侧：模拟 SSE(/api/events)、/api/summary、/api/nodes/:id/active-ips、/api/nodes/:id/ip-state
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if states, _ := s.Snapshot(); len(states) > 0 {
					_ = states["1"].ActiveIPs
				}
				_ = s.IPStateSnapshot()
				_ = s.ActiveIPs("1")
				_ = s.NodeIPSnapshot("1")
				_ = s.LastError()
			}
		}()
	}

	// 订阅/退订与广播并发（SSE 连接建立/断开）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			ch, unsub := s.Subscribe()
			select {
			case <-ch:
			default:
			}
			unsub()
		}
	}()

	// 节点删除与 reconcile 并发（DeleteNode 同时持 runMu 与 mu，锁序必须一致）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = s.DeleteNode(context.Background(), "2")
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()

	// 收尾一致性：reconcile 完成后快照必须可读且结构完整
	snap := s.IPStateSnapshot()
	if _, ok := snap["1"]; !ok {
		t.Fatal("节点 1 的快照应存在")
	}
	if got := s.NodeIPSnapshot("1"); got.NodeID != "1" {
		t.Errorf("NodeIPSnapshot 返回的 node_id 应为 1, got %q", got.NodeID)
	}
}

// 并发调用 Reconcile（多个 API 请求同时保存策略）不得交错破坏状态：
// runMu 必须把它们串行化，最终状态是某一轮的完整结果，而不是两次的混合。
func TestConcurrentReconcileIsSerialized(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "shadowsocks", 30118)
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Entries: 2, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "203.0.113.9", SrcPort: 40001, DstPort: 30118, Bytes: 512},
		}}
	})
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Reconcile(context.Background()); err != nil {
				t.Errorf("并发 Reconcile 失败: %v", err)
			}
		}()
	}
	wg.Wait()

	states, ready := s.Snapshot()
	if !ready {
		t.Fatal("reconcile 后 ready 应为 true")
	}
	if got := states["1"].ActiveIPs; got != 1 {
		t.Fatalf("并发 reconcile 后在线 IP 应为 1, got %d", got)
	}
}
