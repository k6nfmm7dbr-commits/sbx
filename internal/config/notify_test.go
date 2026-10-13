package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const validFeishuHook = `https://open.feishu.cn/open-apis/bot/v2/hook/1111aaaa-2222-bbbb-3333-cccc4444dddd`

// TestParseNotifyJSONAccepts 覆盖合法配置的多种写法。
func TestParseNotifyJSONAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"最小配置", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","daily_at":"00:05"}`},
		{"带签名密钥", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","secret":"s3cr3t","daily_at":"09:00"}`},
		{"省略 type 时默认 feishu", `{"enabled":true,"webhook":"` + validFeishuHook + `","daily_at":"23:59"}`},
		{"未启用时可留半配置", `{"enabled":false,"type":"feishu","daily_at":"00:05"}`},
		{"空对象即显式关闭", `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseNotifyJSON([]byte(c.in)); err != nil {
				t.Fatalf("应通过，实得错误: %v", err)
			}
		})
	}
}

// TestParseNotifyJSONRejects 逐条覆盖 fail-closed 规则。
func TestParseNotifyJSONRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"非对象", `"just a string"`},
		{"类型不支持", `{"enabled":true,"type":"telegram","webhook":"` + validFeishuHook + `","daily_at":"00:05"}`},
		{"webhook 为空", `{"enabled":true,"type":"feishu","webhook":"","daily_at":"00:05"}`},
		{"webhook 用 http", `{"enabled":true,"type":"feishu","webhook":"http://open.feishu.cn/open-apis/bot/v2/hook/x","daily_at":"00:05"}`},
		{"webhook 主机错误", `{"enabled":true,"type":"feishu","webhook":"https://evil.example.com/open-apis/bot/v2/hook/x","daily_at":"00:05"}`},
		{"webhook 路径错误", `{"enabled":true,"type":"feishu","webhook":"https://open.feishu.cn/other/hook/x","daily_at":"00:05"}`},
		{"webhook 缺 token", `{"enabled":true,"type":"feishu","webhook":"https://open.feishu.cn/open-apis/bot/v2/hook/","daily_at":"00:05"}`},
		{"daily_at 非法", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","daily_at":"24:00"}`},
		{"daily_at 非时刻", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","daily_at":"noon"}`},
		{"timeout 越界", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","daily_at":"00:05","timeout_seconds":120}`},
		{"secret 含控制字符", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","daily_at":"00:05","secret":"a\u0007b"}`},
		{"secret 过长", `{"enabled":true,"type":"feishu","webhook":"` + validFeishuHook + `","daily_at":"00:05","secret":"` + strings.Repeat("x", 300) + `"}`},
		{"尾部有多余内容", `{"enabled":false} {"x":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if nc, err := ParseNotifyJSON([]byte(c.in)); err == nil {
				t.Fatalf("应被拒绝，却通过了: %+v", nc)
			}
		})
	}
}

// TestNotifyDefaultsApplied 确认省略字段取默认值，而不是留成零值。
func TestNotifyDefaultsApplied(t *testing.T) {
	nc, err := ParseNotifyJSON([]byte(`{"enabled":true,"webhook":"` + validFeishuHook + `","daily_at":"00:05"}`))
	if err != nil {
		t.Fatal(err)
	}
	if nc.Type != ChannelFeishu {
		t.Errorf("省略 type 应默认为 %q, 实得 %q", ChannelFeishu, nc.Type)
	}
	if nc.TimeoutSeconds != defaultNotifyTimeoutSeconds {
		t.Errorf("默认超时应为 %d, 实得 %d", defaultNotifyTimeoutSeconds, nc.TimeoutSeconds)
	}
	nc2, err := ParseNotifyJSON([]byte(`{"enabled":true,"webhook":"` + validFeishuHook + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if nc2.DailyAt != defaultNotifyDailyAt {
		t.Errorf("默认日报时刻应为 %s, 实得 %s", defaultNotifyDailyAt, nc2.DailyAt)
	}
}

// TestNotifyLegacyFormatTreatedAsUnconfigured 确认旧版多渠道配置不会导致启动失败。
//
// 这是刻意的例外：配置格式变更若让面板整体启动不了，代价远大于"暂时没有日报"。
// 但必须降级为"未配置"（而不是半读半用），否则会出现难以理解的推送行为。
func TestNotifyLegacyFormatTreatedAsUnconfigured(t *testing.T) {
	legacy := []string{
		`{"enabled":true,"interval_minutes":30,"channels":[{"type":"telegram","bot_token":"1:AAAA"}]}`,
		`{"enabled":true,"channels":[]}`,
		`{"enabled":false,"channels":[{"type":"webhook","url":"https://e.com/h"}]}`,
	}
	for _, in := range legacy {
		v := decodeAny(t, in)
		nc, err := parseNotify(v)
		if err != nil {
			t.Fatalf("旧格式不应报错（应降级为未配置）: %v", err)
		}
		if nc != nil {
			t.Fatalf("旧格式必须按未配置处理, 实得 %+v", nc)
		}
	}
}

// TestNotifyViaLoadStrictFailClosed 确认非法配置会经 LoadStrict 上抛，拒绝启动。
func TestNotifyViaLoadStrictFailClosed(t *testing.T) {
	base := `{"listen":"127.0.0.1","port":9999,"token":"t","nft_conf":"/etc/sbx/nft.conf"`

	// 未配置：正常
	path := strictEnv(t)
	mustWrite(t, path, base+`}`)
	if _, err := LoadStrict(); err != nil {
		t.Fatalf("无 notify 段不应报错: %v", err)
	}

	// 非法 notify：必须上抛
	path = strictEnv(t)
	mustWrite(t, path, base+`,"notify":{"enabled":true,"type":"feishu",`+
		`"webhook":"https://evil.example.com/open-apis/bot/v2/hook/x","daily_at":"00:05"}}`)
	if _, err := LoadStrict(); err == nil {
		t.Error("非法 webhook 必须导致 LoadStrict 报错（fail-closed）")
	}

	// 合法 notify：正常，且被解析为已启用
	path = strictEnv(t)
	mustWrite(t, path, base+`,"notify":{"enabled":true,"type":"feishu","webhook":"`+validFeishuHook+`","daily_at":"00:05"}}`)
	c, err := LoadStrict()
	if err != nil {
		t.Fatalf("合法 notify 不应报错: %v", err)
	}
	if c.Notify == nil || !c.Notify.Enabled {
		t.Fatal("notify 应被解析为已启用")
	}
}

func decodeAny(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("测试输入不是合法 JSON: %v", err)
	}
	return v
}
