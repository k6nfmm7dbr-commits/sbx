package traffic

import (
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/clash"
)

func mkConn(id, tag, host string, up, down int64) clash.Connection {
	return clash.Connection{
		ID:       id,
		Upload:   up,
		Download: down,
		Metadata: clash.Metadata{Type: "shadowsocks/" + tag, Host: host, Network: "tcp"},
	}
}

func deltaOf(ds []domainDelta, scope, domain string) (domainDelta, bool) {
	for _, d := range ds {
		if d.scope == scope && d.domain == domain {
			return d, true
		}
	}
	return domainDelta{}, false
}

// 同一连接在两次采样之间只应计入增量；连接数只在首次见到时记一次。
func TestDomainTrackerCountsDeltaOnly(t *testing.T) {
	tr := newDomainTracker()
	const day = "2026-10-15"

	first := tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 10, 100)}, day)
	d, ok := deltaOf(first, "node:2", "a.com")
	if !ok {
		t.Fatalf("首轮应有记录: %#v", first)
	}
	if d.rx != 10 || d.tx != 100 || d.hits != 1 {
		t.Errorf("首轮应从 0 起算并计 1 次，实得 %+v", d)
	}

	second := tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 30, 500)}, day)
	d, ok = deltaOf(second, "node:2", "a.com")
	if !ok {
		t.Fatalf("次轮应有增量: %#v", second)
	}
	if d.rx != 20 || d.tx != 400 {
		t.Errorf("次轮应只计增量 (20/400)，实得 %+v", d)
	}
	if d.hits != 0 {
		t.Errorf("同一连接不应重复计次数，实得 %d", d.hits)
	}

	// 字节数没变时不应产生任何记录（避免每 tick 都写库）
	third := tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 30, 500)}, day)
	if len(third) != 0 {
		t.Errorf("无增量时不应产生记录: %#v", third)
	}
}

// 连接关闭后必须从跟踪表里移除，否则内存随连接数无限增长。
func TestDomainTrackerDropsClosedConnections(t *testing.T) {
	tr := newDomainTracker()
	const day = "2026-10-15"
	tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 10, 10)}, day)
	if len(tr.last) != 1 {
		t.Fatalf("跟踪表应有 1 项，实得 %d", len(tr.last))
	}
	tr.observe(nil, day)
	if len(tr.last) != 0 {
		t.Errorf("连接消失后应清空跟踪表，实得 %d", len(tr.last))
	}
}

// 同一个连接 ID 再次出现时视为新连接（ID 由 sing-box 生成，不复用；
// 这里只是保证不因残留状态而漏算）。
func TestDomainTrackerReappearingIDCountsAsNew(t *testing.T) {
	tr := newDomainTracker()
	const day = "2026-10-15"
	tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 10, 10)}, day)
	tr.observe(nil, day)
	again := tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 5, 5)}, day)
	d, ok := deltaOf(again, "node:2", "a.com")
	if !ok || d.hits != 1 || d.rx != 5 {
		t.Errorf("应作为新连接重新计数，实得 %+v (ok=%v)", d, ok)
	}
}

// 只统计 sbx 管理的节点：用户自建 inbound 不能被卷进来。
func TestDomainTrackerIgnoresForeignInbounds(t *testing.T) {
	tr := newDomainTracker()
	out := tr.observe([]clash.Connection{
		mkConn("c1", "user-custom-in", "a.com", 10, 10),
		mkConn("c2", "sbx-n2", "b.com", 10, 10),
		mkConn("c3", "sbx-news", "c.com", 10, 10), // 前缀相近但非 sbx 节点
		mkConn("c4", "", "d.com", 10, 10),
	}, "2026-10-15")
	if len(out) != 1 {
		t.Fatalf("应只有 1 条记录，实得 %#v", out)
	}
	if out[0].scope != "node:2" || out[0].domain != "b.com" {
		t.Errorf("只应统计 sbx-n2，实得 %+v", out[0])
	}
}

// 客户端没把域名交给服务端时，退化为目标 IP；两者都没有时给出占位符。
func TestNormalizeHostFallback(t *testing.T) {
	cases := []struct {
		name string
		meta clash.Metadata
		want string
	}{
		{"域名", clash.Metadata{Host: "A.COM."}, "a.com"},
		{"只有IP", clash.Metadata{DestinationIP: "1.2.3.4"}, "1.2.3.4"},
		{"都没有", clash.Metadata{}, unknownDomain},
		{"空白", clash.Metadata{Host: "   "}, unknownDomain},
	}
	for _, c := range cases {
		if got := normalizeHost(c.meta); got != c.want {
			t.Errorf("%s: 期望 %q，实得 %q", c.name, c.want, got)
		}
	}
}

// 超长域名要截断，防止异常 host 撑爆存储。
func TestNormalizeHostTruncates(t *testing.T) {
	long := ""
	for i := 0; i < 400; i++ {
		long += "a"
	}
	got := normalizeHost(clash.Metadata{Host: long})
	if len(got) != maxDomainLen {
		t.Errorf("应截断到 %d，实得 %d", maxDomainLen, len(got))
	}
}

// 域名条目数必须有上限：异常流量（扫描、随机子域）能让域名数无限膨胀。
func TestDomainTrackerCapsDistinctDomains(t *testing.T) {
	tr := newDomainTracker()
	const day = "2026-10-15"
	var conns []clash.Connection
	for i := 0; i < maxDomainsPerScope+50; i++ {
		conns = append(conns, mkConn("c"+itoa(i), "sbx-n2", "d"+itoa(i)+".com", 1, 1))
	}
	out := tr.observe(conns, day)

	var other int
	distinct := map[string]bool{}
	for _, d := range out {
		if d.domain == otherDomain {
			other++
			continue
		}
		distinct[d.domain] = true
	}
	if len(distinct) > maxDomainsPerScope {
		t.Errorf("独立域名数应被限制在 %d 以内，实得 %d", maxDomainsPerScope, len(distinct))
	}
	if other == 0 {
		t.Error("超出的域名应归入占位项")
	}
}

// 跨天要重置域名计数，否则第二天的额度会被前一天占用。
func TestDomainTrackerResetsAcrossDays(t *testing.T) {
	tr := newDomainTracker()
	tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 1, 1)}, "2026-10-15")
	if _, ok := tr.known["node:2"]["a.com"]; !ok {
		t.Fatal("第一天应记录域名")
	}
	tr.observe([]clash.Connection{mkConn("c2", "sbx-n2", "b.com", 1, 1)}, "2026-10-16")
	if _, ok := tr.known["node:2"]["a.com"]; ok {
		t.Error("跨天后应重置域名集合")
	}
}

// 字节数回退（理论上不该发生）不能产生负数入账。
func TestDomainTrackerClampsNegativeDelta(t *testing.T) {
	tr := newDomainTracker()
	const day = "2026-10-15"
	tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 100, 100)}, day)
	out := tr.observe([]clash.Connection{mkConn("c1", "sbx-n2", "a.com", 10, 10)}, day)
	if len(out) != 0 {
		t.Errorf("回退时不应入账，实得 %#v", out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
