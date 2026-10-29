package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// setupSubServer 准备一个含单节点、已设置分享地址的面板。
func setupSubServer(t *testing.T) (base string, token string) {
	t.Helper()
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	dir := filepath.Dir(nodesFile)
	writeTemp(t, filepath.Join(dir, "state.json"), `{"host":"203.0.113.9"}`)
	writeTemp(t, nodesFile, `[{"id":1,"type":"shadowsocks","name":"HK","port":30001,`+
		`"method":"aes-128-gcm","password":"pw"}]`)

	code, info := doJSON(t, ts, http.MethodGet, "/api/sub", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/sub = %d", code)
	}
	tok, _ := info["token"].(string)
	if tok == "" {
		t.Fatal("订阅令牌为空")
	}
	return ts.URL, tok
}

func getRaw(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url) // 刻意不带 Authorization：订阅客户端不会带 Cookie/Bearer
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestSubscriptionBase64(t *testing.T) {
	base, tok := setupSubServer(t)

	code, body, hdr := getRaw(t, base+"/sub/"+tok)
	if code != http.StatusOK {
		t.Fatalf("code = %d body = %s", code, body)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("Content-Type = %s", hdr.Get("Content-Type"))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
	if err != nil {
		t.Fatalf("订阅体不是 base64: %v (%q)", err, body)
	}
	if !strings.Contains(string(raw), "ss://") || !strings.Contains(string(raw), "203.0.113.9:30001") {
		t.Fatalf("订阅内容不含节点地址: %q", raw)
	}
}

func TestSubscriptionWrongTokenIs404(t *testing.T) {
	base, _ := setupSubServer(t)
	code, _, _ := getRaw(t, base+"/sub/deadbeefdeadbeefdeadbeefdeadbeef")
	// 刻意不是 401：不告诉扫描者"路径存在、只是令牌错了"。
	if code != http.StatusNotFound {
		t.Fatalf("错误令牌应 404，实际 %d", code)
	}
}

func TestSubscriptionSingboxProfile(t *testing.T) {
	base, tok := setupSubServer(t)

	code, body, _ := getRaw(t, base+"/sub/"+tok+"?target=singbox")
	if code != http.StatusOK {
		t.Fatalf("code = %d body = %s", code, body)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(body), &profile); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	obs, _ := profile["outbounds"].([]any)
	if len(obs) != 3 { // 1 节点 + selector + direct
		t.Fatalf("outbounds = %d, want 3", len(obs))
	}
	first := obs[0].(map[string]any)
	if first["server"] != "203.0.113.9" || first["type"] != "shadowsocks" {
		t.Fatalf("首个 outbound 错误: %+v", first)
	}
	route := profile["route"].(map[string]any)
	if route["final"] != "proxy" {
		t.Fatalf("route.final = %v", route["final"])
	}
}

func TestSubscriptionUnknownTarget(t *testing.T) {
	base, tok := setupSubServer(t)
	code, _, _ := getRaw(t, base+"/sub/"+tok+"?target=clash")
	if code != http.StatusBadRequest {
		t.Fatalf("未知 target 应 400，实际 %d", code)
	}
}

func TestSubscriptionResetRotatesToken(t *testing.T) {
	base, old := setupSubServer(t)

	req, _ := http.NewRequest(http.MethodPost, base+"/api/sub/reset", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset = %d", resp.StatusCode)
	}
	var info map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&info)
	next, _ := info["token"].(string)
	if next == "" || next == old {
		t.Fatalf("令牌未轮换: old=%s new=%s", old, next)
	}

	// 旧地址必须立刻失效，新地址立刻可用。
	if code, _, _ := getRaw(t, base+"/sub/"+old); code != http.StatusNotFound {
		t.Fatalf("旧令牌仍可用: %d", code)
	}
	if code, _, _ := getRaw(t, base+"/sub/"+next); code != http.StatusOK {
		t.Fatalf("新令牌不可用: %d", code)
	}
}

func TestSubscriptionResetRequiresPost(t *testing.T) {
	base, _ := setupSubServer(t)
	req, _ := http.NewRequest(http.MethodGet, base+"/api/sub/reset", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET reset 应 404（避免被预取），实际 %d", resp.StatusCode)
	}
}
