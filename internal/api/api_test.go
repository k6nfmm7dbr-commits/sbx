package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// fakeSource 是可编程的采集器快照。
type fakeSource struct {
	err      string
	lastOK   int64
	conns    map[string]connection.Conns
	hasConns bool
	backend  string
}

func (f *fakeSource) Snapshot() traffic.Status {
	st := traffic.Status{Error: f.err, LastOK: f.lastOK, HasConns: f.hasConns, Conns: f.conns}
	return st
}

func (f *fakeSource) BackendName() string { return f.backend }

func newTestServer(t *testing.T, token string, src traffic.LiveSource,
	nodesFile string) (*httptest.Server, *database.DB, string) {

	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "traffic.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DB:        dbPath,
		NodesFile: nodesFile,
		NftConf:   filepath.Join(dir, "nft.conf"),
		IptScript: filepath.Join(dir, "iptables.sh"),
		Backend:   "nft", Listen: "127.0.0.1", Port: 8080,
		Token: token, Interval: 2, TZ: "UTC",
	}
	s, _ := New(cfg, db, src)
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); db.Close() })
	return ts, db, dir
}

func writeTemp(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	resp.Body.Close()
	return string(buf[:n])
}

const testToken = "tok123"

// noRedirectClient 用于断言 302 原始响应。
var noRedirectClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func TestAuthMatrix(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")

	// healthz 永远免鉴权
	resp := doReq(t, ts, http.MethodGet, "/healthz", nil)
	if resp.StatusCode != 200 || !strings.Contains(body(t, resp), `"ok":true`) {
		t.Error("healthz 异常")
	}

	// 未授权 → 401 {"error":"unauthorized"}
	resp = doReq(t, ts, http.MethodGet, "/api/summary", nil)
	if resp.StatusCode != 401 {
		t.Errorf("无令牌应 401, got %d", resp.StatusCode)
	}
	if bj := body(t, resp); !strings.Contains(bj, `"error":"unauthorized"`) {
		t.Errorf("401 内容异常: %s", bj)
	}

	// 仅 Bearer 与 Cookie 放行；query string token 已移除（防泄漏进日志/历史）
	hdr := map[string]string{"Authorization": "Bearer " + testToken}
	if r := doReq(t, ts, http.MethodGet, "/api/summary", hdr); r.StatusCode != 200 {
		t.Errorf("Bearer 应放行, got %d", r.StatusCode)
	}
	if r := doReq(t, ts, http.MethodGet, "/api/summary?token="+testToken, nil); r.StatusCode != 401 {
		t.Errorf("?token= 应拒绝(已移除 query token 认证), got %d", r.StatusCode)
	}
	if r := doReq(t, ts, http.MethodGet, "/api/summary",
		map[string]string{"Cookie": "sbx_token=" + testToken}); r.StatusCode != 200 {
		t.Errorf("Cookie 应放行, got %d", r.StatusCode)
	}
	// 空值参数视为未提供（parse_qs 兼容）
	if r := doReq(t, ts, http.MethodGet, "/api/summary?token=", nil); r.StatusCode != 401 {
		t.Errorf("空 token 参数应视为未提供, got %d", r.StatusCode)
	}
	if r := doReq(t, ts, http.MethodGet, "/api/summary",
		map[string]string{"Authorization": "Bearer wrong"}); r.StatusCode != 401 {
		t.Error("错误令牌应 401")
	}
}

func TestStaticPagesAndLoginFlow(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")

	// 未授权首页 → 登录页内容（200）
	resp := doReq(t, ts, http.MethodGet, "/", nil)
	loginBody := body(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(loginBody, "登录") ||
		!strings.Contains(loginBody, `action="/login"`) {
		t.Errorf("未授权应渲染 login.html, status=%d", resp.StatusCode)
	}
	// GET /login 渲染登录页（兼容性修复：旧版此处返回 404）
	resp = doReq(t, ts, http.MethodGet, "/login?error=1", nil)
	lb := body(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(lb, "登录") {
		t.Errorf("/login 应渲染登录页, status=%d", resp.StatusCode)
	}
	// 授权后首页
	resp = doReq(t, ts, http.MethodGet, "/", map[string]string{"Cookie": "sbx_token=" + testToken})
	idxBody := body(t, resp)
	if !strings.Contains(idxBody, "流量面板") || !strings.Contains(idxBody, `src="app.js"`) {
		t.Error("已授权应渲染 index.html")
	}
	if resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("响应头缺失")
	}
	// 静态资源免鉴权 + Content-Type
	r := doReq(t, ts, http.MethodGet, "/app.js", nil)
	ct := r.Header.Get("Content-Type")
	b := body(t, r)
	if ct != "application/javascript; charset=utf-8" || !strings.Contains(b, "fmtRate") {
		t.Errorf("app.js 异常 ct=%s", ct)
	}
	r = doReq(t, ts, http.MethodGet, "/style.css", nil)
	if r.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Error("style.css Content-Type 异常")
	}
	// HEAD 无响应体但状态正常
	hr := doReq(t, ts, http.MethodHead, "/", nil)
	if hr.StatusCode != 200 {
		t.Error("HEAD / 应 200")
	}

	// POST /login 成功 → 302 / + Cookie 属性齐全
	form := strings.NewReader("token=" + testToken)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp2, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound || resp2.Header.Get("Location") != "/" {
		t.Errorf("成功登录应 302 /, got %d %s", resp2.StatusCode, resp2.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp2.Cookies() {
		if c.Name == "sbx_token" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.MaxAge != 604800 ||
		cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("会话 Cookie 属性异常: %+v", cookie)
	}

	// POST 失败 → 302 /login?error=1
	form2 := strings.NewReader("token=nope")
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/login", form2)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp3, err := noRedirectClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusFound ||
		resp3.Header.Get("Location") != "/login?error=1" {
		t.Errorf("失败登录应 302 error=1, got %s", resp3.Header.Get("Location"))
	}
	// 其它 POST → 404 文本
	if r := doReq(t, ts, http.MethodPost, "/api/summary", nil); r.StatusCode != 404 {
		t.Error("非 /login 的 POST 应 404")
	}
}

func TestAPIEndpoints(t *testing.T) {
	dir := t.TempDir()
	nodesPath := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesPath,
		`[{"id":1,"type":"vless","port":443,"name":"n1","custom_key":"keepme"},
		  {"id":2,"type":"shadowsocks","port":8388,"name":"ss"}]`)

	intPtr := func(v int) *int { return &v }
	src := &fakeSource{
		lastOK:   1699999999,
		conns:    map[string]connection.Conns{"1": {TCP: intPtr(3)}},
		hasConns: true,
		backend:  "nft",
	}
	ts, db, _ := newTestServer(t, "", src, nodesPath)

	// summary/live 形状与关键字段
	resp := doReq(t, ts, http.MethodGet, "/api/summary", nil)
	var sum map[string]any
	if err := json.Unmarshal([]byte(body(t, resp)), &sum); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"now", "day", "tz", "interval", "backend", "healthy",
		"error", "last_sample", "nodes", "today", "total", "system_today",
		"system_total", "rate_total", "rate_known", "conns_total", "conns_udp_total"} {
		if _, ok := sum[key]; !ok {
			t.Errorf("/api/summary 缺少键 %s", key)
		}
	}
	if len(sum["nodes"].([]any)) != 2 {
		t.Error("summary 节点数应为 2")
	}

	resp = doReq(t, ts, http.MethodGet, "/api/live", nil)
	var live map[string]any
	if err := json.Unmarshal([]byte(body(t, resp)), &live); err != nil {
		t.Fatal(err)
	}
	if live["healthy"] != true || live["conns_total"] != float64(3) {
		t.Errorf("live healthy/conns_total 异常: %+v", live)
	}
	node0 := live["nodes"].([]any)[0].(map[string]any)
	if node0["conns_tcp"] != float64(3) || node0["conns_udp"] != nil {
		t.Errorf("live 节点 conns 异常: %+v", node0)
	}

	// /api/nodes 必须脱敏：只含 id/name/type/port，绝不含 secret 字段。
	resp = doReq(t, ts, http.MethodGet, "/api/nodes", nil)
	var nd struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &nd); err != nil {
		t.Fatal(err)
	}
	if len(nd.Nodes) != 2 {
		t.Fatalf("/api/nodes 节点数异常: %+v", nd.Nodes)
	}
	for _, n := range nd.Nodes {
		for _, secret := range []string{"password", "uuid", "private_key",
			"public_key", "short_id", "cert", "key", "custom_key"} {
			if _, ok := n[secret]; ok {
				t.Errorf("/api/nodes 泄漏 secret 字段 %q: %+v", secret, n)
			}
		}
	}
	if _, ok := nd.Nodes[0]["name"]; !ok {
		t.Errorf("/api/nodes 缺少展示字段 name: %+v", nd.Nodes[0])
	}

	// daily 参数处理
	if r := doReq(t, ts, http.MethodGet, "/api/daily?days=60", nil); r.StatusCode != 200 ||
		!strings.Contains(body(t, r), `"days":[`) {
		t.Error("/api/daily 异常")
	}
	if r := doReq(t, ts, http.MethodGet, "/api/daily?days=abc", nil); r.StatusCode != 400 {
		t.Error("days 非法应 400")
	}
	if r := doReq(t, ts, http.MethodGet, "/api/daily?days=100000&scope=node%3A1", nil); r.StatusCode != 200 {
		t.Error("scope+超大 days 应钳制到 365 并成功")
	}
	// 未知 API → 404 JSON；未知路径 → 404 文本
	r := doReq(t, ts, http.MethodGet, "/api/nope", nil)
	if r.StatusCode != 404 || !strings.Contains(body(t, r), `"error":"not found"`) {
		t.Error("未知 api 应 404 json")
	}
	r = doReq(t, ts, http.MethodGet, "/whatever", nil)
	if r.StatusCode != 404 || body(t, r) != "not found" {
		t.Error("未知路径应 404 文本 not found")
	}
	// 尾斜杠归一化
	if r = doReq(t, ts, http.MethodGet, "/api/live/", nil); r.StatusCode != 200 {
		t.Error("尾斜杠应归一化")
	}
	_ = db
}

// ---- 认证常量时间比较 + login body 限制（v3.0.5） -------------------------

func TestTokenEqual(t *testing.T) {
	tok := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name  string
		given string
		want  bool
	}{
		{"正确", tok, true},
		{"首字节错", "x123456789abcdef0123456789abcdef", false},
		{"中间错", "0123456789abcdefX123456789abcdef", false},
		{"末字节错", "0123456789abcdef0123456789abcdeX", false},
		{"短", "0123456789abcdef", false},
		{"长", tok + "extra", false},
		{"空", "", false},
	}
	for _, c := range cases {
		if got := tokenEqual(c.given, tok); got != c.want {
			t.Errorf("%s: tokenEqual=%v want %v", c.name, got, c.want)
		}
	}
}

func TestLoginBodyTooLarge(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")
	big := strings.Repeat("x", maxLoginBody+1)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/login", strings.NewReader("token="+big))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("超大登录体应 413, got %d", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{backend: "nft"}, "")
	resp := doReq(t, ts, http.MethodGet, "/healthz", nil)
	for hdr, want := range map[string]string{
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
		"Content-Security-Policy":   "default-src 'self'; img-src 'self' data:",
	} {
		if got := resp.Header.Get(hdr); got != want {
			t.Errorf("响应头 %s=%q want %q", hdr, got, want)
		}
	}
}

func TestIPv6AddrJoin(t *testing.T) {
	// api.New 用 net.JoinHostPort 拼接 Addr，IPv6 不应产生非法 ":::8080"
	for _, listen := range []string{"127.0.0.1", "0.0.0.0", "::1", "::"} {
		cfg := &config.Config{Listen: listen, Port: 8080, Backend: "nft"}
		s, hs := New(cfg, nil, &fakeSource{backend: "nft"})
		_ = s
		if hs.Addr == "" {
			t.Fatalf("listen=%q Addr 为空", listen)
		}
		if listen == "::" && hs.Addr == ":::8080" {
			t.Errorf("IPv6 :: Addr 非法: %q", hs.Addr)
		}
	}
}

func TestExportCSV(t *testing.T) {
	ts, db, _ := newTestServer(t, "", &fakeSource{hasConns: true}, "")
	for _, q := range []string{
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2023-11-15','node:1',3,4,5,6)",
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2023-11-14','node:1',1,2,3,4)",
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2023-11-14','system',7,8,9,10)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	resp := doReq(t, ts, http.MethodGet, "/api/export", nil)
	got := body(t, resp)
	want := "day,scope,rx_bytes,tx_bytes,rx_pkts,tx_pkts\n" +
		"2023-11-14,node:1,1,2,3,4\n" +
		"2023-11-14,system,7,8,9,10\n" +
		"2023-11-15,node:1,3,4,5,6\n"
	if got != want {
		t.Errorf("CSV 不一致:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" ||
		resp.Header.Get("Content-Disposition") != "attachment; filename=sbx-traffic.csv" {
		t.Error("CSV 响应头异常")
	}
}
