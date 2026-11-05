package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

// F1 第二道防线：即使绕过 Serve 的启动闸门直接构造 Server，
// 反代信号 + 空 token 也不能再让管理 API 无鉴权可达（原 PoC 场景）。
func TestAuthorizedFailsClosedBehindReverseProxy(t *testing.T) {
	for _, cfg := range []*config.Config{
		{Listen: "127.0.0.1", SecureCookie: true},
		{Listen: "127.0.0.1", TrustedOrigin: "https://panel.example.com"},
	} {
		s := &Server{cfg: cfg}
		for _, path := range []string{"/api/nodes", "/api/export", "/"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if s.authorized(req) {
				t.Errorf("cfg=%+v path=%s: 空 token + 反代信号不应放行", cfg, path)
			}
		}
	}
	// 本机直连且无反代信号：保留既有的空 token 免登录行为。
	s := &Server{cfg: &config.Config{Listen: "127.0.0.1"}}
	if !s.authorized(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("本机直连且无反代信号时，空 token 仍应免登录")
	}
}

// 端到端：按原渗透 PoC 一模一样构造反代 + 空 token 的服务，
// 所有敏感端点必须 401，而不是 200 / 业务码 / 500。
func TestReverseProxyEmptyTokenReturns401(t *testing.T) {
	cfg := &config.Config{
		Token: "", Listen: "127.0.0.1", Port: 8080,
		SecureCookie: true, TrustedOrigin: "https://panel.example.com",
	}
	s, _ := New(cfg, nil, nil, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	for _, path := range []string{"/api/nodes", "/api/summary", "/api/export"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: 期望 401, got %d", path, resp.StatusCode)
		}
	}
}
