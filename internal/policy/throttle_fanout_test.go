package policy

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// 应用节流：仅 allow set 内容变化时，在 enforceMinInterval 窗口内合并、
// 不重写 nft；窗口过后收敛。扫描者 SYN churn 无法再诱发每秒整表重写。
func TestEnforceThrottleCoalescesAllowSetChurn(t *testing.T) {
	s := newTestService(t)
	s.enforceMinInterval = 3 * time.Second
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	s.SetClock(func() time.Time { return clock })

	seedNode(t, s, 1, "vless", 443)
	if err := s.UpsertConfig(context.Background(), Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 1}); err != nil {
		t.Fatal(err)
	}
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	applies := 0
	s.SetNFTApply(func(ctx context.Context, p string) error { applies++; return nil })

	// 第 1 轮：无活跃 IP → allow set 空 → 首次应用
	s.SetRemoteIPs(procResult(nil, nil, false))
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applies != 1 {
		t.Fatalf("首次应应用 1 次, got %d", applies)
	}

	// 第 2 轮：新 IP 被授予 slot → allow set 内容变化，但距上次 < 3s → 节流
	clock = base.Add(1 * time.Second)
	s.SetRemoteIPs(procResult(map[string]bool{"1.2.3.4": true}, nil, false))
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applies != 1 {
		t.Fatalf("窗口内 allow set 变化应被合并(仍 1 次), got %d", applies)
	}

	// 第 3 轮：窗口已过 → 收敛应用，allow set 生效
	clock = base.Add(4 * time.Second)
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applies != 2 {
		t.Fatalf("窗口过后应收敛应用(2 次), got %d", applies)
	}
	b, err := os.ReadFile(s.PolicyConfPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "1.2.3.4") {
		t.Fatalf("收敛后 allow set 应包含授予的 IP:\n%s", b)
	}
}

// 端口形态漂移：节点改端口但 allow set 不变时，也必须立即重写 nft——
// 否则留下指向旧端口的死规则，新端口失去 enforcement。
func TestPortChangeTriggersRewrite(t *testing.T) {
	s := newTestService(t)
	s.enforceMinInterval = 3 * time.Second // 即便在节流窗口内，端口变化也必须立即应用
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	s.SetClock(func() time.Time { return clock })

	seedNode(t, s, 1, "vless", 443)
	if err := s.UpsertConfig(context.Background(), Config{NodeID: "1", IPLimitEnabled: true, IPLimitMax: 1}); err != nil {
		t.Fatal(err)
	}
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(map[string]bool{"1.2.3.4": true}, nil, false))
	applies := 0
	s.SetNFTApply(func(ctx context.Context, p string) error { applies++; return nil })

	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applies != 1 {
		t.Fatalf("首次应用 1 次, got %d", applies)
	}

	// 端口 443 → 8443，活跃 IP 不变（allow set 内容不变，但端口变了）
	// 注意 seedNode 是「追加」，直接重写节点列表为新端口，避免产生重复 id。
	if err := saveNodePort(t, s, 1, "vless", 8443); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(1 * time.Second) // 距上次应用 1s < 3s 节流窗口
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applies != 2 {
		t.Fatalf("端口变化必须立即重写(即便在节流窗口内), got %d", applies)
	}
	b, err := os.ReadFile(s.PolicyConfPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dport 8443") {
		t.Fatalf("nft 应指向新端口 8443:\n%s", b)
	}
	if strings.Contains(string(b), "dport 443") {
		t.Fatalf("nft 不应残留旧端口 443 的规则:\n%s", b)
	}
}

// Subscribe 扇出：一次 reconcile 必须唤醒所有订阅者（旧实现单共享 chan 只唤醒一个）。
func TestSubscribeFanoutWakesAll(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	s.SetConntrack(func(string) connection.ConntrackResult { return connection.ConntrackResult{Available: false} })
	s.SetRemoteIPs(procResult(nil, nil, false))

	ch1, un1 := s.Subscribe()
	defer un1()
	ch2, un2 := s.Subscribe()
	defer un2()

	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("订阅者 %d 未被唤醒", i)
		}
	}
}

// saveNodePort 重写单个节点的端口（覆盖写，不产生重复 id）。
func saveNodePort(t *testing.T, s *Service, id int64, typ string, port int64) error {
	t.Helper()
	list := []nodes.Node{{
		"id":   json.Number(strconv.FormatInt(id, 10)),
		"type": typ,
		"port": json.Number(strconv.FormatInt(port, 10)),
		"name": "n" + strconv.FormatInt(id, 10),
	}}
	return nodes.SaveNodesFile(s.nodesPath(), list)
}
