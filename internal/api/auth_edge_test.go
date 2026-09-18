package api

import (
	"net/http"
	"strings"
	"testing"
)

// tokenEqual 的边界补测（审计项「Token 鉴权时序攻击」）。
//
// 实现侧已满足要求：internal/api/auth.go 用 crypto/subtle.ConstantTimeCompare
// 做恒定时间比较，并先比长度（长度本身不是保密信息）。这里补齐清单要求的
// 边界用例，防止将来被"优化"成 `==`。
//
// 注意：**不做**基于计时断言的测试——CI 上噪声远大于信号，必然 flaky。
// 恒定时间属性由实现事实保证（auth.go 只调用 subtle.ConstantTimeCompare），
// 代码评审时以「是否仍调用 subtle」为检查点即可。
func TestTokenEqualEdgeCases(t *testing.T) {
	tok := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name  string
		given string
		want  bool
	}{
		{"正确", tok, true},
		{"大小写敏感-全大写", strings.ToUpper(tok), false},
		{"大小写敏感-单字符", "0123456789abcdef0123456789abcdeF", false},
		{"首尾空白不裁剪", " " + tok + " ", false},
		{"前导空白", " " + tok, false},
		{"短一字节", tok[:len(tok)-1], false},
		{"长一字节", tok + "0", false},
		{"空串", "", false},
		{"仅空白", " ", false},
		{"多字节 UTF-8 等字节数", strings.Repeat("é", 16), false}, // 16 runes = 32 bytes
		{"NUL 注入", tok[:15] + "\x00", false},
		{"换行截断尝试", tok[:15] + "\n", false},
	}
	for _, c := range cases {
		if got := tokenEqual(c.given, tok); got != c.want {
			t.Errorf("%s: tokenEqual(%q)=%v want %v", c.name, c.given, got, c.want)
		}
	}
}

// 服务端 token 为空时不得把空串当作"匹配成功"——放行与否由 authorized 的
// 显式分支决定（token=="" → 完全开放，配合 serve 层的非回环拒绝启动兜底），
// 而不是让空串走进比较逻辑。
func TestTokenEqualEmptyConfiguredToken(t *testing.T) {
	// 空 token 与任意输入都不应"相等"（否则等于把空口令当成万能口令）
	for _, given := range []string{"", "x", "00000000000000000000000000000000"} {
		if tokenEqual(given, "") {
			t.Errorf("tokenEqual(%q, \"\") 必须为 false", given)
		}
	}
}

// 未配置 token 的面板保持"完全开放"的既有语义（serve 层已用 listenIsPublic
// 拒绝"公网监听 + 空 token"的组合，此处只锁定 HTTP 层行为不被误改）。
func TestEmptyTokenServerStaysOpen(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{backend: "nft"}, "")
	if r := doReq(t, ts, http.MethodGet, "/api/summary", nil); r.StatusCode != 200 {
		t.Errorf("未配置 token 时应放行（与旧行为一致）, got %d", r.StatusCode)
	}
}

// Bearer 前缀大小写：沿用旧实现的大小写敏感匹配（只认 "Bearer "）。
// RFC 7235 允许 scheme 大小写不敏感，但这里保持与历史行为一致以避免
// 改变既有部署的可观测行为；如需放宽属于行为变更，应单独评估。
func TestBearerSchemeCaseSensitive(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")
	ok := map[string]string{"Authorization": "Bearer " + testToken}
	if r := doReq(t, ts, http.MethodGet, "/api/summary", ok); r.StatusCode != 200 {
		t.Fatalf("标准 Bearer 应放行, got %d", r.StatusCode)
	}
	for _, bad := range []string{"bearer " + testToken, "BEARER " + testToken, "Bearer" + testToken} {
		r := doReq(t, ts, http.MethodGet, "/api/summary", map[string]string{"Authorization": bad})
		if r.StatusCode != 401 {
			t.Errorf("Authorization=%q 应 401（保持历史大小写敏感行为）, got %d", bad, r.StatusCode)
		}
	}
}

// Cookie 解析：多个同名 cookie 取第一个；值做百分号解码（不把 + 当空格）。
func TestCookieTokenParsing(t *testing.T) {
	ts, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")
	cases := []struct {
		name   string
		cookie string
		want   int
	}{
		{"单值", "sbx_token=" + testToken, 200},
		{"混在其它 cookie 中", "a=1; sbx_token=" + testToken + "; b=2", 200},
		{"同名取第一个", "sbx_token=" + testToken + "; sbx_token=wrong", 200},
		{"第一个错误值", "sbx_token=wrong; sbx_token=" + testToken, 401},
		{"空值", "sbx_token=", 401},
		{"无关 cookie", "other=1", 401},
	}
	for _, c := range cases {
		r := doReq(t, ts, http.MethodGet, "/api/summary", map[string]string{"Cookie": c.cookie})
		if r.StatusCode != c.want {
			t.Errorf("%s: got %d want %d", c.name, r.StatusCode, c.want)
		}
	}
}
