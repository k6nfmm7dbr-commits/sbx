package notify

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

const testHookToken = "1111aaaa-2222-bbbb-3333-cccc4444dddd"

func TestRedactURLHidesHookToken(t *testing.T) {
	raw := "https://open.feishu.cn/open-apis/bot/v2/hook/" + testHookToken
	got := RedactURL(raw)
	if strings.Contains(got, testHookToken) {
		t.Fatalf("hook token 未脱敏: %s", got)
	}
	if !strings.Contains(got, "open.feishu.cn") || !strings.Contains(got, "/open-apis/bot/v2/hook/") {
		t.Errorf("应保留可排查的 host 与路径结构, 实得 %s", got)
	}
}

func TestRedactURLHidesQueryAndUserinfo(t *testing.T) {
	got := RedactURL("https://user:pass@example.com/hook?key=abc&token=def#frag")
	for _, leak := range []string{"pass", "abc", "def", "frag"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q 未脱敏: %s", leak, got)
		}
	}
	if !strings.Contains(got, "key=***") || !strings.Contains(got, "token=***") {
		t.Errorf("应保留 query 键名, 实得 %s", got)
	}
}

func TestRedactURLEmpty(t *testing.T) {
	if got := RedactURL("   "); got != "" {
		t.Errorf("空串应返回空串, 实得 %q", got)
	}
}

func TestRedactSecret(t *testing.T) {
	if got := RedactSecret(""); got != "" {
		t.Errorf("空密钥应返回空串, 实得 %q", got)
	}
	if got := RedactSecret("my-secret"); got != "***" {
		t.Errorf("密钥必须整体替换, 实得 %q", got)
	}
}

// TestSafeErrHidesCredentialInURLError 是本文件最重要的一条。
//
// net/http 会把请求错误包装成 *url.Error，其 Error() 含完整 URL。
// 未经 SafeErr 直接写日志就会把 hook token 打进日志文件。
func TestSafeErrHidesCredentialInURLError(t *testing.T) {
	raw := "https://open.feishu.cn/open-apis/bot/v2/hook/" + testHookToken
	inner := fmt.Errorf("dial tcp: connection refused")
	ue := &url.Error{Op: "Post", URL: raw, Err: inner}

	got := SafeErr(ue)
	if strings.Contains(got, testHookToken) {
		t.Fatalf("凭据泄漏: %s", got)
	}
	if !strings.Contains(got, "连接被拒绝") {
		t.Errorf("应给出可读的原因, 实得 %s", got)
	}

	// 多层包装同样不能泄漏
	wrapped := fmt.Errorf("发送失败: %w", ue)
	if got := SafeErr(wrapped); strings.Contains(got, testHookToken) {
		t.Fatalf("包装后凭据泄漏: %s", got)
	}
}

func TestSafeErrNil(t *testing.T) {
	if got := SafeErr(nil); got != "" {
		t.Errorf("nil 应返回空串, 实得 %q", got)
	}
}

func TestSafeErrPlainError(t *testing.T) {
	if got := SafeErr(errors.New("boom")); got != "boom" {
		t.Errorf("普通错误应原样返回, 实得 %q", got)
	}
}
