package api

import (
	"context"
	"strings"
	"testing"
)

func TestValidateSNIAcceptsDomains(t *testing.T) {
	for _, v := range []string{
		"", // 空值由调用方填默认域名
		"www.bing.com", "www.microsoft.com", "panel.example.com",
		"a-b.example.com", "127.0.0.1", "xn--bcher-kva.example", "A.B.C",
	} {
		if err := validateSNI(v); err != nil {
			t.Errorf("合法 SNI %q 被拒绝: %v", v, err)
		}
	}
}

// F2：这些值会作为独立 argv 传给 sing-box，或拼进 --sni= 与配置文件。
func TestValidateSNIRejectsArgumentInjection(t *testing.T) {
	for _, v := range []string{
		"--help", "-c /etc/passwd", "--config=/etc/shadow", "-",
		"a; touch /tmp/sbx_pwned", "$(id)", "a b", "a,b", "a\"b",
		"host@evil.com", "a\nb", ".example.com", "-example.com",
		"a..b", "a/b", "a:b", "a_b", strings.Repeat("a", sniMaxLen+1),
	} {
		if err := validateSNI(v); err == nil {
			t.Errorf("非法 SNI %q 被放行（参数注入 / 结构破坏风险）", v)
		}
	}
}

// F2 端到端：恶意 SNI 必须在创建入口就被拒，绝不能走到外部命令调用。
func TestNewNodeArgsRejectsMaliciousSNI(t *testing.T) {
	for _, sni := range []string{"--help", "--config=/etc/shadow", "a; touch /tmp/sbx_pwned"} {
		req := nodeCreateRequest{Type: "trojan", SNI: sni}
		if _, err := (&Server{}).newNodeArgs(context.Background(), nil, req, nil); err == nil {
			t.Errorf("恶意 SNI %q 未在创建入口被拒绝", sni)
		}
	}
}
