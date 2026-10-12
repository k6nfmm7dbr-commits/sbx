package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// webhookChannel 把推送内容以 JSON POST 到任意地址，并把结构化数据一并送出，
// 方便接收方（自建服务、Bark、ntfy 等）直接消费而无需解析文本。
type webhookChannel struct {
	channelBase
	url      string
	secret   string
	maxBytes int
	client   *http.Client
}

func (w *webhookChannel) Caps() Capabilities { return Capabilities{MaxBytes: w.maxBytes} }

type webhookTotals struct {
	Rx     int64 `json:"rx"`
	Tx     int64 `json:"tx"`
	RxPkts int64 `json:"rx_pkts"`
	TxPkts int64 `json:"tx_pkts"`
}

type webhookNode struct {
	Scope  string `json:"scope"`
	Name   string `json:"name"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
	RxPkts int64  `json:"rx_pkts"`
	TxPkts int64  `json:"tx_pkts"`
}

type webhookPayload struct {
	V         int           `json:"v"`
	Event     string        `json:"event"`
	TS        int64         `json:"ts"`
	Day       string        `json:"day"`
	Text      string        `json:"text"`
	Totals    webhookTotals `json:"totals"`
	Nodes     []webhookNode `json:"nodes"`
	Replay    []string      `json:"replay"`
	Truncated bool          `json:"truncated,omitempty"`
}

func (w *webhookChannel) Send(ctx context.Context, msg Message) error {
	payload := w.buildPayload(msg)

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SBX-Event", msg.Event)
	if w.secret != "" {
		// 接收方用同样的密钥重算并 **以 hmac.Equal 做时间无关比较** 来验签。
		// 这里绝不能用 == 比较签名。
		mac := hmac.New(sha256.New, []byte(w.secret))
		mac.Write(body)
		req.Header.Set("X-SBX-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	_, err = doRequest(w.client, req)
	return err
}

// buildPayload 组装请求体。
//
// text 与 replay 刻意不重复：text 只放人类可读的正文行，replay 只放 SBX1 行。
// 裁剪顺序为 nodes → text，**replay 永不裁剪** —— 它是"异地留痕"的载体，
// 为了迎合字节上限而丢掉它属于本末倒置。
func (w *webhookChannel) buildPayload(msg Message) webhookPayload {
	textLines := make([]string, 0, 32)
	replay := make([]string, 0, 4)
	for _, block := range msg.Blocks {
		for _, line := range strings.Split(block, "\n") {
			// 同一个块里可能既有正文行也有 SBX1 行（第一个块通常如此）。
			if idx := strings.Index(line, ReplayPrefix); idx >= 0 {
				replay = append(replay, strings.TrimSpace(line[idx:]))
				continue
			}
			if strings.TrimSpace(line) != "" {
				textLines = append(textLines, line)
			}
		}
	}

	p := webhookPayload{
		V:      1,
		Event:  msg.Event,
		Day:    msg.Day,
		Text:   strings.Join(textLines, "\n"),
		Replay: replay,
	}
	if msg.Snap != nil {
		p.TS = msg.Snap.Slot.Unix()
		p.Totals = webhookTotals{
			Rx: msg.Snap.Totals.Rx, Tx: msg.Snap.Totals.Tx,
			RxPkts: msg.Snap.Totals.RxPkts, TxPkts: msg.Snap.Totals.TxPkts,
		}
		for _, n := range msg.Snap.Nodes {
			p.Nodes = append(p.Nodes, webhookNode{
				Scope: n.Scope, Name: n.Name,
				Rx: n.Rx, Tx: n.Tx, RxPkts: n.RxPkts, TxPkts: n.TxPkts,
			})
		}
	}

	limit := w.maxBytes
	if limit <= 0 {
		limit = defaultWebhookLimit
	}
	for {
		b, err := json.Marshal(p)
		if err != nil || len(b) <= limit {
			return p
		}
		switch {
		case len(p.Nodes) > 0:
			// 一次砍掉一半，避免 50+ 节点场景下的 O(n²) 序列化。
			cut := len(p.Nodes) / 2
			if cut < 1 {
				cut = 1
			}
			p.Nodes = p.Nodes[:len(p.Nodes)-cut]
			p.Truncated = true
		case p.Text != "":
			p.Text = ""
			p.Truncated = true
		default:
			// 只剩 replay 仍然超限：接受。宁可超出目标字节数，
			// 也不能为了"看起来合规"而丢掉可回放数据。
			return p
		}
	}
}

// defaultWebhookLimit 与 config 的默认值保持一致（配置层已保证非 0，这里只是兜底）。
const defaultWebhookLimit = 8192
