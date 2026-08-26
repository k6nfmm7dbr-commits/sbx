package api

import (
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
		IptScript: filepath.Join(dir, "iptables.sh"),
		Backend:   "nft", Listen: "127.0.0.1", Port: 8080,
		Token: token, Interval: 2, TZ: "UTC",
	}
	pol := policy.New(db.DB, dir, filepath.Join(dir, "policy.nft"))
	s, _ := New(cfg, db, &fakeSource{backend: "nft"}, pol)
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); db.Close() })
	return ts, pol
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
	if st["quota_enabled"] != false || st["quota_state"] != "unlimited" {
		t.Fatalf("默认应全不限: %+v", st)
	}

	// PUT 设置 quota
	putBody := `{"quota_enabled":true,"quota_limit_bytes":1073741824,"ip_limit_enabled":true,"ip_limit_max":2}`
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
	if rr.StatusCode != 200 || !strings.Contains(string(buf[:n]), `"quota_enabled":true`) {
		t.Fatalf("PUT policy 应 200 且生效: %d %s", rr.StatusCode, buf[:n])
	}

	// 参数校验：quota enabled 但 limit=0 → 400
	bad := `{"quota_enabled":true,"quota_limit_bytes":0}`
	req2, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/nodes/1/policy", strings.NewReader(bad))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testToken)
	rr2, _ := http.DefaultClient.Do(req2)
	rr2.Body.Close()
	if rr2.StatusCode != 400 {
		t.Fatalf("quota limit=0 应 400, got %d", rr2.StatusCode)
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
