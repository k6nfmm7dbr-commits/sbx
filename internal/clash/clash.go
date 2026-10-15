// Package clash 是 sing-box 内置 Clash API 的最小客户端。
//
// 用途只有一个：读取实时连接表，从而按「域名」统计流量。
// sing-box 自身不提供按域名的流量统计，nftables 计数器只有端口维度，
// 连接表是唯一能同时拿到「域名 + 字节数 + 属于哪个节点」的地方。
//
// 安全：该接口具备控制能力（可切断连接、切换出站），因此 SBX 只让它监听
// 127.0.0.1 并带随机密钥。sing-box 1.14 不支持 unix socket（已实测），
// 所以只能走回环 TCP。
package clash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBody 是响应体上限，防止异常响应把内存吃满。
const maxBody = 16 << 20

// Metadata 是连接的目标与来源信息。
type Metadata struct {
	Network string `json:"network"`
	// Type 形如 "shadowsocks/sbx-n2"，即「入站类型/入站标签」。
	// 这是把流量归到具体节点的唯一依据。
	Type            string `json:"type"`
	Host            string `json:"host"`
	SourceIP        string `json:"sourceIP"`
	DestinationIP   string `json:"destinationIP"`
	DestinationPort string `json:"destinationPort"`
}

// InboundTag 取出入站标签，例如 "shadowsocks/sbx-n2" → "sbx-n2"。
func (m Metadata) InboundTag() string {
	if i := strings.LastIndex(m.Type, "/"); i >= 0 {
		return m.Type[i+1:]
	}
	return m.Type
}

// Connection 是一条活动连接。
//
// 注意：连接一旦关闭就会从列表消失，因此字节数只能靠高频轮询做差分累加。
type Connection struct {
	ID       string   `json:"id"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
	Chains   []string `json:"chains"`
	Rule     string   `json:"rule"`
	Metadata Metadata `json:"metadata"`
}

type connectionsResponse struct {
	Connections []Connection `json:"connections"`
}

// Client 访问一个 Clash API 端点。
type Client struct {
	baseURL string
	secret  string
	hc      *http.Client
}

// New 构造客户端。addr 形如 "127.0.0.1:19090"。
func New(addr, secret string) *Client {
	return &Client{
		baseURL: "http://" + addr,
		secret:  secret,
		// 超时必须短：这个调用发生在采集主循环里，不能拖慢计数。
		hc: &http.Client{Timeout: 2 * time.Second},
	}
}

// Connections 返回当前所有活动连接。
func (c *Client) Connections(ctx context.Context) ([]Connection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/connections", nil)
	if err != nil {
		return nil, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clash api 返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	var out connectionsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析连接表失败: %w", err)
	}
	return out.Connections, nil
}
