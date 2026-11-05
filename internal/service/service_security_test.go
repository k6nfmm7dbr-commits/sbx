package service

import (
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

// F1：反代部署的两个配置信号必须算「对外可达」，否则 listen=127.0.0.1
// 会被误判成仅本机，空 token 时全部管理 API 无鉴权。
func TestPanelExposedIncludesReverseProxySignals(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		secure bool
		origin string
		want   bool
	}{
		{"loopback 且无反代信号", "127.0.0.1", false, "", false},
		{"localhost", "localhost", false, "", false},
		{"IPv6 loopback", "::1", false, "", false},
		{"公网监听", "0.0.0.0", false, "", true},
		{"未配置监听等价于全接口", "", false, "", true},
		{"loopback + secure_cookie", "127.0.0.1", true, "", true},
		{"loopback + trusted_origin", "127.0.0.1", false, "https://panel.example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Listen: tc.listen, SecureCookie: tc.secure, TrustedOrigin: tc.origin}
			if got := panelExposed(cfg); got != tc.want {
				t.Fatalf("panelExposed = %v, want %v", got, tc.want)
			}
		})
	}
}

// F1：启动闸门必须覆盖反代信号，且只在空 token 时触发；有 token 的正常部署不受影响。
func TestRejectEmptyTokenGate(t *testing.T) {
	t.Setenv("SBX_ALLOW_EMPTY_TOKEN", "")
	const validToken = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"本机回环 + 空 token（保留既有免登录）", &config.Config{Listen: "127.0.0.1"}, false},
		{"公网监听 + 空 token", &config.Config{Listen: "0.0.0.0"}, true},
		{"loopback + secure_cookie + 空 token", &config.Config{Listen: "127.0.0.1", SecureCookie: true}, true},
		{"loopback + trusted_origin + 空 token", &config.Config{Listen: "127.0.0.1", TrustedOrigin: "https://panel.example.com"}, true},
		{"公网监听 + 有效 token", &config.Config{Listen: "0.0.0.0", Token: validToken}, false},
		{"反代 + 有效 token", &config.Config{Listen: "127.0.0.1", SecureCookie: true, Token: validToken}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rejectEmptyToken(tc.cfg); got != tc.want {
				t.Fatalf("rejectEmptyToken = %v, want %v", got, tc.want)
			}
		})
	}

	t.Setenv("SBX_ALLOW_EMPTY_TOKEN", "1")
	if rejectEmptyToken(&config.Config{Listen: "127.0.0.1", SecureCookie: true}) {
		t.Fatal("显式逃生开关应放行空 token")
	}
}

func TestAllowEmptyTokenRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("SBX_ALLOW_EMPTY_TOKEN", "")
	if allowEmptyToken() {
		t.Fatal("默认必须关闭")
	}
	t.Setenv("SBX_ALLOW_EMPTY_TOKEN", "1")
	if !allowEmptyToken() {
		t.Fatal("显式设置 1 后应开启")
	}
	t.Setenv("SBX_ALLOW_EMPTY_TOKEN", "true")
	if allowEmptyToken() {
		t.Fatal("只接受精确的 1，避免误开")
	}
}
