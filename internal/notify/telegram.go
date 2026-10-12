package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// telegramAPIBase 是固定端点：bot token 直接拼在路径里，
	// 不允许配置 host，避免凭据被改写到第三方。
	telegramAPIBase = "https://api.telegram.org/bot"
	// telegramMaxUTF16 是单条消息的 UTF-16 code unit 上限（Telegram 官方口径）。
	telegramMaxUTF16 = 4096
	// telegramMaxRetryAfter 是接受的 retry_after 上限，超过则放弃本轮重试。
	telegramMaxRetryAfter = 60 * time.Second
)

type telegramChannel struct {
	channelBase
	// endpoint 生产恒为 telegramAPIBase；抽成字段只为让测试能把它指向 httptest。
	endpoint string
	token    string
	chatID   string
	silent   bool
	client   *http.Client
}

func (t *telegramChannel) Caps() Capabilities {
	return Capabilities{MaxUTF16: telegramMaxUTF16}
}

func (t *telegramChannel) Send(ctx context.Context, msg Message) error {
	apiURL := t.endpoint + t.token + "/sendMessage"
	for _, block := range msg.Blocks {
		if err := t.sendBlock(ctx, apiURL, block); err != nil {
			return err
		}
	}
	return nil
}

func (t *telegramChannel) sendBlock(ctx context.Context, apiURL, text string) error {
	payload := map[string]any{
		"chat_id":                  t.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if t.silent {
		payload["disable_notification"] = true
	}

	data, err := postJSON(ctx, t.client, apiURL, payload)
	var se *httpStatusError
	if errors.As(err, &se) && se.Status == 429 {
		// 限流：按 retry_after 退避后重试一次。doRequest 在非 2xx 时同样
		// 返回响应体，所以这里能读到 retry_after。
		if wait := telegramRetryAfter(data); wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			data, err = postJSON(ctx, t.client, apiURL, payload)
		}
	}
	if err != nil {
		return err
	}

	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("响应无法解析: %w", err)
	}
	// 只看 HTTP 状态码会误判：Telegram 业务失败时也常返回 200。
	if !resp.OK {
		return fmt.Errorf("telegram 返回失败: %s", redactBare(truncate(resp.Description, 120)))
	}
	return nil
}

func telegramRetryAfter(data []byte) time.Duration {
	var r struct {
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0
	}
	s := time.Duration(r.Parameters.RetryAfter) * time.Second
	if s <= 0 || s > telegramMaxRetryAfter {
		return 0
	}
	return s
}
