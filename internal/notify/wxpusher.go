package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	// wxPusherBaseURL 生产恒为官方域名；抽成字段只为让测试能指向 httptest。
	wxPusherBaseURL = "https://wxpusher.zjiecode.com"
	// wxPusherSuccessCode 是官方的成功业务码。
	//
	// 注意它既不是 0（Server酱）、也不是 200（PushPlus or HTTP）：
	// 各家成功码口径都不一样，只看 HTTP 200 会把业务失败当成功。
	wxPusherSuccessCode = 1000
	// wxPusherContentTypeText：1 = 纯文本。
	//
	// 刻意不用 html(2) 或 markdown(3)——它们会解析 < > * 等字符，
	// 可能破坏内嵌的 SBX1 数据行。
	wxPusherContentTypeText = 1
	// wxPusherMaxBytes 是 content 的 UTF-8 字节上限（官方 65535），
	// 这里留出 JSON 结构与 summary 的余量。
	wxPusherMaxBytes = 60000
	// wxPusherSummaryMaxChars 是 summary 的上限（官方 100 字符）。
	wxPusherSummaryMaxChars = 100
)

// wxPusherChannel 同时支持两种模式：
//
//   - **极简（SPT）**：扫个码就能拿到 token，无需注册账号或创建应用，适合自用。
//   - **标准**：需要 app_token + uids（或 topic_ids），能力更全。
//
// 用哪个模式由配置里填了什么决定（校验在 config 层完成，见 reWxPusherSPT）。
//
// 接收端说明（重要）：WxPusher 主推的是**独立全平台客户端**（Android/iOS/鸿蒙/
// 桌面端），走厂商推送与 APNs；微信只是补充渠道（ClawBot/iLink），且该渠道
// **24 小时内最多收 10 条**、需用户手动回复激活。因此高频心跳应走 App 通道。
type wxPusherChannel struct {
	channelBase
	baseURL  string
	simple   bool
	spt      string
	appToken string
	uids     []string
	topicIDs []int64
	client   *http.Client
}

func (w *wxPusherChannel) Caps() Capabilities {
	return Capabilities{MaxBytes: wxPusherMaxBytes}
}

func (w *wxPusherChannel) url() string {
	if w.simple {
		return w.baseURL + "/api/send/message/simple-push"
	}
	return w.baseURL + "/api/send/message"
}

func (w *wxPusherChannel) Send(ctx context.Context, msg Message) error {
	summary := clipUTF16(msg.Title, wxPusherSummaryMaxChars)
	for _, block := range msg.Blocks {
		if err := w.sendBlock(ctx, summary, block); err != nil {
			return err
		}
	}
	return nil
}

func (w *wxPusherChannel) sendBlock(ctx context.Context, summary, content string) error {
	payload := map[string]any{
		"content":     content,
		"summary":     summary,
		"contentType": wxPusherContentTypeText,
	}
	if w.simple {
		payload["spt"] = w.spt
	} else {
		payload["appToken"] = w.appToken
		payload["uids"] = w.uids
		if len(w.topicIDs) > 0 {
			payload["topicIds"] = w.topicIDs
		}
	}

	data, err := postJSON(ctx, w.client, w.url(), payload)
	if err != nil {
		return err
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("响应无法解析: %w", err)
	}
	if resp.Code != wxPusherSuccessCode {
		return fmt.Errorf("WxPusher 返回错误 %d: %s", resp.Code, truncate(resp.Msg, 120))
	}
	return nil
}
