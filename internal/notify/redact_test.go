package notify

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// testBotToken 是格式合法的假 token（不指向任何真实 bot）。
const testBotToken = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

func TestRedactURLHidesQueryValues(t *testing.T) {
	in := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcd-1234-efgh-5678"
	got := RedactURL(in)
	if strings.Contains(got, "abcd-1234-efgh-5678") {
		t.Fatalf("query 里的凭据未脱敏: %s", got)
	}
	if !strings.Contains(got, "key=***") {
		t.Errorf("应保留键名以便排查, 实得 %s", got)
	}
	if !strings.Contains(got, "qyapi.weixin.qq.com") {
		t.Errorf("应保留 host, 实得 %s", got)
	}
}

func TestRedactURLHidesPathSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gone string
	}{
		{"telegram bot token", "https://api.telegram.org/bot" + testBotToken + "/sendMessage", testBotToken},
		{"serverchan sendkey", "https://sctapi.ftqq.com/SCT123456abcdef.send", "SCT123456abcdef"},
		{"userinfo", "https://user:pa55w0rd@example.com/hook", "pa55w0rd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactURL(c.in)
			if strings.Contains(got, c.gone) {
				t.Fatalf("凭据未脱敏: %s", got)
			}
		})
	}
}

func TestRedactURLOnNonURL(t *testing.T) {
	// 不是合法 URL 时应退化为裸模式替换，而不是原样返回。
	got := RedactURL("connect to " + testBotToken + " failed")
	if strings.Contains(got, testBotToken) {
		t.Fatalf("裸文本里的 token 未脱敏: %s", got)
	}
}

func TestRedactSecretKeepsBotIDOnly(t *testing.T) {
	if got := RedactSecret(testBotToken); got != "123456789:***" {
		t.Errorf("bot token 应保留 id、隐藏密钥, 实得 %q", got)
	}
	if got := RedactSecret("SCT123456abcdef"); got != "***" {
		t.Errorf("非 bot token 应完全隐藏, 实得 %q", got)
	}
	if got := RedactSecret(""); got != "" {
		t.Errorf("空值应返回空, 实得 %q", got)
	}
}

// TestSafeErrNeverLeaksCredentials 是本包最重要的安全断言。
//
// net/http 把请求错误包装成 *url.Error，其 Error() **包含完整 URL**；
// 若日志直接打印 err，凭据就会落盘。这里覆盖直接与多层包装两种情形。
func TestSafeErrNeverLeaksCredentials(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		secrets []string
	}{
		{
			"telegram",
			"https://api.telegram.org/bot" + testBotToken + "/sendMessage",
			[]string{testBotToken, "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		},
		{
			"wxwork",
			"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRETKEY123456",
			[]string{"SECRETKEY123456"},
		},
		{
			"serverchan",
			"https://sctapi.ftqq.com/SCT99887766.send",
			[]string{"SCT99887766"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := &url.Error{Op: "Post", URL: c.rawURL, Err: errors.New("connection refused")}
			for _, err := range []error{
				base,
				fmt.Errorf("推送失败: %w", base),
				fmt.Errorf("dispatch: %w", fmt.Errorf("channel: %w", base)),
			} {
				got := SafeErr(err)
				for _, s := range c.secrets {
					if strings.Contains(got, s) {
						t.Fatalf("SafeErr 泄露凭据 %q: %s", s, got)
					}
				}
				if got == "" {
					t.Fatal("SafeErr 不应返回空串")
				}
			}
		})
	}
}

func TestSafeErrDescribesCause(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://example.com/hook", Err: errors.New("dial tcp: connection refused")}
	got := SafeErr(err)
	if !strings.Contains(got, "连接被拒绝") {
		t.Errorf("应给出可读的失败原因, 实得 %s", got)
	}
	if !strings.Contains(got, "example.com") {
		t.Errorf("应保留 host 便于定位, 实得 %s", got)
	}
}

func TestIsTimeout(t *testing.T) {
	if IsTimeout(nil) {
		t.Error("nil 不应判为超时")
	}
	if IsTimeout(errors.New("connection refused")) {
		t.Error("普通错误不应判为超时")
	}
	if !IsTimeout(&timeoutErr{}) {
		t.Error("实现了 net.Error 且 Timeout() 为真时应判为超时")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
