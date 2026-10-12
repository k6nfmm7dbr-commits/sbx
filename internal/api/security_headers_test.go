package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

func newSecurityHeaderTestServer(t *testing.T, secureCookie bool) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DB: db.Path(), NodesFile: filepath.Join(dir, "nodes.json"),
		NftConf: filepath.Join(dir, "nft.conf"), Listen: "127.0.0.1", Port: 8080,
		Token: testToken, Interval: 2, TZ: "UTC", SecureCookie: secureCookie,
	}
	srv, _ := New(cfg, db, &fakeSource{backend: "nft"}, nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); _ = db.Close() })
	return ts
}

func assertSecurityHeaders(t *testing.T, h http.Header, secure bool) {
	t.Helper()
	if h.Get("Permissions-Policy") != "geolocation=(), microphone=(), camera=()" {
		t.Errorf("Permissions-Policy 缺失或错误: %q", h.Get("Permissions-Policy"))
	}
	if secure {
		if got := h.Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
			t.Errorf("secure_cookie=true 时 HSTS 错误: %q", got)
		}
	} else if got := h.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("纯 HTTP 模式不应设置 HSTS，got %q", got)
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
		"Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("既有安全头 %s 被改变: got %q want %q", name, got, want)
		}
	}
}

func TestSecurityHeadersRespectSecureCookie(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https-proxy"}[secure], func(t *testing.T) {
			ts := newSecurityHeaderTestServer(t, secure)
			resp, err := http.Get(ts.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			assertSecurityHeaders(t, resp.Header, secure)
			resp.Body.Close()

			req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/export", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			assertSecurityHeaders(t, resp.Header, secure)
			resp.Body.Close()
		})
	}
}

func TestAPILogoutClearsCookieAndRequiresAuthentication(t *testing.T) {
	ts := newSecurityHeaderTestServer(t, true)
	resp := doReq(t, ts, http.MethodPost, "/api/logout", map[string]string{"Cookie": "sbx_token=" + testToken})
	if resp.StatusCode != http.StatusOK {
		got := body(t, resp)
		t.Fatalf("authenticated logout should return 200, got %d: %s", resp.StatusCode, got)
	}
	var cleared *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "sbx_token" {
			cleared = c
		}
	}
	if cleared == nil || cleared.Path != "/" || cleared.MaxAge >= 0 || !cleared.HttpOnly || !cleared.Secure || cleared.SameSite != http.SameSiteLaxMode {
		t.Fatalf("logout must expire the secure HttpOnly cookie at Path=/, got %+v", cleared)
	}
	body(t, resp)

	resp = doReq(t, ts, http.MethodPost, "/api/logout", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout without credentials should be 401, got %d: %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
}
