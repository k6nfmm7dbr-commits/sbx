package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// maxRespBytes 限制响应体读取长度，避免被超大响应拖垮内存。
const maxRespBytes = 64 << 10

// Message 是一次待发送的卡片。
type Message struct {
	// Event 是事件标识：EventDaily / EventRecover / EventTest。
	Event string
	// Title 是卡片的标题文本（不含装饰图标）。
	Title string
	// Card 是已渲染好的飞书卡片 JSON 对象。
	Card map[string]any
}

// Sender 是一个推送目标。实现必须遵守三条纪律：
//   - 错误里不得出现凭据（返回原始 err 即可，调用方统一经 SafeErr 处理）；
//   - 必须对响应体做语义判定，不能只看 HTTP 状态码；
//   - 响应体限长读取。
type Sender interface {
	Name() string
	Send(ctx context.Context, msg Message) error
}

// httpStatusError 表示非 2xx 响应。Body 已截断并脱敏，可安全记录。
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// postJSON 发送 JSON 请求并返回响应体。
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

// doRequest 执行请求并返回响应体。非 2xx 时同时返回响应体（供调用方读取服务端
// 的业务错误码）与 httpStatusError。
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
