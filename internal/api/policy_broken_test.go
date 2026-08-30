package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nodes.json 损坏时策略端点必须返回 503 + 明确说明，而不是 404「not found」
// （后者让用户以为节点被删了，真实情况是配置文件坏了、策略仍按上一轮生效）。
func TestPolicyEndpointDistinguishesBrokenNodesFile(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	if err := os.WriteFile(nodesFile,
		[]byte(`[{"id":1,"name":"n1","type":"vless","port":443}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	ts, _ := newPolicyTestServer(t, "", nodesFile)

	get := func(path string) (int, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 正常：存在的节点 200；不存在的节点 404
	if code, body := get("/api/nodes/1/policy"); code != 200 {
		t.Fatalf("存在的节点应 200, got %d (%s)", code, body)
	}
	if code, body := get("/api/nodes/99/policy"); code != 404 {
		t.Fatalf("不存在的节点应 404, got %d (%s)", code, body)
	}

	// nodes.json 损坏 → 503，并说明策略维持上一轮
	if err := os.WriteFile(nodesFile, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body := get("/api/nodes/1/policy")
	if code != http.StatusServiceUnavailable {
		t.Errorf("nodes.json 损坏应返回 503, got %d (%s)", code, body)
	}
	if !strings.Contains(body, "维持上一轮") {
		t.Errorf("响应应说明策略维持上一轮状态, got %s", body)
	}
}
