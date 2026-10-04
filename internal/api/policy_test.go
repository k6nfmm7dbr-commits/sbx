package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
)

// newPolicyTestServer 构造带真实 policy 服务的测试服务器。
func newPolicyTestServer(t *testing.T, token, nodesFile string) (*httptest.Server, *policy.Service) {
	t.Helper()
	dir := filepath.Dir(nodesFile)
	if dir == "." || dir == "" {
		dir = t.TempDir()
	}
	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DB:        filepath.Join(dir, "traffic.db"),
		NodesFile: nodesFile,
		NftConf:   filepath.Join(dir, "nft.conf"),
		Listen:    "127.0.0.1", Port: 8080,
		Token: token, Interval: 2, TZ: "UTC",
	}
	pol := policy.New(db.DB, dir, filepath.Join(dir, "policy.nft"))
	pol.SetNFTApply(func(ctx context.Context, p string) error { return nil })
	s, _ := New(cfg, db, &fakeSource{backend: "nft"}, pol)
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); db.Close() })
	return ts, pol
}

func TestToI64JSONNumber(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{json.Number("1"), 1},
		{json.Number("42"), 42},
		{int64(7), 7},
		{float64(3), 3},
		{"9", 9},
		{nil, 0},
	}
	for _, c := range cases {
		if got := toI64(c.in); got != c.want {
			t.Errorf("toI64(%v)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestPolicyEndpoints(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesFile, `[{"id":1,"type":"vless","port":443,"name":"n1"}]`)
	ts, _ := newPolicyTestServer(t, testToken, nodesFile)

	// GET policy（默认全不限）
	resp := doReq(t, ts, http.MethodGet, "/api/nodes/1/policy", map[string]string{"Authorization": "Bearer " + testToken})
	if resp.StatusCode != 200 {
		t.Fatalf("GET policy 应 200, got %d", resp.StatusCode)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body(t, resp)), &st); err != nil {
		t.Fatal(err)
	}
	if st["paused"] != false || st["ip_limit_state"] != "unlimited" {
		t.Fatalf("默认应未暂停且不限 IP: %+v", st)
	}
	if _, exists := st["quota_enabled"]; exists {
		t.Fatalf("配额字段不应再暴露: %+v", st)
	}

	// PUT 暂停节点并配置 IP limit / rate limit。
	putBody := `{"paused":true,"ip_limit_enabled":true,"ip_limit_max":2,"rate_limit_enabled":true,"rate_limit_mbps":50}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/nodes/1/policy", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Body.Close()
	buf := make([]byte, 1<<16)
	n, _ := rr.Body.Read(buf)
	if rr.StatusCode != 200 || !strings.Contains(string(buf[:n]), `"paused":true`) {
		t.Fatalf("PUT policy 应暂停节点: %d %s", rr.StatusCode, buf[:n])
	}
	if !strings.Contains(string(buf[:n]), `"rate_limit_enabled":true`) || !strings.Contains(string(buf[:n]), `"rate_limit_mbps":50`) {
		t.Fatalf("PUT policy 限速应生效: %s", buf[:n])
	}

	// 向后兼容：旧客户端省略 paused 时保留当前暂停值，不能误启用节点。
	legacyBody := `{"ip_limit_enabled":true,"ip_limit_max":2,"rate_limit_enabled":true,"rate_limit_mbps":50}`
	legacyReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/nodes/1/policy", strings.NewReader(legacyBody))
	legacyReq.Header.Set("Content-Type", "application/json")
	legacyReq.Header.Set("Authorization", "Bearer "+testToken)
	legacyResp, err := http.DefaultClient.Do(legacyReq)
	if err != nil {
		t.Fatal(err)
	}
	legacyBodyBytes := make([]byte, 1<<16)
	legacyN, _ := legacyResp.Body.Read(legacyBodyBytes)
	legacyResp.Body.Close()
	if legacyResp.StatusCode != 200 || !strings.Contains(string(legacyBodyBytes[:legacyN]), `"paused":true`) {
		t.Fatalf("省略 paused 的旧客户端必须保留暂停状态: %d %s", legacyResp.StatusCode, legacyBodyBytes[:legacyN])
	}

	// 暂停字段错误类型应被严格拒绝。
	badPaused := `{"paused":"yes"}`
	req2, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/nodes/1/policy", strings.NewReader(badPaused))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testToken)
	rr2, _ := http.DefaultClient.Do(req2)
	rr2.Body.Close()
	if rr2.StatusCode != 400 {
		t.Fatalf("paused 类型错误应 400, got %d", rr2.StatusCode)
	}

	// 参数校验：限速 enabled 但 mbps=0 → 400
	badRate := `{"rate_limit_enabled":true,"rate_limit_mbps":0}`
	req3, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/nodes/1/policy", strings.NewReader(badRate))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer "+testToken)
	rr3, _ := http.DefaultClient.Do(req3)
	rr3.Body.Close()
	if rr3.StatusCode != 400 {
		t.Fatalf("rate_limit_mbps=0 应 400, got %d", rr3.StatusCode)
	}

	// 不存在的节点 → 404
	resp = doReq(t, ts, http.MethodGet, "/api/nodes/999/policy", map[string]string{"Authorization": "Bearer " + testToken})
	if resp.StatusCode != 404 {
		t.Fatalf("不存在节点应 404, got %d", resp.StatusCode)
	}

	// 未授权 → 401
	resp = doReq(t, ts, http.MethodGet, "/api/nodes/1/policy", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("未授权应 401, got %d", resp.StatusCode)
	}

	// active-ips 返回空列表
	resp = doReq(t, ts, http.MethodGet, "/api/nodes/1/active-ips", map[string]string{"Authorization": "Bearer " + testToken})
	var ips map[string]any
	if err := json.Unmarshal([]byte(body(t, resp)), &ips); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || ips["ips"] == nil {
		t.Fatalf("active-ips 异常: %d %+v", resp.StatusCode, ips)
	}
}
