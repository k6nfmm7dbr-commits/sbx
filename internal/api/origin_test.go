package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

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
	t.Run("same-host-proxy-port", func(t *testing.T) {
		u, _ := url.Parse(ts.URL)
		resp := post("https://"+u.Hostname()+":443", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("反代 scheme/port 差异应按同 host 放行，got %d", resp.StatusCode)
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
		responseError, _ := got["error"].(string)
		for _, leak := range []string{"attacker.invalid", ts.URL, testToken} {
			if strings.Contains(responseError, leak) {
				t.Fatalf("跨站响应泄漏来源/内部信息 %q: %#v", leak, got)
			}
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
