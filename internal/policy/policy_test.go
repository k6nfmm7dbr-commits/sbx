package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// 默认视为没有遗留策略表；自愈与重启清理测试显式注入存在性探针。
	s.tableProbe = func() bool { return false }
	// 默认关闭应用节流：既有测试用真实时钟，多次 reconcile 间隔极短，
	// 节流会让「第二次应用」被合并而失败；节流行为由专门测试用假时钟覆盖。
	s.enforceMinInterval = 0
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

func TestPausedStateAndPersistence(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", Paused: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if !st["1"].Paused {
		t.Fatalf("paused state not published: %+v", st["1"])
	}
	cfg, err := s.GetConfig(ctx, "1")
	if err != nil || !cfg.Paused {
		t.Fatalf("paused state not persisted: cfg=%+v err=%v", cfg, err)
	}
	var script string
	if b, err := os.ReadFile(s.PolicyConfPath()); err == nil {
		script = string(b)
	}
	if !strings.Contains(script, "paused_ports") || !strings.Contains(script, "tcp dport @paused_ports drop") || !strings.Contains(script, "tcp sport @paused_ports drop") {
		t.Fatalf("paused node must be blocked in both directions: %s", script)
	}

	cfg.Paused = false
	if err := s.UpsertConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].Paused {
		t.Fatalf("resume state not published: %+v", st["1"])
	}
	b, err := os.ReadFile(s.PolicyConfPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "paused_ports") {
		t.Fatalf("resumed node still has pause rules: %s", b)
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
	if c.Paused || c.IPLimitEnabled || c.RateLimitEnabled {
		t.Fatalf("新策略默认必须是未暂停且不限流: %+v", c)
	}
}

func TestGenPolicyNFT(t *testing.T) {
	list := []nodes.Node{{"id": int64(1), "type": "vless", "port": int64(443)}}
	script := genPolicyNFT(map[int64]bool{443: true}, nil, nil, list)
	if script == "" {
		t.Fatal("脚本不应为空")
	}
	if !containsStr(script, "paused_ports") || !containsStr(script, "tcp dport @paused_ports drop") || !containsStr(script, "tcp sport @paused_ports drop") {
		t.Errorf("暂停脚本必须双向阻断节点端口: %s", script)
	}
	if containsStr(script, "quota_ports") {
		t.Errorf("脚本不应再包含配额规则: %s", script)
	}
	// IP limit allow set
	script2 := genPolicyNFT(nil, map[string]map[string]bool{"1": {"1.1.1.1": true}}, nil, list)
	if !containsStr(script2, "ip_allow_1_v4") || !containsStr(script2, "1.1.1.1") {
		t.Errorf("IP limit 脚本缺 allow set: %s", script2)
	}
	if !containsStr(script2, "ct state established") {
		t.Errorf("IP limit 脚本缺 ct state established: %s", script2)
	}
	// 限速：100 Mbps → 12500000 bytes/second（双向各一条 tcp/udp limit rate over drop）
	script3 := genPolicyNFT(nil, nil, map[int64]int{443: 100}, list)
	if !containsStr(script3, "limit rate over 12500000 bytes/second") {
		t.Errorf("限速脚本缺 limit rate: %s", script3)
	}
	if !containsStr(script3, "tcp dport 443 limit rate over") || !containsStr(script3, "tcp sport 443 limit rate over") {
		t.Errorf("限速应双向(dport 入站/sport 出站)各一条: %s", script3)
	}
}

func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexStr(haystack, needle) >= 0
}

// TestRateLimitStateAndScript 验证限速：配置生效后 State 正确、nft 脚本含双向 policer。
func TestRateLimitStateAndScript(t *testing.T) {
	var scripts []string
	s := newTestService(t)
	s.SetNFTApply(func(ctx context.Context, p string) error {
		b, _ := os.ReadFile(p)
		scripts = append(scripts, string(b))
		return nil
	})
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()

	// 未启用 → State 不限速
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].RateLimitOn {
		t.Fatalf("默认不应限速: %+v", st["1"])
	}

	// 启用 200 Mbps
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", RateLimitEnabled: true, RateLimitMbps: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if !st["1"].RateLimitOn || st["1"].RateLimitMbps != 200 {
		t.Fatalf("限速状态应为 on/200, got %+v", st["1"])
	}
	last := scripts[len(scripts)-1]
	// 200 Mbps → 25000000 bytes/second，双向各 tcp/udp
	if !containsStr(last, "limit rate over 25000000 bytes/second") {
		t.Fatalf("脚本缺 200Mbps policer: %s", last)
	}
	if !containsStr(last, "tcp dport 443 limit rate over") || !containsStr(last, "tcp sport 443 limit rate over") {
		t.Fatalf("限速应双向: %s", last)
	}

	// 关闭限速 → 规则消失
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", RateLimitEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	last = scripts[len(scripts)-1]
	if containsStr(last, "limit rate over") {
		t.Fatalf("关闭限速后脚本不应再含 policer: %s", last)
	}
}

func indexStr(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestStartupClearsStalePolicyTableWhenNoPolicyRemains(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetTableProbe(func() bool { return true }) // 持久 nft 表残留于前一进程
	var applied string
	s.SetNFTApply(func(_ context.Context, path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		applied = string(b)
		return nil
	})
	list, err := nodes.LoadPanelNodesStrict(s.nodesPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyEnforcement(context.Background(), map[string]bool{}, map[string]map[string]bool{}, map[string]int{}, list); err != nil {
		t.Fatal(err)
	}
	if applied == "" || !strings.Contains(applied, "delete table inet sbx_policy") {
		t.Fatalf("startup should replace stale strategy table with empty target: %s", applied)
	}
	if strings.Contains(applied, "paused_ports") || strings.Contains(applied, "ip_allow_") || strings.Contains(applied, "limit rate over") {
		t.Fatalf("empty current policy must not preserve stale node rules: %s", applied)
	}
}
