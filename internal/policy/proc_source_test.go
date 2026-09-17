package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// 稳定性回归锁（v3.0.10）：conntrack 可用时绝不读 /proc 回退源。
//
// 背景：buildActivity 只在 !cr.Available 分支消费 procSplit，但早期实现
// 无条件读取并解析 4 个 /proc 文件。真机 A/B 实测该浪费为 4.1ms + 1.16MB
// 分配/秒（空载 3.78%→3.33% 单核；~2400 连接下 7.27%→5.27%），且随连接数
// 线性放大。这里注入「一调用就报错」的假 procSource，任何回归都会立刻失败。
func TestConntrackAvailableSkipsProcSource(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "shadowsocks", 30118)
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{
			Available: true,
			Entries:   3,
			Flows: []connection.ConntrackFlow{{
				Proto: "tcp", State: "ESTABLISHED",
				SrcIP: "203.0.113.9", SrcPort: 40000, DstPort: 30118, Bytes: 4096,
			}},
		}
	})
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })
	// 刻意不调用 SetRemoteIPs：走生产路径（s.remoteIPs == nil）。

	called := 0
	restore := swapProcSource(func([]nodes.Node, func(string) (string, error)) (map[string]connection.RemoteIPSet, bool, error) {
		called++
		return nil, false, errors.New("conntrack 可用时不应读取 /proc")
	})
	defer restore()

	if err := s.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 不应因未读 /proc 而失败: %v", err)
	}
	if called != 0 {
		t.Fatalf("conntrack 可用时 procSource 被调用 %d 次, 应为 0", called)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("应仅凭 conntrack 得到 1 个在线 IP, got %d", st["1"].ActiveIPs)
	}
}

// conntrack 不可用时必须回退 /proc，且回退结果被真正消费（优化不能把回退路径改坏）。
func TestProcSourceUsedWhenConntrackUnavailable(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 30118)
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: false}
	})
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	called := 0
	restore := swapProcSource(func(list []nodes.Node, _ func(string) (string, error)) (map[string]connection.RemoteIPSet, bool, error) {
		called++
		out := map[string]connection.RemoteIPSet{}
		for _, n := range list {
			out[nodes.IDString(n)] = connection.RemoteIPSet{
				TCP: map[string]bool{"198.51.100.7": true},
				UDP: map[string]bool{},
			}
		}
		return out, false, nil
	})
	defer restore()

	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("conntrack 不可用时 procSource 应被调用 1 次, got %d", called)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("回退 /proc 应得到 1 个在线 IP, got %d", st["1"].ActiveIPs)
	}
}

// swapProcSource 替换包级 /proc 回退源，返回还原函数。
func swapProcSource(fn func([]nodes.Node, func(string) (string, error)) (map[string]connection.RemoteIPSet, bool, error)) func() {
	old := procSource
	procSource = fn
	return func() { procSource = old }
}
