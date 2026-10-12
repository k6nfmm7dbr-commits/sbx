package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

// maxRespBytes 限制响应体读取长度，避免被超大响应拖垮内存。
const maxRespBytes = 64 << 10

// Message 是一次待发送的内容。
type Message struct {
	// Event 是 config.EventHeartbeat / config.EventDaily。
	Event string
	// Kind 是行内事件短名（notify.KindHeartbeat / KindDaily）。
	Kind string
	Day  string
	HHMM string
	// Title 用作推送服务的标题（Server酱 / PushPlus）。
	Title string
	// Blocks 是已经按渠道容量渲染好的文本块，需逐块发送。
	Blocks []string
	// Snap 是结构化数据（供 webhook 类型的接收方直接消费）。
	Snap *Snapshot
}

// Channel 是一个推送目标。实现必须遵守三条纪律：
//   - 错误里不得出现凭据（返回原始 err 即可，调用方统一经 SafeErr 处理）；
//   - 必须对响应体做语义判定，不能只看 HTTP 状态码；
//   - 响应体限长读取。
type Channel interface {
	Name() string
	HasEvent(ev string) bool
	Caps() Capabilities
	Send(ctx context.Context, msg Message) error
}

// channelBase 提供各渠道共用的名字与事件订阅判断。
type channelBase struct {
	name   string
	events []string
}

func (b channelBase) Name() string { return b.name }

func (b channelBase) HasEvent(ev string) bool {
	// events 为空 = 订阅全部事件。
	//
	// config 的 applyDefaults 已保证正常配置非空，但这里必须再兜一层：
	// 若返回 false，渠道会被静默跳过——"配好了却收不到"是最难排查的一类故障。
	if len(b.events) == 0 {
		return true
	}
	for _, e := range b.events {
		if e == ev {
			return true
		}
	}
	return false
}

// NewChannel 按配置构造渠道实现。
func NewChannel(ch config.NotifyChannel, client *http.Client) (Channel, error) {
	base := channelBase{name: ch.Name, events: ch.Events}
	switch ch.Type {
	case config.ChannelTelegram:
		return &telegramChannel{
			channelBase: base, endpoint: telegramAPIBase, token: ch.BotToken,
			chatID: ch.ChatID, silent: ch.Silent, client: client,
		}, nil
	case config.ChannelWxWork:
		return &wxworkChannel{
			channelBase: base, webhook: ch.Webhook, msgtype: ch.MsgType, client: client,
		}, nil
	case config.ChannelServerChan:
		return &serverChanChannel{
			channelBase: base, endpoint: serverChanEndpoint, sendKey: ch.SendKey, client: client,
		}, nil
	case config.ChannelPushPlus:
		return &pushPlusChannel{
			channelBase: base, endpoint: pushPlusEndpoint, token: ch.SendKey, client: client,
		}, nil
	case config.ChannelWxPusher:
		// 填了 spt 走极简端点；否则走标准端点（校验已保证 app_token/uids 完备）。
		return &wxPusherChannel{
			channelBase: base, baseURL: wxPusherBaseURL,
			simple:   strings.TrimSpace(ch.SPT) != "",
			spt:      ch.SPT,
			appToken: ch.AppToken, uids: ch.UIDs, topicIDs: ch.TopicIDs,
			client: client,
		}, nil
	case config.ChannelWebhook:
		return &webhookChannel{
			channelBase: base, url: ch.URL, secret: ch.Secret,
			maxBytes: ch.MaxBytes, client: client,
		}, nil
	}
	return nil, fmt.Errorf("未知渠道类型 %q", ch.Type)
}

// httpStatusError 表示非 2xx 响应。Body 已截断并脱敏，可安全记录。
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

func postJSON(ctx context.Context, client *http.Client, rawURL string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(client, req)
}

func postForm(ctx context.Context, client *http.Client, rawURL string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doRequest(client, req)
}

// doRequest 执行请求并返回响应体。非 2xx 时同时返回响应体（供调用方读取
// retry_after 等限流信息）与 httpStatusError。
func doRequest(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, &httpStatusError{
			Status: resp.StatusCode,
			Body:   redactBare(truncate(string(data), 200)),
		}
	}
	return data, nil
}

// truncate 按字符（而非字节）截断，避免切碎多字节字符。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
