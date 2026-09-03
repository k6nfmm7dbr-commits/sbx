package policy

import (
	"context"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
)

// 真机故障复现（v3.0.8）：conntrack 模块已加载但内核未跟踪任何连接
// （干净机器上没有引用 ct 的 netfilter 规则），此时面板连接数正常
// （直接读 /proc/net/tcp）但在线 IP 恒为 0。
//
// ReadConntrack 现在对「文件可读但整表 0 条」返回 Available=false + Inactive=true，
// 判活自然走 /proc 回退分支，在线 IP 恢复正常。
func TestConntrackInactiveFallsBackToProc(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "shadowsocks", 30118)
	// 模拟未激活的 conntrack：不可用 + Inactive
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: false, Inactive: true}
	})
	// /proc 里有两条来自不同 IP 的 ESTABLISHED
	s.SetRemoteIPs(procResult(map[string]bool{"1.2.3.4": true, "5.6.7.8": true}, nil, false))
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 2 {
		t.Fatalf("conntrack 未激活时应回退 /proc 得到 2 个在线 IP, got %d", st["1"].ActiveIPs)
	}
	ips := s.ActiveIPs("1")
	if len(ips) != 2 {
		t.Fatalf("active-ips 应有 2 条, got %v", ips)
	}
	// 状态标记应被记录（用于日志去重）
	if !s.ctInactive {
		t.Error("ctInactive 应被置位")
	}

	// conntrack 恢复跟踪后回到 conntrack 口径：/proc 里的残留不再被复活
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Entries: 5, Flows: nil}
	})
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].ActiveIPs != 0 {
		t.Fatalf("conntrack 可用且 0 条相关流时不得回退 /proc, got %d", st["1"].ActiveIPs)
	}
	if s.ctInactive {
		t.Error("conntrack 恢复后 ctInactive 应清除")
	}
}
