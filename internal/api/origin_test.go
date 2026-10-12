package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// originTestServer 构造一个已配置 trusted_origin 的 Server（空值 = 严格模式）。
// 独立于 newTestServer：这里需要带 TrustedOrigin 的 Config。
func originTestServer(t *testing.T, trustedOrigin string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "traffic.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DB:            dbPath,
		NodesFile:     filepath.Join(dir, "nodes.json"),
		NftConf:       filepath.Join(dir, "nft.conf"),
		Listen:        "127.0.0.1",
		Port:          8080,
		Token:         testToken,
		Interval:      2,
		TZ:            "UTC",
		TrustedOrigin: trustedOrigin,
	}
	s, _ := New(cfg, db, &fakeSource{backend: "nft"}, nil)
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); db.Close() })
	return ts
}

func TestUnsafeAPIOriginValidation(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")
	post := func(originHeader, referer string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", "sbx_token="+testToken)
		if originHeader != "" {
			req.Header.Set("Origin", originHeader)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("same-origin", func(t *testing.T) {
		u, _ := url.Parse(ts.URL)
		resp := post(u.Scheme+"://"+u.Host, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("同源 POST 应成功，got %d", resp.StatusCode)
		}
	})
	t.Run("same-host-different-port-rejected", func(t *testing.T) {
		// 关键回归：同主机名不同端口默认必须拒绝。
		// 浏览器按 scheme+可注册域名判定 same-site（忽略端口），Cookie 会照常带上，
		// 因此"端口无关回退"会把这层 CSRF 兜底变成摆设。
		u, _ := url.Parse(ts.URL)
		resp := post(u.Scheme+"://"+u.Hostname()+":9999", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("同 host 不同端口默认应 403，got %d", resp.StatusCode)
		}
	})
	t.Run("cross-site-origin", func(t *testing.T) {
		resp := post("https://attacker.invalid", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("跨站 POST 应 403，got %d", resp.StatusCode)
		}
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got["error_code"] != codeCrossSiteBlocked || got["request_id"] == "" {
			t.Fatalf("跨站响应缺稳定错误码/request_id: %#v", got)
		}
		// 文案可以提示 trusted_origin（排查线索），但不得泄漏来源、URL 或凭据。
		responseError, _ := got["error"].(string)
		for _, leak := range []string{"attacker.invalid", ts.URL, testToken} {
			if strings.Contains(responseError, leak) {
				t.Fatalf("跨站响应泄漏来源/内部信息 %q: %#v", leak, got)
			}
		}
	})
	t.Run("userinfo-origin-rejected", func(t *testing.T) {
		resp := post("https://user:pass@attacker.invalid", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("带 userinfo 的 Origin 应 403，got %d", resp.StatusCode)
		}
	})
	t.Run("referer-only-cross-site", func(t *testing.T) {
		resp := post("", "https://attacker.invalid/page")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("仅 Referer 时跨站 POST 应 403，got %d", resp.StatusCode)
		}
	})
	t.Run("no-origin-script-client", func(t *testing.T) {
		resp := post("", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("无 Origin/Referer 的合法 Cookie 客户端应放行，got %d", resp.StatusCode)
		}
	})
}

// trusted_origin 是反代改写 Host 的显式开关：配置后，浏览器实际发出的公开地址
// 必须被放行，而其它来源仍被拒绝。
func TestTrustedOriginAllowsProxyPortRewrite(t *testing.T) {
	ts := originTestServer(t, "https://panel.example.com")
	post := func(origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", "sbx_token="+testToken)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("trusted-origin-allowed", func(t *testing.T) {
		resp := post("https://panel.example.com")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("已配置的 trusted_origin 应放行，got %d", resp.StatusCode)
		}
	})
	t.Run("trusted-origin-default-port-allowed", func(t *testing.T) {
		// https://panel.example.com:443 与 https://panel.example.com 等价，必须都放行，
		// 否则用户会被迫在配置里写出浏览器根本不会发送的端口。
		resp := post("https://panel.example.com:443")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("显式默认端口应等价放行，got %d", resp.StatusCode)
		}
	})
	t.Run("other-origin-still-blocked", func(t *testing.T) {
		resp := post("https://evil.example.com")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("非 trusted_origin 应仍 403，got %d", resp.StatusCode)
		}
	})
	t.Run("same-host-different-port-still-blocked", func(t *testing.T) {
		resp := post("https://panel.example.com:9999")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("trusted_origin 不得放宽同 host 的其他端口，got %d", resp.StatusCode)
		}
	})
}

func TestSafeGETIgnoresCrossSiteOrigin(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")
	// 使用真实请求，保留测试服务器的 Host 与 Authorization 行为。
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://attacker.invalid")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET 带跨站 Origin 不应受 CSRF 校验影响，got %d", resp.StatusCode)
	}
}
