package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

// 事件类型。推送侧与恢复侧共用同一套字面量，避免两处硬编码字符串漂移。
const (
	EventHeartbeat = "heartbeat"
	EventDaily     = "daily"
)

// 渠道类型白名单。
const (
	ChannelTelegram   = "telegram"
	ChannelWxWork     = "wxwork"
	ChannelServerChan = "serverchan"
	ChannelPushPlus   = "pushplus"
	ChannelWebhook    = "webhook"
)

// 主机的默认值。与 secure_cookie / trusted_origin 一样，notify 不进 defaultConf()：
// 不配置就完全关闭（不启动 goroutine、不新建文件）。
const (
	defaultNotifyIntervalMinutes = 30
	defaultNotifyDailyAt         = "00:05"
	defaultNotifyTimeoutSeconds  = 8
	defaultWebhookMaxBytes       = 8192
)

// NotifyConfig 是 panel.json 的 notify 段。
//
// 为什么整段校验放在 LoadStrict 阶段 fail-closed：推送是"异地留痕"的唯一手段，
// "配置了却没生效"会让人误以为已有备份——这比"配置错误直接拒绝启动"危险得多。
type NotifyConfig struct {
	Enabled         bool            `json:"enabled"`
	IntervalMinutes int             `json:"interval_minutes"`
	DailyAt         string          `json:"daily_at"`
	TimeoutSeconds  int             `json:"timeout_seconds"`
	HeartbeatURL    string          `json:"heartbeat_url"`
	Channels        []NotifyChannel `json:"channels"`
}

// NotifyChannel 是一个推送目标。各 type 的专属字段共用同一结构体（panel.json 里
// 它们本就平铺在同一个对象里），校验与使用时按 Type 取对应字段。
type NotifyChannel struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Events []string `json:"events"`

	// telegram
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	Silent   bool   `json:"disable_notification"`

	// wxwork
	Webhook string `json:"webhook"`
	MsgType string `json:"msgtype"`

	// serverchan
	SendKey string `json:"sendkey"`

	// webhook
	URL      string `json:"url"`
	Secret   string `json:"secret"`
	MaxBytes int    `json:"max_bytes"`
}

var validChannelTypes = map[string]bool{
	ChannelTelegram:   true,
	ChannelWxWork:     true,
	ChannelServerChan: true,
	ChannelPushPlus:   true,
	ChannelWebhook:    true,
}

var validEvents = map[string]bool{EventHeartbeat: true, EventDaily: true}

var (
	reDailyAt       = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	reBotToken      = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{20,}$`)
	reSendKey       = regexp.MustCompile(`^SCT[A-Za-z0-9]+$`)
	rePushPlusToken = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
)

// HasEvent 判断该渠道是否订阅了某事件。
func (ch *NotifyChannel) HasEvent(ev string) bool {
	for _, e := range ch.Events {
		if e == ev {
			return true
		}
	}
	return false
}

// parseNotify 从 raw 配置里解析 notify 段。(nil, nil) 表示未配置（等同于关闭）。
//
// raw 里的值来自 json.Number 解码，这里用紧凑 JSON 往返一次以复用结构体标签。
// 若值是字符串（例如用 `config-set notify '...'` 误写成字符串），往返后无法解码
// 到结构体，会在此明确报错，而不是被静默忽略。
func parseNotify(v any) (*NotifyConfig, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化失败: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var nc NotifyConfig
	if err := dec.Decode(&nc); err != nil {
		return nil, fmt.Errorf("必须是对象(键 enabled/interval_minutes/daily_at/channels 等): %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("尾部存在非法数据")
	}
	nc.applyDefaults()
	return &nc, nil
}

// applyDefaults 补齐可选键并规范化枚举值。
// 语义：0 / 空串视为"未配置"→ 取默认值，因此不会因为"配了 0"而触发范围校验失败。
func (n *NotifyConfig) applyDefaults() {
	if n.IntervalMinutes == 0 {
		n.IntervalMinutes = defaultNotifyIntervalMinutes
	}
	if strings.TrimSpace(n.DailyAt) == "" {
		n.DailyAt = defaultNotifyDailyAt
	}
	if n.TimeoutSeconds == 0 {
		n.TimeoutSeconds = defaultNotifyTimeoutSeconds
	}
	n.DailyAt = strings.TrimSpace(n.DailyAt)
	n.HeartbeatURL = strings.TrimSpace(n.HeartbeatURL)
	for i := range n.Channels {
		ch := &n.Channels[i]
		ch.Type = strings.ToLower(strings.TrimSpace(ch.Type))
		if len(ch.Events) == 0 {
			// 未声明 = 订阅全部事件。用非 nil 切片，让 HasEvent 与序列化都稳定。
			ch.Events = []string{EventHeartbeat, EventDaily}
		}
		if ch.Type == ChannelWxWork && strings.TrimSpace(ch.MsgType) == "" {
			// 默认 text：2048 字节上限，但零 markdown 解析风险（数据行不被转义破坏）。
			ch.MsgType = "text"
		}
		if ch.Type == ChannelWebhook && ch.MaxBytes == 0 {
			ch.MaxBytes = defaultWebhookMaxBytes
		}
		if strings.TrimSpace(ch.Name) == "" {
			ch.Name = fmt.Sprintf("%s#%d", ch.Type, i+1)
		}
	}
}

// Validate 逐条校验 notify 段。任何一条不满足都返回错误 —— 由 LoadStrict 上抛，
// 拒绝启动。enabled=false 时只校验全局字段，允许保留半配置状态。
func (n *NotifyConfig) Validate() error {
	if n.IntervalMinutes < 1 || n.IntervalMinutes > 1440 {
		return fmt.Errorf("notify.interval_minutes 必须在 1-1440: %d", n.IntervalMinutes)
	}
	if !reDailyAt.MatchString(n.DailyAt) {
		return fmt.Errorf("notify.daily_at 必须是 24 小时制 HH:MM: %q", n.DailyAt)
	}
	if n.TimeoutSeconds < 3 || n.TimeoutSeconds > 60 {
		return fmt.Errorf("notify.timeout_seconds 必须在 3-60: %d", n.TimeoutSeconds)
	}
	if n.HeartbeatURL != "" {
		if err := validateHTTPURL("notify.heartbeat_url", n.HeartbeatURL); err != nil {
			return err
		}
	}
	if !n.Enabled {
		return nil
	}
	if len(n.Channels) == 0 {
		return fmt.Errorf("notify.enabled=true 但 channels 为空——至少配置一个推送目标")
	}
	for i := range n.Channels {
		if err := n.Channels[i].validate(i); err != nil {
			return err
		}
	}
	return nil
}

func (ch *NotifyChannel) validate(i int) error {
	where := fmt.Sprintf("notify.channels[%d]", i)
	if !validChannelTypes[ch.Type] {
		return fmt.Errorf("%s: 未知渠道类型 %q（可选 telegram/wxwork/serverchan/webhook）", where, ch.Type)
	}
	where = fmt.Sprintf("notify.channels[%d](%s)", i, ch.Type)
	for _, e := range ch.Events {
		if !validEvents[e] {
			return fmt.Errorf("%s: events 含未知事件 %q（可选 heartbeat/daily）", where, e)
		}
	}
	switch ch.Type {
	case ChannelTelegram:
		if !reBotToken.MatchString(ch.BotToken) {
			return fmt.Errorf("%s: bot_token 格式非法（应形如 123456789:AAG...）", where)
		}
		if strings.TrimSpace(ch.ChatID) == "" {
			return fmt.Errorf("%s: chat_id 不能为空", where)
		}
	case ChannelWxWork:
		// 强制域名：webhook key 是明文凭据且出现在 URL 里，
		// 限定 host 可杜绝"误配把 key 发到第三方"。
		u, err := url.Parse(ch.Webhook)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "qyapi.weixin.qq.com") {
			return fmt.Errorf("%s: webhook 必须是 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=... ", where)
		}
		if u.Query().Get("key") == "" {
			return fmt.Errorf("%s: webhook 缺少 key 参数", where)
		}
		switch ch.MsgType {
		case "text", "markdown":
		default:
			return fmt.Errorf("%s: msgtype 只能是 text 或 markdown: %q", where, ch.MsgType)
		}
	case ChannelServerChan:
		if !reSendKey.MatchString(ch.SendKey) {
			return fmt.Errorf("%s: sendkey 格式非法（应形如 SCTxxxx）", where)
		}
	case ChannelPushPlus:
		// PushPlus 的 token 是一个普通密钥串（无固定前缀），只做长度与字符集约束。
		if !rePushPlusToken.MatchString(ch.SendKey) {
			return fmt.Errorf("%s: sendkey 格式非法（应为 PushPlus 的 32 位 token）", where)
		}
	case ChannelWebhook:
		if err := validateHTTPURL(where+".url", ch.URL); err != nil {
			return err
		}
		if ch.MaxBytes < 256 || ch.MaxBytes > 1<<20 {
			return fmt.Errorf("%s: max_bytes 必须在 256-1048576: %d", where, ch.MaxBytes)
		}
	}
	return nil
}

func validateHTTPURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s 无法解析: %w", key, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s 必须是 http/https 绝对 URL: %q", key, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s 缺少主机名: %q", key, raw)
	}
	return nil
}

// ParseNotifyJSON 解析并校验一段 notify 配置 JSON（用于 CLI 写入前的预检）。
// 传入的对象必须是 JSON 对象。
func ParseNotifyJSON(data []byte) (*NotifyConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("不是合法 JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("尾部存在多余内容")
	}
	nc, err := parseNotify(v)
	if err != nil {
		return nil, err
	}
	if nc == nil {
		return nil, fmt.Errorf("必须是 JSON 对象")
	}
	if err := nc.Validate(); err != nil {
		return nil, err
	}
	return nc, nil
}

// SetNotify 把 notify 段作为 JSON 对象写入 panel.json。
//
// 刻意不复用 Set(key, value)：Set 会把无法解析为整数的值一律存成**字符串**，
// 而 notify 必须是嵌套对象。这里走与 Set/EnsureToken 相同的
// 「LoadStrict → 合并 → fsx.WriteFileAtomic(0600)」，保留其它键（含未知自定义键）。
//
// 写入前先做完整校验：非法配置绝不落盘，否则会把能正常启动的实例改成启动失败。
func SetNotify(data []byte) error {
	nc, err := ParseNotifyJSON(data)
	if err != nil {
		return fmt.Errorf("拒绝写入非法 notify 配置: %w", err)
	}
	c, err := LoadStrict()
	if err != nil {
		return fmt.Errorf("拒绝修改配置: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("notify 配置 JSON 解析失败: %w", err)
	}
	c.raw["notify"] = v
	c.Notify = nc
	c.notifyErr = nil
	out, err := fsx.MarshalIndent(c.raw)
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return fsx.WriteFileAtomic(ConfPath(), out, 0o600)
}
