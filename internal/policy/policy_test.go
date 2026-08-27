package policy

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
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

func TestIPLimitSlotSemantics(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()

	// 启用 max=2
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 2}); err != nil {
		t.Fatal(err)
	}

	// 直接注入 slot，验证 active 数与达限判断（绕过 /proc 读取）
	s.mu.Lock()
	s.slots["1"] = map[string]int64{
		"1.1.1.1": s.now().UnixNano(),
		"8.8.8.8": s.now().UnixNano(),
	}
	s.mu.Unlock()

	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 2 || st["1"].IPLimitState != "exceeded" {
		t.Fatalf("2 个 IP + max=2 应 exceeded, got active=%d state=%s", st["1"].ActiveIPs, st["1"].IPLimitState)
	}

	// 调高 max=3 → ok
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].IPLimitState != "ok" {
		t.Fatalf("max=3 应 ok, got %s", st["1"].IPLimitState)
	}

	// 降低 max=1：不踢现有 2 个，仅 exceeded
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].ActiveIPs != 2 || st["1"].IPLimitState != "exceeded" {
		t.Fatalf("降 max 不应踢 IP, got active=%d state=%s", st["1"].ActiveIPs, st["1"].IPLimitState)
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

func TestTCPDisconnectImmediateRelease(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 2}); err != nil {
		t.Fatal(err)
	}

	cur := map[string]connection.RemoteIPSet{
		"1": {TCP: map[string]bool{"9.9.9.9": true}, UDP: map[string]bool{}},
	}
	s.SetRemoteIPs(func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error) {
		return cur, false, nil
	})

	// 第一轮：TCP 在线
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("第一轮应 1 个在线 IP, got %d", st["1"].ActiveIPs)
	}

	// 第二轮：TCP 断开（cur.TCP 清空）——必须立即释放，不等 120s TTL
	cur["1"] = connection.RemoteIPSet{TCP: map[string]bool{}, UDP: map[string]bool{}}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].ActiveIPs != 0 {
		t.Fatalf("TCP 断开后应立即 0 个在线 IP, got %d", st["1"].ActiveIPs)
	}
}

func TestUDPTTLRelease(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "shadowsocks", 443)
	ctx := context.Background()
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 2}); err != nil {
		t.Fatal(err)
	}
	s.SetUDPTTL(120 * time.Second)

	cur := map[string]connection.RemoteIPSet{
		"1": {TCP: map[string]bool{}, UDP: map[string]bool{"8.8.8.8": true}},
	}
	s.SetRemoteIPs(func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error) {
		return cur, false, nil
	})

	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("UDP 在线应 1 个, got %d", st["1"].ActiveIPs)
	}

	// UDP 断开但未超 TTL：仍在线
	cur["1"] = connection.RemoteIPSet{TCP: map[string]bool{}, UDP: map[string]bool{}}
	s.SetClock(func() time.Time { return time.Now().Add(60 * time.Second) })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("UDP 断开 60s 内应仍在线, got %d", st["1"].ActiveIPs)
	}
}

// TestActiveIPsIncludesTCP 锁定「查看在线 IP」列表要包含 TCP 在线 IP（移动网络主要是 TCP）。
func TestActiveIPsIncludesTCP(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()

	cur := map[string]connection.RemoteIPSet{
		"1": {TCP: map[string]bool{"9.9.9.9": true, "8.8.8.8": true}, UDP: map[string]bool{}},
	}
	s.SetRemoteIPs(func(list []nodes.Node) (map[string]connection.RemoteIPSet, bool, error) {
		return cur, false, nil
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	ips := s.ActiveIPs("1")
	if len(ips) != 2 {
		t.Fatalf("ActiveIPs 应含 2 个 TCP IP, got %v", ips)
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
	if st["1"].AccessState != AccessStateQuotaBlocked || st["1"].QuotaRemaining != 0 {
		t.Fatalf("配额用尽必须暂停节点接入且剩余为 0: %+v", st["1"])
	}
}

// TestAutoResetAtDue 锁定：定时重置到期后，reconcile 会把 baseline 抬到当前
// lifetime（used 归零）并推进下一次时间。
func TestAutoResetAtDue(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	seedTotals(t, s, "1", 600, 400) // lifetime = 1000
	ctx := context.Background()

	// 先用一个已过期的下次时间戳，模拟「到期了」。
	now := s.now()
	cfg := Config{
		NodeID:          "1",
		QuotaEnabled:    true,
		QuotaLimitBytes: 1000,
		ResetEnabled:    true,
		ResetDay:        21,
		ResetTime:       "00:00:00",
		ResetNextAt:     now.Unix() - 10, // 已过期
	}
	if err := s.UpsertConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatalf("到期 reconcile 不应失败: %v", err)
	}
	st, _ := s.Snapshot()
	if st["1"].QuotaUsed != 0 {
		t.Fatalf("到期应已重置 used=0, got %d", st["1"].QuotaUsed)
	}
	if st["1"].AccessState != AccessStateOpen || st["1"].QuotaRemaining != 1000 {
		t.Fatalf("自动归零后应立即恢复节点接入并恢复全部额度: %+v", st["1"])
	}
	if st["1"].QuotaState != "ok" {
		t.Fatalf("自动归零后配额状态应恢复为 ok: %+v", st["1"])
	}
	if st["1"].ResetNextAt <= now.Unix() {
		t.Fatalf("重置后下次时间应 > now, got %d", st["1"].ResetNextAt)
	}
	// 推进必须落在未来、且是某月 21 日 00:00:00（日历锚定不漂移）。
	next := time.Unix(st["1"].ResetNextAt, 0).In(time.Local)
	if next.Day() != 21 || next.Hour() != 0 || next.Minute() != 0 || next.Second() != 0 {
		t.Fatalf("推进后应保持每月 21 日 00:00:00 相位, got %s", next)
	}
	if st["1"].ResetEnabled != true || st["1"].ResetDay != 21 {
		t.Fatalf("重置后配置应保留: %+v", st["1"])
	}
}

// TestAutoResetSkipsInvalidDay 锁定：day 非法（旧数据残留）时 reconcile
// 不执行自动重置，宁可不动也不乱归零。
func TestAutoResetSkipsInvalidDay(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	seedTotals(t, s, "1", 600, 400)
	ctx := context.Background()
	now := s.now()
	cfg := Config{
		NodeID:             "1",
		QuotaEnabled:       true,
		QuotaLimitBytes:    1000,
		QuotaResetBaseline: 0,
		ResetEnabled:       true,
		ResetDay:           0, // 非法
		ResetTime:          "00:00:00",
		ResetNextAt:        now.Unix() - 10,
	}
	if err := s.UpsertConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 不应失败: %v", err)
	}
	st, _ := s.Snapshot()
	if st["1"].QuotaUsed != 1000 {
		t.Fatalf("day 非法不应重置 used, got %d", st["1"].QuotaUsed)
	}
}

// TestAutoResetSkippedWhenQuotaOff 锁定：配额关闭时即使 reset 到期也不自动归零，
// 因为「定时重置」依附于流量配额——没有配额就没有「本期已用」可归零。
func TestAutoResetSkippedWhenQuotaOff(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	seedTotals(t, s, "1", 600, 400)
	ctx := context.Background()
	now := s.now()
	cfg := Config{
		NodeID:          "1",
		QuotaEnabled:    false,
		QuotaLimitBytes: 0,
		ResetEnabled:    true,
		ResetDay:        21,
		ResetTime:       "00:00:00",
		ResetNextAt:     now.Unix() - 10,
	}
	if err := s.UpsertConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 不应失败: %v", err)
	}
	st, _ := s.Snapshot()
	if st["1"].QuotaUsed != 1000 {
		t.Fatalf("配额关闭时不应自动重置 used=1000, got %d", st["1"].QuotaUsed)
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
