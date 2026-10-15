package nodes

import (
	"net"
	"testing"
)

// 新建时：只监听回环、带密钥，且端口是内核分配的合法端口。
func TestEnsureClashAPICreatesLoopback(t *testing.T) {
	cfg := map[string]any{}
	addr, secret := EnsureClashAPI(cfg)

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("地址格式非法: %q", addr)
	}
	if host != "127.0.0.1" {
		t.Errorf("必须只监听回环，实得 %q", host)
	}
	if port == "" || port == "0" {
		t.Errorf("端口应有效，实得 %q", port)
	}
	if len(secret) < 32 {
		t.Errorf("密钥过短: %d 字符", len(secret))
	}
	assertClashAPIShape(t, cfg)
}

// 幂等：重复调用必须沿用同一组地址与密钥。
// 端口一变，采集端与 sing-box 之间就会出现一段无数据的空窗。
func TestEnsureClashAPIStable(t *testing.T) {
	cfg := map[string]any{}
	addr1, secret1 := EnsureClashAPI(cfg)
	for i := 0; i < 3; i++ {
		addr2, secret2 := EnsureClashAPI(cfg)
		if addr2 != addr1 || secret2 != secret1 {
			t.Fatalf("第 %d 次调用改变了配置: %q/%q → %q/%q",
				i+1, addr1, secret1, addr2, secret2)
		}
	}
}

// 用户自建的管理接口必须原样保留，不能被覆盖。
func TestEnsureClashAPIKeepsUserConfig(t *testing.T) {
	cfg := map[string]any{
		"experimental": map[string]any{
			"clash_api": map[string]any{
				"external_controller": "127.0.0.1:19999",
				"secret":              "user-secret",
			},
			"cache_file": map[string]any{"enabled": true},
		},
	}
	addr, secret := EnsureClashAPI(cfg)
	if addr != "127.0.0.1:19999" || secret != "user-secret" {
		t.Errorf("应沿用用户配置，实得 %q/%q", addr, secret)
	}
	// experimental 里的其它键不能被碰掉
	if _, ok := cfg["experimental"].(map[string]any)["cache_file"]; !ok {
		t.Error("不应影响 experimental 下的其它配置")
	}
}

// 已有 experimental 但不是对象时不能 panic。
func TestEnsureClashAPIHandlesWeirdShapes(t *testing.T) {
	cfg := map[string]any{"experimental": "not-an-object"}
	addr, _ := EnsureClashAPI(cfg)
	if addr == "" {
		t.Fatal("应仍然写入可用的管理接口")
	}
	assertClashAPIShape(t, cfg)
}
