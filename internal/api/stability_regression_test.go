package api

// 稳定性专项回归（全面检阅轮）：本文件覆盖该轮修复的四个缺陷，
// 防止它们在后续重构中回退。
//
//  1. /api/export 改为分批查询：库连接池只有 1 条连接，一条不关的 rows
//     会让下载期间采集入账与策略 reconcile 全部排队；超过单批大小的
//     导出必须仍然完整（LIMIT/OFFSET 分页边界不能丢行/重复行）。
//  2. /api/node-domains 的 day 参数必须受 180 天保留期约束（早于保留期
//     或未来日期 → 400），与 days 上限同一口径。
//  3. ip_limit_max 必须有上界（与 rate_limit_mbps 口径一致），防止误填
//     天文数字直接进 nft enforcement。
//  4. 节点变更 422 回显的 CLI 错误不得携带本机绝对路径。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportCSVBeyondBatchSize：超过单批（1000 行）的导出必须完整且不重复。
func TestExportCSVBeyondBatchSize(t *testing.T) {
	ts, db, _ := newTestServer(t, "", &fakeSource{hasConns: true}, "")
	const total = 2500 // > batchSize(1000)，覆盖两段分页 + 尾批
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,1,2,3,4)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < total; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("2024-01-%02d", i%28+1), fmt.Sprintf("node:%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// scope 唯一，行数可控；day 在 1..28 循环不影响 ORDER BY 的分页正确性。
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	resp := doReq(t, ts, http.MethodGet, "/api/export", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status=%d", resp.StatusCode)
	}
	// 不能用共享的 body() helper：它只做一次 64KB Read，这里的 CSV 有约
	// 70KB，会被截断——必须读完整响应。
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != total+1 { // + header
		t.Fatalf("导出行数应为 %d（含 header），实得 %d", total+1, len(lines))
	}
	seen := make(map[string]bool, total)
	for _, l := range lines[1:] {
		scope := strings.SplitN(l, ",", 3)[1]
		if seen[scope] {
			t.Fatalf("分页边界丢/重行: %s 出现多次", scope)
		}
		seen[scope] = true
	}
}

// TestNodeDomainsDayOutsideRetention：day 越过保留期或指向未来 → 400。
func TestNodeDomainsDayOutsideRetention(t *testing.T) {
	ts, _, _, _, _ := newNodeCRUDTestServer(t)
	for _, path := range []string{
		"/api/node-domains?id=1&day=1999-12-31", // 远早于 180 天保留期
		"/api/node-domains?id=1&day=2999-01-01", // 未来日期
	} {
		if code, _ := doJSON(t, ts, http.MethodGet, path, ""); code != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实得 %d", path, code)
		}
	}
}

// TestPolicyIPLimitUpperBound：ip_limit_max 超过上界必须 400。
func TestPolicyIPLimitUpperBound(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesFile, `[{"id":1,"type":"vless","port":443,"name":"n1"}]`)
	ts, _ := newPolicyTestServer(t, testToken, nodesFile)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"ip_limit_enabled":true,"ip_limit_max":65535}`, http.StatusOK},
		{`{"ip_limit_enabled":true,"ip_limit_max":65536}`, http.StatusBadRequest},
		{`{"ip_limit_enabled":true,"ip_limit_max":0}`, http.StatusBadRequest},
		{`{"ip_limit_enabled":true,"ip_limit_max":-5}`, http.StatusBadRequest},
	} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/nodes/1/policy", strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		rr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rr.Body.Close()
		if rr.StatusCode != c.want {
			t.Errorf("body=%s 应得 %d，实得 %d", c.body, c.want, rr.StatusCode)
		}
	}
}

// TestRespondNodeMutationErrorRedactsLocalPaths：422 回显不得带本机绝对路径。
func TestRespondNodeMutationErrorRedactsLocalPaths(t *testing.T) {
	raw := "nodes.json 读取失败: open /etc/sbx/nodes.json: permission denied; see /var/lib/sing-box/config.json"
	s := &Server{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nodes", nil)
	s.respondNodeMutationError(rr, req, fmt.Errorf("%s", raw))
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp["error"], "/etc/") || strings.Contains(resp["error"], "/var/") {
		t.Fatalf("错误响应泄露本地路径: %q", resp["error"])
	}
	// 可操作的语义信息（非路径部分）必须保留。
	if !strings.Contains(resp["error"], "permission denied") {
		t.Fatalf("脱敏不应丢掉错误语义: %q", resp["error"])
	}
	// 普通业务错误（无路径）必须原样保留。
	rr2 := httptest.NewRecorder()
	s.respondNodeMutationError(rr2, req, fmt.Errorf("端口 %d 已被占用", 33973))
	var resp2 map[string]string
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2["error"] != "端口 33973 已被占用" {
		t.Fatalf("无路径错误应原样回显: %q", resp2["error"])
	}
}
