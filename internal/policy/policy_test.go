package policy

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// newTestService 构造一个带临时 SQLite 的策略服务。
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db.DB, dir, filepath.Join(dir, "policy.nft"))
	// 单元测试不依赖真实 nft（CI runner 无 netlink 权限）；仅验证脚本生成与状态机。
	s.nftApply = func(ctx context.Context, scriptPath string) error { return nil }
	return s
}

// seedNode 写入 nodes.json。
func seedNode(t *testing.T, s *Service, id int64, typ string, port int64) {
	t.Helper()
	path := s.nodesPath()
	list := nodes.LoadToolNodes(path)
	list = append(list, nodes.Node{"id": id, "type": typ, "port": port, "name": fmt.Sprintf("n%d", id)})
	if err := nodes.SaveNodesFile(path, list); err != nil {
		t.Fatal(err)
	}
}

// seedTotals 写入节点累计流量。
func seedTotals(t *testing.T, s *Service, nodeID string, rx, tx int64) {
	t.Helper()
	_, err := s.db.Exec(
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,0,0) "+
			"ON CONFLICT(scope) DO UPDATE SET rx=excluded.rx, tx=excluded.tx",
		"node:"+nodeID, rx, tx)
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuotaStates(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	seedTotals(t, s, "1", 600, 400) // lifetime = 1000

	ctx := context.Background()

	// 未启用 → unlimited
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].QuotaState != "unlimited" {
		t.Fatalf("未启用应为 unlimited, got %s", st["1"].QuotaState)
	}

	// 未达限：limit=2000, used=1000 → ok
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].QuotaState != "ok" || st["1"].QuotaUsed != 1000 {
		t.Fatalf("未达限应为 ok, used=1000, got state=%s used=%d", st["1"].QuotaState, st["1"].QuotaUsed)
	}

	// 刚好达限：limit=1000 → exceeded
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].QuotaState != "exceeded" {
		t.Fatalf("刚好达限应为 exceeded, got %s", st["1"].QuotaState)
	}

	// 提高额度恢复：limit=5000 → ok
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].QuotaState != "ok" {
		t.Fatalf("提高额度应恢复 ok, got %s", st["1"].QuotaState)
	}
}

func TestQuotaResetKeepsHistory(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	seedTotals(t, s, "1", 600, 400) // lifetime=1000
	ctx := context.Background()

	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 800}); err != nil {
		t.Fatal(err)
	}
	_ = s.reconcile(ctx)
	st, _ := s.Snapshot()
	if st["1"].QuotaState != "exceeded" {
		t.Fatalf("应 exceeded, got %s", st["1"].QuotaState)
	}

	// reset：used 归零，但 totals 不变
	if _, err := s.ResetQuota(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].QuotaUsed != 0 || st["1"].QuotaState != "ok" {
		t.Fatalf("reset 后 used=0 ok, got used=%d state=%s", st["1"].QuotaUsed, st["1"].QuotaState)
	}
	// 历史累计仍在
	var rx, tx int64
	if err := s.db.QueryRow("SELECT rx,tx FROM totals WHERE scope='node:1'").Scan(&rx, &tx); err != nil {
		t.Fatal(err)
	}
	if rx != 600 || tx != 400 {
		t.Fatalf("reset 不应删历史, got rx=%d tx=%d", rx, tx)
	}
}

func TestMigrationDefaults(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	// 新库无 node_policy 记录 → GetConfig 返回全不限
	c, err := s.GetConfig(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if c.QuotaEnabled || c.IPLimitEnabled {
		t.Fatalf("旧节点升级后应默认全不限, got quota=%v ip=%v", c.QuotaEnabled, c.IPLimitEnabled)
	}
}

// TestQuotaExceededReconcile 锁定 quota limit < used 时 reconcile 正常（不 crash，状态 exceeded）。
func TestQuotaExceededReconcile(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	seedTotals(t, s, "1", 600, 400) // lifetime = 1000
	ctx := context.Background()
	// limit=100 < used=1000 → exceeded
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatalf("quota 达限 reconcile 不应失败: %v", err)
	}
	st, _ := s.Snapshot()
	if st["1"].QuotaState != "exceeded" {
		t.Fatalf("limit<used 应 exceeded, got %s", st["1"].QuotaState)
	}
}

func TestGenPolicyNFT(t *testing.T) {
	list := []nodes.Node{{"id": int64(1), "type": "vless", "port": int64(443)}}
	script := genPolicyNFT(map[int64]bool{443: true}, nil, list)
	if script == "" {
		t.Fatal("脚本不应为空")
	}
	// quota 达限应生成端口集合
	if !containsStr(script, "quota_ports") || !containsStr(script, "443") {
		t.Errorf("quota 脚本缺端口: %s", script)
	}
	// IP limit allow set
	script2 := genPolicyNFT(nil, map[string]map[string]bool{"1": {"1.1.1.1": true}}, list)
	if !containsStr(script2, "ip_allow_1_v4") || !containsStr(script2, "1.1.1.1") {
		t.Errorf("IP limit 脚本缺 allow set: %s", script2)
	}
	// 关键：IP limit 必须只 drop「已建立」连接并放行 SYN，否则第二个 IP 拿不到 slot。
	if !containsStr(script2, "ct state established") {
		t.Errorf("IP limit 脚本缺 ct state established: %s", script2)
	}
}

func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexStr(haystack, needle) >= 0
}

func indexStr(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
