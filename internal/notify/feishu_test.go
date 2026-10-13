package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFeishuSignKnownVector 用独立算出的固定向量钉死签名算法。
//
// 向量由 Python 独立计算：
//
//	base64(hmac_sha256(key = f"{ts}\n{secret}", msg = b""))
func TestFeishuSignKnownVector(t *testing.T) {
	cases := []struct{ ts, secret, want string }{
		{"1599363739", "test-secret", "aB8EwvkBd//JjCanMLvnfugzQBeYspM3vbRaKRYkKes="},
		{"1700000000", "abc", "VIS10b0EBvzzSdFnuk4tznEmK5wHaruvf/WnViv2yR4="},
	}
	for _, c := range cases {
		if got := feishuSign(c.ts, c.secret); got != c.want {
			t.Errorf("feishuSign(%q,%q) = %q, 期望 %q", c.ts, c.secret, got, c.want)
		}
	}
}

// TestFeishuSignIsNotNaiveHMAC 反向断言：常见的错误写法必须算出不同的值。
//
// 这套 API 的参数顺序与直觉相反，写反了不会报错，只会一直返回"签名校验失败"，
// 是最费时间的坑。这条断言把它钉死，防止后人"顺手简化"。
func TestFeishuSignIsNotNaiveHMAC(t *testing.T) {
	ts, secret := "1599363739", "test-secret"
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts))
	naive := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if naive == feishuSign(ts, secret) {
		t.Fatal("签名算法疑似退化成常规 HMAC(key=secret, data=timestamp)")
	}
	if naive == "" {
		t.Fatal("naive 计算结果为空，测试本身失效")
	}
}

func newTestFeishu(t *testing.T, h http.HandlerFunc, secret string, captured *map[string]any) *feishuChannel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				*captured = body
			}
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &feishuChannel{
		name:    "feishu",
		webhook: srv.URL,
		secret:  secret,
		client:  srv.Client(),
		now:     func() time.Time { return time.Unix(1599363739, 0) },
	}
}

func okFeishu(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
}

func TestFeishuSendSuccess(t *testing.T) {
	var body map[string]any
	ch := newTestFeishu(t, okFeishu, "test-secret", &body)
	msg := Message{Event: EventDaily, Title: "t", Card: map[string]any{"elements": []any{}}}
	if err := ch.Send(context.Background(), msg); err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if body["msg_type"] != "interactive" {
		t.Errorf("msg_type 应为 interactive, 实得 %v", body["msg_type"])
	}
	if _, ok := body["card"]; !ok {
		t.Error("载荷缺少 card")
	}
	if body["timestamp"] != "1599363739" {
		t.Errorf("timestamp 不正确: %v", body["timestamp"])
	}
	if body["sign"] != feishuSign("1599363739", "test-secret") {
		t.Errorf("sign 不正确: %v", body["sign"])
	}
}

// TestFeishuSendWithoutSecret 确认未配密钥时不带签名字段（飞书会拒绝多余签名）。
func TestFeishuSendWithoutSecret(t *testing.T) {
	var body map[string]any
	ch := newTestFeishu(t, okFeishu, "", &body)
	if err := ch.Send(context.Background(), Message{Event: EventDaily}); err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if _, ok := body["sign"]; ok {
		t.Error("未配置密钥时不应带 sign")
	}
	if _, ok := body["timestamp"]; ok {
		t.Error("未配置密钥时不应带 timestamp")
	}
}

// TestFeishuSendRejects 覆盖各种"看起来成功其实失败"的响应。
func TestFeishuSendRejects(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"业务错误码", 200, `{"code":19021,"msg":"sign match fail"}`, "19021"},
		{"旧版错误码字段", 200, `{"StatusCode":9499,"StatusMessage":"Bad Request"}`, "9499"},
		{"响应缺状态码", 200, `{"msg":"whatever"}`, "缺少状态码"},
		{"HTTP 200 但是 HTML", 200, `<html><body>502 Bad Gateway</body></html>`, "不是合法 JSON"},
		{"HTTP 500 带业务码", 500, `{"code":19001,"msg":"system busy"}`, "19001"},
		{"HTTP 500 无结构", 500, `oops`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := newTestFeishu(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}, "", nil)
			err := ch.Send(context.Background(), Message{Event: EventDaily})
			if err == nil {
				t.Fatal("必须判为失败")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误信息应含 %q, 实得 %q", c.wantSub, err.Error())
			}
		})
	}
}

// TestFeishuErrorDoesNotLeakCredential 确认错误信息里不出现 hook token。
//
// 这是最容易被忽略的一处：net/http 的 *url.Error 会带上完整 URL。
func TestFeishuErrorDoesNotLeakCredential(t *testing.T) {
	const token = "SECRET-TOKEN-abcdef123456"
	ch := &feishuChannel{
		name:    "feishu",
		webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/" + token,
		client:  &http.Client{Timeout: 200 * time.Millisecond},
		now:     time.Now,
	}
	err := ch.Send(context.Background(), Message{Event: EventDaily})
	if err == nil {
		t.Skip("本环境下请求意外成功，跳过")
	}
	msg := SafeErr(err)
	if strings.Contains(msg, token) {
		t.Fatalf("凭据泄漏到错误信息里: %s", msg)
	}
	// 原始 err.Error() 也必须被脱敏后才可记录——这里验证 SafeErr 是唯一出口。
	if strings.Contains(err.Error(), token) {
		t.Log("提示: 原始 err 含凭据属预期（*url.Error），调用方必须经 SafeErr")
	}
}
