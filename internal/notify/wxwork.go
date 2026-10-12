package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	// 企业微信群机器人的容量：text 2048 字节 / markdown 4096 字节。
	wxWorkTextMaxBytes     = 2048
	wxWorkMarkdownMaxBytes = 4096
)

type wxworkChannel struct {
	channelBase
	webhook string
	msgtype string
	client  *http.Client
}

// Caps 按 msgtype 返回容量。默认 text：上限更小，但零 markdown 解析风险
// （数据行不会被 * _ ` 等字符影响）。
func (w *wxworkChannel) Caps() Capabilities {
	if w.msgtype == "markdown" {
		return Capabilities{MaxBytes: wxWorkMarkdownMaxBytes}
	}
	return Capabilities{MaxBytes: wxWorkTextMaxBytes}
}

func (w *wxworkChannel) Send(ctx context.Context, msg Message) error {
	for _, block := range msg.Blocks {
		payload := map[string]any{"msgtype": w.msgtype}
		if w.msgtype == "markdown" {
			payload["markdown"] = map[string]string{"content": block}
		} else {
			payload["text"] = map[string]string{"content": block}
		}
		data, err := postJSON(ctx, w.client, w.webhook, payload)
		if err != nil {
			return err
		}
		var resp struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("响应无法解析: %w", err)
		}
		// 企业微信同样以 HTTP 200 + errcode 表达业务失败。
		if resp.ErrCode != 0 {
			return fmt.Errorf("企业微信返回错误 %d: %s", resp.ErrCode, truncate(resp.ErrMsg, 120))
		}
	}
	return nil
}
