package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

// 事件标识。与卡片标题复用同一套字面量，避免两处硬编码字符串漂移。
const (
	EventDaily   = "daily"
	EventRecover = "recover"
	EventTest    = "test"
)

// feishuChannel 是飞书群机器人。
type feishuChannel struct {
	name    string
	webhook string
	secret  string
	client  *http.Client
	// now 可注入，用于稳定地测试签名的时间戳。
	now func() time.Time
}

func (f *feishuChannel) Name() string { return f.name }

// feishuResp 对齐飞书机器人的响应体。
//
// 用指针而不是 int：缺字段时 Code 会默认成 0，也就是"成功"——那会让一个
// 完全没有状态码的响应（例如网关返回的 HTML 错误页）被当成投递成功。
// 拿不到明确的成功码就必须判失败。
type feishuResp struct {
	Code          *int   `json:"code"`
	Msg           string `json:"msg"`
	StatusCode    *int   `json:"StatusCode"`
	StatusMessage string `json:"StatusMessage"`
}

func (f *feishuChannel) Send(ctx context.Context, msg Message) error {
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     msg.Card,
	}
	if f.secret != "" {
		ts := strconv.FormatInt(f.now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, f.secret)
	}
	data, err := postJSON(ctx, f.client, f.webhook, payload)
	if err != nil {
		// 非 2xx 时响应体里可能仍带业务错误码，优先用它给出可读原因。
		if len(data) > 0 {
			if rerr := parseFeishuResp(data); rerr != nil {
				return rerr
			}
		}
		return err
	}
	return parseFeishuResp(data)
}

// feishuSign 计算飞书自定义机器人的签名。
//
// 注意这个算法与常规 HMAC 的写法**相反**：密钥是 timestamp+"\n"+secret，待签名数据是**空**。
// 写成 hmac(key=secret, data=timestamp...) 不会报错，只会一直返回签名校验失败，
// 是这套 API 最费时间的坑。feishu_test.go 里有针对它的固定向量断言。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	h := hmac.New(sha256.New, []byte(stringToSign))
	h.Write(nil)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// parseFeishuResp 判定响应体的业务语义。
func parseFeishuResp(data []byte) error {
	var r feishuResp
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %s", truncate(redactBare(string(data)), 120))
	}
	switch {
	case r.Code != nil:
		if *r.Code != 0 {
			return fmt.Errorf("飞书拒绝接收 (code=%d): %s", *r.Code, truncate(r.Msg, 120))
		}
		return nil
	case r.StatusCode != nil:
		if *r.StatusCode != 0 {
			return fmt.Errorf("飞书拒绝接收 (StatusCode=%d): %s", *r.StatusCode, truncate(r.StatusMessage, 120))
		}
		return nil
	}
	return fmt.Errorf("响应缺少状态码, 无法确认投递结果: %s", truncate(redactBare(string(data)), 120))
}

// NewSender 按配置构造推送目标。目前只有飞书一个实现，但保留构造函数是为了
// 让 service 侧不关心具体类型，也便于测试注入替身。
func NewSender(c *config.NotifyConfig, client *http.Client) (Sender, error) {
	if c == nil {
		return nil, fmt.Errorf("notify 配置为空")
	}
	if c.Type != config.ChannelFeishu {
		return nil, fmt.Errorf("未知渠道类型 %q", c.Type)
	}
	return &feishuChannel{
		name:    config.ChannelFeishu,
		webhook: c.Webhook,
		secret:  c.Secret,
		client:  client,
		now:     time.Now,
	}, nil
}
