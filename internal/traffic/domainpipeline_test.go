package traffic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// 用 httptest 冒充 sing-box 的连接表，验证「读配置 → 拉连接 → 差分 → 入库 → 查询」
// 这条完整链路（只把进程外的 sing-box 换成测试替身，其余全是真的）。
func TestDomainPipelineEndToEnd(t *testing.T) {
	var body = `{"connections":[
	  {"id":"c1","upload":100,"download":900,
	   "metadata":{"network":"tcp","type":"shadowsocks/sbx-n2","host":"shop.example.com"}},
	  {"id":"c2","upload":50,"download":50,
	   "metadata":{"network":"tcp","type":"shadowsocks/sbx-n2","host":"video.example.com"}}
	]}`
	authed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authed = true
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	sbConf := filepath.Join(dir, "config.json")
	if err := os.WriteFile(sbConf, []byte(
		`{"experimental":{"clash_api":{"external_controller":"`+addr+`","secret":"s"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := &Collector{sbConf: sbConf, domTracker: newDomainTracker(), now: time.Now}
	ctx := context.Background()
	const day = "2026-10-15"

	// 第一轮：两条新连接
	first := c.observeDomains(ctx, day)
	if !authed {
		t.Fatal("应带上密钥访问连接表")
	}
	if len(first) != 2 {
		t.Fatalf("应结算出 2 个域名，实得 %#v", first)
	}
	commitDomains(t, db, day, first)

	rows, err := QTopDomains(db.DB, day, day, "node:2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Domain != "shop.example.com" {
		t.Fatalf("应按流量倒序，实得 %#v", rows)
	}
	if rows[0].Total != 1000 || rows[0].Hits != 1 {
		t.Errorf("首轮 shop.example.com 应为 1000/1 次，实得 %+v", rows[0])
	}

	// 第二轮：同一批连接继续增长——只应累加增量，次数不再增加
	body = `{"connections":[
	  {"id":"c1","upload":300,"download":1700,
	   "metadata":{"network":"tcp","type":"shadowsocks/sbx-n2","host":"shop.example.com"}}
	]}`
	second := c.observeDomains(ctx, day)
	commitDomains(t, db, day, second)

	rows, _ = QTopDomains(db.DB, day, day, "node:2", 10)
	var shop DomainRow
	for _, r := range rows {
		if r.Domain == "shop.example.com" {
			shop = r
		}
	}
	if shop.Total != 2000 {
		t.Errorf("增量累加后应为 2000，实得 %d", shop.Total)
	}
	if shop.Hits != 1 {
		t.Errorf("连接数不应重复累加，实得 %d", shop.Hits)
	}

	// 已归类总量与未归类的计算基础
	classified, err := QDomainClassified(db.DB, day, day, "node:2")
	if err != nil {
		t.Fatal(err)
	}
	if classified != 2100 { // 1000 + 1100
		t.Errorf("已归类总量应为 2100，实得 %d", classified)
	}
}

// 配置里没有管理接口时，采集必须安静地降级（返回空、不报错、不 panic）。
func TestDomainPipelineWithoutClashAPI(t *testing.T) {
	dir := t.TempDir()
	sbConf := filepath.Join(dir, "config.json")
	if err := os.WriteFile(sbConf, []byte(`{"inbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Collector{sbConf: sbConf, domTracker: newDomainTracker(), now: time.Now}
	if got := c.observeDomains(context.Background(), "2026-10-15"); got != nil {
		t.Errorf("无管理接口时应返回空，实得 %#v", got)
	}
	if c.clashClient() != nil {
		t.Error("无管理接口时不应构造客户端")
	}
}

// 配置缺失同样不能影响采集主流程。
func TestDomainPipelineMissingConfig(t *testing.T) {
	c := &Collector{sbConf: filepath.Join(t.TempDir(), "nope.json"), domTracker: newDomainTracker(), now: time.Now}
	if got := c.observeDomains(context.Background(), "2026-10-15"); got != nil {
		t.Errorf("配置缺失时应返回空，实得 %#v", got)
	}
	if got := c.observeDomains(context.Background(), "2026-10-15"); got != nil {
		t.Error("重复调用也应安全")
	}
}

// 管理接口地址变化后必须重建客户端（配置里换了端口）。
func TestClashClientRebuildsOnConfigChange(t *testing.T) {
	dir := t.TempDir()
	sbConf := filepath.Join(dir, "config.json")
	write := func(cfg string) {
		if err := os.WriteFile(sbConf, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		// mtime 精度可能导致同秒内写入无法区分，显式推后
		future := time.Now().Add(2 * time.Second)
		_ = os.Chtimes(sbConf, future, future)
	}
	c := &Collector{sbConf: sbConf, domTracker: newDomainTracker(), now: time.Now}

	write(`{"experimental":{"clash_api":{"external_controller":"127.0.0.1:11111","secret":"a"}}}`)
	first := c.clashClient()
	if first == nil {
		t.Fatal("应构造出客户端")
	}
	write(`{"experimental":{"clash_api":{"external_controller":"127.0.0.1:22222","secret":"b"}}}`)
	second := c.clashClient()
	if second == nil || second == first {
		t.Error("配置变化后应重建客户端")
	}
}

func commitDomains(t *testing.T, db *database.DB, day string, deltas []domainDelta) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyDomainDeltas(context.Background(), tx, day, deltas); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
