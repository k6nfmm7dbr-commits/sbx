package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	// serverChanEndpoint / pushPlusEndpoint 都是固定端点，凭据拼在路径或请求体里。
	serverChanEndpoint = "https://sctapi.ftqq.com/"
	pushPlusEndpoint   = "https://www.pushplus.plus/send"
	// 两者的正文上限都远大于 IM，这里给一个保守值即可。
	serverChanMaxBytes = 32 << 10
	pushPlusMaxBytes   = 32 << 10
	// 推送服务的标题长度上限（保守取 32 个 UTF-16 单元）。
	notifyTitleMaxUnits = 32
)

// serverChanChannel 是 Server酱。
//
// ⚠️ 免费版每日额度很小（个位数），而半小时心跳一天就有 48 条。
// 因此强烈建议该渠道只订阅 daily：在 panel.json 里写 "events": ["daily"]。
type serverChanChannel struct {
	channelBase
	// endpoint 生产恒为 serverChanEndpoint；抽成字段只为让测试指向 httptest。
	endpoint string
	sendKey  string
	client   *http.Client
}

func (s *serverChanChannel) Caps() Capabilities { return Capabilities{MaxBytes: serverChanMaxBytes} }

func (s *serverChanChannel) Send(ctx context.Context, msg Message) error {
	form := url.Values{}
	form.Set("title", clipUTF16(msg.Title, notifyTitleMaxUnits))
	form.Set("desp", strings.Join(msg.Blocks, "\n\n"))

	data, err := postForm(ctx, s.client, s.endpoint+s.sendKey+".send", form)
	if err != nil {
		return err
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("响应无法解析: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("Server酱返回错误 %d: %s", resp.Code, truncate(resp.Msg, 120))
	}
	return nil
}

// pushPlusChannel 是 PushPlus。成功码是 200（不是 0）。
type pushPlusChannel struct {
	channelBase
	// endpoint 生产恒为 pushPlusEndpoint；抽成字段只为让测试指向 httptest。
	endpoint string
	token    string
	client   *http.Client
}

func (p *pushPlusChannel) Caps() Capabilities { return Capabilities{MaxBytes: pushPlusMaxBytes} }

func (p *pushPlusChannel) Send(ctx context.Context, msg Message) error {
	payload := map[string]any{
		"token": p.token,
		"title": clipUTF16(msg.Title, notifyTitleMaxUnits),
		// 用 txt 模板：正文按纯文本原样呈现，可回放行不会被 markdown 解析破坏。
		"content":  strings.Join(msg.Blocks, "\n\n"),
		"template": "txt",
	}
	data, err := postJSON(ctx, p.client, p.endpoint, payload)
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
	if resp.Code != 200 {
		return fmt.Errorf("PushPlus 返回错误 %d: %s", resp.Code, truncate(resp.Msg, 120))
	}
	return nil
}
