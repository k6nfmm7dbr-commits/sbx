package config

import (
	"errors"
	"strings"
	"testing"
)

const okBotToken = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// TestParseNotifyJSONAccepts 覆盖五种渠道的合法配置。
func TestParseNotifyJSONAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"未启用", `{"enabled":false}`},
		{"最小可用", `{"enabled":true,"channels":[{"type":"telegram","bot_token":"` + okBotToken + `","chat_id":"-1001234567890"}]}`},
		{"企业微信", `{"enabled":true,"channels":[{"type":"wxwork","webhook":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcd-1234"}]}`},
		{"企业微信markdown", `{"enabled":true,"channels":[{"type":"wxwork","webhook":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcd","msgtype":"markdown"}]}`},
		{"Server酱", `{"enabled":true,"channels":[{"type":"serverchan","sendkey":"SCTabc123"}]}`},
		{"PushPlus", `{"enabled":true,"channels":[{"type":"pushplus","sendkey":"abcdef1234567890abcdef1234567890"}]}`},
		{"通用webhook", `{"enabled":true,"channels":[{"type":"webhook","url":"https://example.com/hook","secret":"s3cret"}]}`},
		{"自定义间隔与日结时刻", `{"enabled":true,"interval_minutes":5,"daily_at":"00:01","timeout_seconds":15,"channels":[{"type":"webhook","url":"http://127.0.0.1:9000/hook"}]}`},
		{"带心跳地址", `{"enabled":true,"heartbeat_url":"https://hc-ping.com/uuid","channels":[{"type":"webhook","url":"https://e.com/h"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nc, err := ParseNotifyJSON([]byte(c.in))
			if err != nil {
				t.Fatalf("应接受该配置: %v", err)
			}
			if nc == nil {
				t.Fatal("不应返回 nil")
			}
			if nc.IntervalMinutes < 1 || nc.DailyAt == "" || nc.TimeoutSeconds < 3 {
				t.Errorf("默认值未补齐: %+v", nc)
			}
			for i := range nc.Channels {
				if len(nc.Channels[i].Events) == 0 {
					t.Errorf("渠道 %d 的 events 应有默认值", i)
				}
				if nc.Channels[i].Name == "" {
					t.Errorf("渠道 %d 应有默认 name", i)
				}
			}
		})
	}
}

// TestParseNotifyJSONRejects 逐条覆盖 fail-closed 规则。
// 这些规则的意义在于"配了却没生效"比"配置错误"更难排查。
func TestParseNotifyJSONRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"启用但无渠道", `{"enabled":true,"channels":[]}`},
		{"间隔过大", `{"enabled":true,"interval_minutes":1441,"channels":[{"type":"webhook","url":"https://e.com/h"}]}`},
		{"日结时刻非法", `{"enabled":true,"daily_at":"25:00","channels":[{"type":"webhook","url":"https://e.com/h"}]}`},
		{"日结时刻格式错", `{"enabled":true,"daily_at":"1:5","channels":[{"type":"webhook","url":"https://e.com/h"}]}`},
		{"超时过短", `{"enabled":true,"timeout_seconds":1,"channels":[{"type":"webhook","url":"https://e.com/h"}]}`},
		{"心跳地址非http", `{"enabled":true,"heartbeat_url":"ftp://x/y","channels":[{"type":"webhook","url":"https://e.com/h"}]}`},
		{"未知渠道类型", `{"enabled":true,"channels":[{"type":"slack"}]}`},
		{"telegram缺token", `{"enabled":true,"channels":[{"type":"telegram","bot_token":"nope","chat_id":"1"}]}`},
		{"telegram缺chatid", `{"enabled":true,"channels":[{"type":"telegram","bot_token":"` + okBotToken + `","chat_id":""}]}`},
		{"企业微信非白名单域名", `{"enabled":true,"channels":[{"type":"wxwork","webhook":"https://evil.example.com/hook?key=abcd"}]}`},
		{"企业微信非https", `{"enabled":true,"channels":[{"type":"wxwork","webhook":"http://qyapi.weixin.qq.com/x?key=abcd"}]}`},
		{"企业微信缺key", `{"enabled":true,"channels":[{"type":"wxwork","webhook":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send"}]}`},
		{"企业微信msgtype非法", `{"enabled":true,"channels":[{"type":"wxwork","webhook":"https://qyapi.weixin.qq.com/x?key=a","msgtype":"card"}]}`},
		{"Server酱sendkey格式错", `{"enabled":true,"channels":[{"type":"serverchan","sendkey":"abc"}]}`},
		{"PushPlus token过短", `{"enabled":true,"channels":[{"type":"pushplus","sendkey":"short"}]}`},
		{"webhook非http", `{"enabled":true,"channels":[{"type":"webhook","url":"ftp://example.com/x"}]}`},
		{"webhook缺host", `{"enabled":true,"channels":[{"type":"webhook","url":"https://"}]}`},
		{"webhook max_bytes过小", `{"enabled":true,"channels":[{"type":"webhook","url":"https://e.com/h","max_bytes":10}]}`},
		{"events含未知值", `{"enabled":true,"channels":[{"type":"webhook","url":"https://e.com/h","events":["hourly"]}]}`},
		{"顶层不是对象", `"just a string"`},
		{"非法JSON", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if nc, err := ParseNotifyJSON([]byte(c.in)); err == nil {
				t.Fatalf("应拒绝该配置，实得 %+v", nc)
			}
		})
	}
}

func TestNotifyEventsDefaultAndFilter(t *testing.T) {
	nc, err := ParseNotifyJSON([]byte(`{"enabled":true,"channels":[{"type":"webhook","url":"https://e.com/h"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ch := &nc.Channels[0]
	if !ch.HasEvent(EventHeartbeat) || !ch.HasEvent(EventDaily) {
		t.Error("未声明 events 时应订阅全部事件")
	}
	if ch.HasEvent("weekly") {
		t.Error("不应出现未声明的事件")
	}

	nc2, err := ParseNotifyJSON([]byte(`{"enabled":true,"channels":[{"type":"webhook","url":"https://e.com/h","events":["daily"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ch2 := &nc2.Channels[0]
	if ch2.HasEvent(EventHeartbeat) {
		t.Error("显式限定 events 时应严格按声明过滤")
	}
	if !ch2.HasEvent(EventDaily) {
		t.Error("声明的 daily 应生效")
	}
}

// TestDisabledSkipsChannelValidation：允许保留半配置状态，
// 这样用户可先禁用再慢慢调整渠道，而不必删掉整段配置。
func TestDisabledSkipsChannelValidation(t *testing.T) {
	nc, err := ParseNotifyJSON([]byte(`{"enabled":false,"channels":[{"type":"telegram","bot_token":"bad","chat_id":""}]}`))
	if err != nil {
		t.Fatalf("未启用时不应校验渠道: %v", err)
	}
	if nc == nil {
		t.Fatal("应返回解析结果")
	}
}

func TestNotifyDefaultsApplied(t *testing.T) {
	nc, err := ParseNotifyJSON([]byte(`{"enabled":true,"channels":[{"type":"webhook","url":"https://e.com/h"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if nc.IntervalMinutes != defaultNotifyIntervalMinutes {
		t.Errorf("默认间隔应为 %d, 实得 %d", defaultNotifyIntervalMinutes, nc.IntervalMinutes)
	}
	if nc.DailyAt != defaultNotifyDailyAt {
		t.Errorf("默认日结时刻应为 %s, 实得 %s", defaultNotifyDailyAt, nc.DailyAt)
	}
	if nc.TimeoutSeconds != defaultNotifyTimeoutSeconds {
		t.Errorf("默认超时应为 %d, 实得 %d", defaultNotifyTimeoutSeconds, nc.TimeoutSeconds)
	}
	if nc.Channels[0].MaxBytes != defaultWebhookMaxBytes {
		t.Errorf("webhook 默认 max_bytes 应为 %d, 实得 %d", defaultWebhookMaxBytes, nc.Channels[0].MaxBytes)
	}
	// 渠道名默认形如 webhook#1
	if !strings.HasPrefix(nc.Channels[0].Name, "webhook") {
		t.Errorf("默认渠道名不符合预期: %q", nc.Channels[0].Name)
	}
}

// TestConfigValidateSurfacesNotifyErrors 确认 notify 的错误会经 LoadStrict 上抛，
// 而不是被静默忽略。
func TestConfigValidateSurfacesNotifyErrors(t *testing.T) {
	c := &Config{raw: defaultConf()}
	c.normalize()
	c.Notify = nil
	c.notifyErr = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("无 notify 配置时不应报错: %v", err)
	}

	c2 := &Config{raw: defaultConf()}
	c2.normalize()
	c2.notifyErr = errors.New("fake")
	if err := c2.Validate(); err == nil {
		t.Error("notify 解析错误必须上抛（LoadStrict fail-closed）")
	}
}
