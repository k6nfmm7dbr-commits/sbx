package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
)

// ---- 出站流误判（本机自身发起的连接不是客户端） -------------------------

// conntrack 原方向元组是 src=本机 dst=远端 dport=远端端口。节点监听 443/8443
// 这类常见端口时，服务器自己 curl https:// 的流会被误判成该节点的客户端：
// 占 slot、虚报在线 IP，IP 限制下把真实用户挤掉。
func TestOutboundFlowNotCountedAsClient(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetLocalAddrs(func() (map[string]bool, error) {
		return map[string]bool{"203.0.113.7": true, "127.0.0.1": true}, nil
	})
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			// 本机出站：src 是本机地址
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "203.0.113.7", SrcPort: 51234, DstPort: 443, Bytes: 12345},
		}}
	})
	s.SetRemoteIPs(procResult(nil, nil, false))

	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 0 {
		t.Errorf("本机出站流被误判为客户端: ActiveIPs=%d (期望 0)", st["1"].ActiveIPs)
	}
	if n := s.activeTCPConn("1"); n != 0 {
		t.Errorf("本机出站流被计入节点 TCP 连接数: %d (期望 0)", n)
	}
}

// 真实客户端流（源 IP 不是本机）必须照常统计，过滤不能过度。
func TestRealClientStillCountedWithSelfIPFilter(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetLocalAddrs(func() (map[string]bool, error) {
		return map[string]bool{"203.0.113.7": true}, nil
	})
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "203.0.113.7", SrcPort: 51234, DstPort: 443, Bytes: 1},
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "198.51.100.5", SrcPort: 40000, DstPort: 443, Bytes: 999},
		}}
	})
	s.SetRemoteIPs(procResult(nil, nil, false))
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Fatalf("真实客户端应在线, ActiveIPs=%d", st["1"].ActiveIPs)
	}
	if ips := s.ActiveIPs("1"); len(ips) != 1 || ips[0] != "198.51.100.5" {
		t.Errorf("在线 IP 应只含真实客户端, got %v", ips)
	}
}

// /proc 回退路径同样要排除本机地址。
func TestProcFallbackExcludesSelfIP(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetLocalAddrs(func() (map[string]bool, error) {
		return map[string]bool{"203.0.113.7": true}, nil
	})
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: false}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"203.0.113.7": true, "198.51.100.5": true}, nil, false))
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Errorf("/proc 回退应排除本机地址, ActiveIPs=%d (期望 1)", st["1"].ActiveIPs)
	}
}

// ---- nf_conntrack_acct=0 降级 -------------------------------------------

// Debian/Ubuntu 默认 nf_conntrack_acct=0，conntrack 无 bytes= → Bytes 恒为 0。
// 字节增量判活会把正在使用的连接在 ipIdle 后判死并从 allow set 移除（真踢线）。
func TestAcctDisabledKeepsEstablishedOnline(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetIPIdle(60 * time.Second)
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "198.51.100.9", SrcPort: 40000, DstPort: 443, Bytes: 0},
		}}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"198.51.100.9": true}, nil, false))

	ctx := context.Background()
	base := s.now()
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 1 {
		t.Fatalf("首轮应在线, got %d", st["1"].ActiveIPs)
	}

	// 远超 ipIdle：连接仍 ESTABLISHED，客户端还在用。
	s.SetClock(func() time.Time { return base.Add(5 * time.Minute) })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].ActiveIPs != 1 {
		t.Errorf("acct=0 时活跃连接被误判离线: ActiveIPs=%d (期望 1)", st["1"].ActiveIPs)
	}
	if n := s.activeTCPConn("1"); n != 1 {
		t.Errorf("acct=0 时 TCP 连接数被误判归零: %d (期望 1)", n)
	}
}

// 混合状态：运行中执行 sysctl -w nf_conntrack_acct=1 只对新建流生效，
// 老流终生 bytes=0。此时不能因为「全局看到有计费流」就按字节判死老流。
func TestMixedAcctKeepsUnaccountedFlowOnline(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetIPIdle(30 * time.Second)
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })
	// 老流(bytes=0，acct 开启前建立) + 新流(bytes>0)
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "198.51.100.1", SrcPort: 40001, DstPort: 443, Bytes: 0},
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "198.51.100.2", SrcPort: 40002, DstPort: 443, Bytes: 7777},
		}}
	})
	s.SetRemoteIPs(procResult(nil, nil, false))

	ctx := context.Background()
	base := s.now()
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 2 {
		t.Fatalf("首轮两个客户端都应在线, got %d", st["1"].ActiveIPs)
	}

	// 超过 ipIdle：老流(bytes 恒 0) 必须仍在线；新流 bytes 不变 → 判死
	s.SetClock(func() time.Time { return base.Add(31 * time.Second) })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	ips := s.ActiveIPs("1")
	found := false
	for _, ip := range ips {
		if ip == "198.51.100.1" {
			found = true
		}
	}
	if !found {
		t.Errorf("无计费数据的老流被误判离线（混合 acct 状态）, 在线=%v", ips)
	}
	for _, ip := range ips {
		if ip == "198.51.100.2" {
			t.Errorf("有计费数据但字节不增长的流应被判死, 在线=%v", ips)
		}
	}
}

// acct 可用时必须保留原有「字节不增长即判死」语义（不能被降级逻辑吞掉）。
func TestAcctEnabledStillReleasesDeadFlow(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetIPIdle(30 * time.Second)
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })
	s.SetConntrack(func(string) connection.ConntrackResult {
		// Bytes 固定非零：acct 开启，但字节不再增长 → 半开死连接
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "198.51.100.9", SrcPort: 40000, DstPort: 443, Bytes: 5000},
		}}
	})
	s.SetRemoteIPs(procResult(map[string]bool{"198.51.100.9": true}, nil, false))

	ctx := context.Background()
	base := s.now()
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return base.Add(31 * time.Second) })
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].ActiveIPs != 0 {
		t.Errorf("acct 可用时无流量的死连接应被剔除, ActiveIPs=%d", st["1"].ActiveIPs)
	}
}

// ---- nodes.json 损坏必须 fail-closed ------------------------------------

// 旧实现用宽松 LoadPanelNodes：损坏时得到 nil，等价「零节点」→ 策略表被清空，
// 所有配额/IP 阻断解除，而 sing-box 仍在服务（fail-open）。
func TestBrokenNodesFileKeepsEnforcement(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	var scripts []string
	s.SetNFTApply(func(ctx context.Context, p string) error {
		b, _ := os.ReadFile(p)
		scripts = append(scripts, string(b))
		return nil
	})

	seedTotals(t, s, "1", 10240, 0)
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Snapshot(); st["1"].QuotaState != "exceeded" {
		t.Fatalf("应为 exceeded, got %q", st["1"].QuotaState)
	}
	if len(scripts) == 0 || !strings.Contains(scripts[len(scripts)-1], "quota_ports") {
		t.Fatalf("应生成 quota 阻断规则, got %v", scripts)
	}
	before := len(scripts)

	// nodes.json 损坏
	if err := os.WriteFile(filepath.Join(s.appDir, "nodes.json"),
		[]byte("{ 这不是合法 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.reconcile(ctx)
	if err == nil {
		t.Fatal("nodes.json 损坏必须返回错误")
	}
	if !strings.Contains(err.Error(), "保持上一轮") {
		t.Errorf("错误信息应说明已保持上一轮 enforcement, got %v", err)
	}
	if len(scripts) != before {
		t.Errorf("nodes.json 损坏后不得重写策略脚本, 新增了 %d 次", len(scripts)-before)
	}
	// 状态也不能被清空
	if st, _ := s.Snapshot(); st["1"].QuotaState != "exceeded" {
		t.Errorf("nodes.json 损坏后状态被清空: %+v", st)
	}
}

// ---- nft 应用失败：状态照常发布 ------------------------------------------

// nftables-only（v3.0.9）：不存在「后端不支持 enforcement」这种稳态，
// 唯一的 enforcement 故障来源是 nft 应用失败（权限/内核/瞬时错误）。
// 此时 states 必须照常发布（否则面板全显示「不限」，用户看不到真实用量），
// 错误经 lastErr → /api/summary 的 policy_error 如实呈现。
func TestNFTApplyFailureStillPublishesState(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })
	s.SetNFTApply(func(ctx context.Context, p string) error {
		return fmt.Errorf("nft 策略规则应用失败: Operation not permitted")
	})

	seedTotals(t, s, "1", 10<<20, 0)
	if err := s.UpsertConfig(ctx, Config{NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}

	if err := s.reconcile(ctx); err == nil {
		t.Fatal("nft 应用失败必须上抛（调用方需知道本轮阻断未生效）")
	}
	states, ready := s.Snapshot()
	if !ready {
		t.Fatal("状态必须照常发布(ready=true)，否则面板全显示「不限」")
	}
	if states["1"].QuotaState != "exceeded" {
		t.Errorf("用量状态应如实呈现, got %q", states["1"].QuotaState)
	}
	if !strings.Contains(s.LastError(), "nft") {
		t.Errorf("lastErr 应明确指出 nft 应用失败, got %q", s.LastError())
	}
}

// 无需阻断时不执行 nft、也不报错（不打扰未使用策略的用户）。
func TestNothingToEnforceSkipsNFT(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })
	s.SetNFTApply(func(ctx context.Context, p string) error {
		t.Error("无任何达限节点时不应执行 nft")
		return nil
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatalf("无策略需要执行时不应报错: %v", err)
	}
	if s.LastError() != "" {
		t.Errorf("不应有 lastErr, got %q", s.LastError())
	}
}

// ---- 配额基线自愈 -------------------------------------------------------

// `sbx-core reset node:1` 删掉 totals 后 lifetime 归零，若基线仍停在旧高水位，
// used 长期被 clamp 到 0，配额要重新跑满旧水位才生效。
func TestBaselineHigherThanLifetimeIsCorrected(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	// 基线 100GiB（用户点过归零），但 totals 被 reset 清空后只剩 5GiB
	if err := s.UpsertConfig(ctx, Config{
		NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 1 << 30,
		QuotaResetBaseline: 100 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	seedTotals(t, s, "1", 5<<30, 0)

	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot()
	if st["1"].QuotaState != "exceeded" {
		t.Errorf("基线高于 lifetime 应被校正后判超额: used=%d limit=%d state=%q",
			st["1"].QuotaUsed, st["1"].QuotaLimit, st["1"].QuotaState)
	}
	// 基线应已落盘为校正值（归零：totals 里现有量全部计入已用）
	cfg, err := s.GetConfig(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaResetBaseline != 0 {
		t.Errorf("基线未被持久化归零, got %d", cfg.QuotaResetBaseline)
	}
}

// ---- 孤儿策略配置清理 ---------------------------------------------------

func TestOrphanPolicyConfigPurged(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetLocalAddrs(func() (map[string]bool, error) { return map[string]bool{}, nil })

	// 节点 7 早已被 nodes CLI 删除，但策略行残留
	if err := s.UpsertConfig(ctx, Config{NodeID: "7", QuotaEnabled: true, QuotaLimitBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM node_policy WHERE node_id='7'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("孤儿策略配置未被清理, 剩余 %d 行", n)
	}
}
