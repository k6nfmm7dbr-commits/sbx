package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

// 渠道类型白名单。当前只支持飞书群机器人。
//
// 刻意只留一个渠道：多渠道路由、渠道级熔断、事件订阅三件事加起来占了大半代码量，
// 而实际使用中几乎所有人都只配一个目标。少一个渠道不是少了功能，
// 是少了"配错了却以为在收"的可能。
const ChannelFeishu = "feishu"

const (
	defaultNotifyDailyAt        = "00:05"
	defaultNotifyTimeoutSeconds = 8
	// maxNotifySecretLen 只是防呆上限：飞书签名密钥是用户自设的字符串。
	maxNotifySecretLen = 256
)

// 飞书群机器人 webhook 的固定形态。
//
// 强制 host 是安全要求而非风格偏好：webhook 里带着可发送消息的凭据，
// 一旦配成任意地址，一个配置错误就会把凭据送到第三方。
const (
	feishuWebhookHost   = "open.feishu.cn"
	feishuWebhookPrefix = "/open-apis/bot/v2/hook/"
)

// NotifyConfig 是 panel.json 的 notify 段。
//
// 与 secure_cookie / trusted_origin 一样不进 defaultConf()：不配置就完全关闭，
// 不启动 goroutine、不新建文件。整段校验放在 LoadStrict 阶段 fail-closed——
// "配置了却没生效"会让人误以为日报还在，这比"配置错误直接拒绝启动"危险得多。
type NotifyConfig struct {
	// Enabled 关闭时保留其余字段，允许用户暂存半配置状态。
	Enabled bool `json:"enabled"`
	// Type 目前只能是 "feishu"。
	Type string `json:"type"`
	// Webhook 是飞书群机器人的完整 webhook 地址。
	Webhook string `json:"webhook"`
	// Secret 可选。填了则对每条消息做 HMAC 签名，防止 webhook 泄露后被乱发。
	Secret string `json:"secret,omitempty"`
	// DailyAt 是每日日报的发送时刻（24 小时制 HH:MM，面板时区）。
	DailyAt string `json:"daily_at"`
	// TimeoutSeconds 是单次 HTTP 请求超时。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

var reDailyAt = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// legacyNotifyKeys 是已废弃的多渠道配置特征键。
//
// 判据取 channels（旧格式的必填字段）而不是 interval_minutes：后者可能与用户自定义键
// 撞名，而 channels 在旧格式里是 enabled=true 时的必填项，撞名概率极低。
var legacyNotifyKeys = []string{"channels"}

// parseNotify 从 raw 配置里解析 notify 段。(nil, nil) 表示未配置（等同于关闭）。
//
// raw 里的值来自 json.Number 解码，这里用紧凑 JSON 往返一次以复用结构体标签。
// 若值是字符串（例如用 `config-set notify '...'` 误写成字符串），往返后无法解码
// 到结构体，会在此明确报错，而不是被静默忽略。
func parseNotify(v any) (*NotifyConfig, error) {
	if v == nil {
		return nil, nil
	}
	// 旧版多渠道格式：识别后按"未配置"处理并告警，而不是抛错。
	//
	// 这里刻意偏离 fail-closed 原则，理由是升级路径：旧格式的用户若因配置格式变更
	// 而无法启动面板，连采集都一并停摆——代价远大于"暂时没有日报"。
	// 关键是告警必须够响：Error 级别 + 明确动作指引。
	if m, ok := v.(map[string]any); ok {
		for _, k := range legacyNotifyKeys {
			if _, found := m[k]; found {
				slog.Error("检测到已废弃的旧版推送配置, 已按『未启用』处理",
					"key", k,
					"action", "请在管理菜单「系统设置与运维 → 流量推送配置」重新配置")
				return nil, nil
			}
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化失败: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var nc NotifyConfig
	if err := dec.Decode(&nc); err != nil {
		return nil, fmt.Errorf("必须是对象(键 enabled/type/webhook/secret/daily_at): %w", err)
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
	n.Type = strings.ToLower(strings.TrimSpace(n.Type))
	if n.Type == "" {
		n.Type = ChannelFeishu
	}
	if strings.TrimSpace(n.DailyAt) == "" {
		n.DailyAt = defaultNotifyDailyAt
	}
	n.DailyAt = strings.TrimSpace(n.DailyAt)
	if n.TimeoutSeconds == 0 {
		n.TimeoutSeconds = defaultNotifyTimeoutSeconds
	}
	n.Webhook = strings.TrimSpace(n.Webhook)
	// 密钥只去首尾空白：中间的空格可能是密钥的一部分，不该替用户改写。
	n.Secret = strings.Trim(n.Secret, "\r\n")
}

// Validate 逐条校验 notify 段。任何一条不满足都返回错误 —— 由 LoadStrict 上抛，
// 拒绝启动。enabled=false 时只校验全局字段，允许保留半配置状态。
func (n *NotifyConfig) Validate() error {
	if !reDailyAt.MatchString(n.DailyAt) {
		return fmt.Errorf("notify.daily_at 必须是 24 小时制 HH:MM: %q", n.DailyAt)
	}
	if n.TimeoutSeconds < 3 || n.TimeoutSeconds > 60 {
		return fmt.Errorf("notify.timeout_seconds 必须在 3-60: %d", n.TimeoutSeconds)
	}
	if !n.Enabled {
		return nil
	}
	if n.Type != ChannelFeishu {
		return fmt.Errorf("notify.type 目前只支持 %q: %q", ChannelFeishu, n.Type)
	}
	if err := validateFeishuWebhook(n.Webhook); err != nil {
		return err
	}
	if n.Secret != "" {
		if len(n.Secret) > maxNotifySecretLen {
			return fmt.Errorf("notify.secret 过长（最多 %d 字节）", maxNotifySecretLen)
		}
		for _, r := range n.Secret {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("notify.secret 含控制字符")
			}
		}
	}
	return nil
}

// validateFeishuWebhook 校验 webhook 的形态。
//
// 必须同时满足 https + 固定 host + 固定路径前缀：只校验"是合法 URL"是不够的，
// 那样任何地址都能通过，凭据就有被送到第三方的机会。
func validateFeishuWebhook(raw string) error {
	if raw == "" {
		return fmt.Errorf("notify.webhook 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("notify.webhook 无法解析: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("notify.webhook 必须是 https")
	}
	if !strings.EqualFold(u.Hostname(), feishuWebhookHost) {
		return fmt.Errorf("notify.webhook 主机必须是 %s（实得 %s）", feishuWebhookHost, u.Hostname())
	}
	if !strings.HasPrefix(u.Path, feishuWebhookPrefix) {
		return fmt.Errorf("notify.webhook 路径必须以 %s 开头", feishuWebhookPrefix)
	}
	if strings.TrimPrefix(u.Path, feishuWebhookPrefix) == "" {
		return fmt.Errorf("notify.webhook 缺少 hook token")
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
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("必须是 JSON 对象")
	}
	nc, err := parseNotify(v)
	if err != nil {
		return nil, err
	}
	if nc == nil {
		return nil, fmt.Errorf("notify 配置不能为空对象（旧版 channels 格式已废弃，请按单渠道格式重写）")
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
