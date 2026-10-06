package firewall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// nftDoc 是 `nft -j list counters` 的 JSON 结构（只取用到的字段）。
type nftDoc struct {
	Nftables []struct {
		Counter *struct {
			Name    string `json:"name"`
			Bytes   int64  `json:"bytes"`
			Packets int64  `json:"packets"`
		} `json:"counter"`
	} `json:"nftables"`
}

// Nft 通过 exec `nft -j list counters table inet sbx_traffic` 读取计数器。
//
// 关于 fork/exec 开销（审计项「nft 采集 fork/exec 开销」）：真机实测单次
// `nft -j list counters table inet sbx_traffic` 约 4.6ms（输出约 900 字节），
// 按默认 2 秒采样即 ~0.23% 单核——不是当前的主要开销（真正的大头是每秒
// conntrack 与 /proc 解析，已另行优化）。netlink 直读可省掉这部分，但需要
// 引入 netlink 依赖与内核特性探测，收益与复杂度不成正比，故列为
// FUTURE_IMPROVEMENTS 的待评估项（含设计草案）。
//
// 这里只做一件**零陈旧风险**的事：合并读取（single-flight）——同一时刻的
// 并发 Read 共享一次 exec。它不会缓存上一次的结果，因此任何一次 Read 只要
// 不是与别人并发，拿到的都是当次真实读数，不存在"读到旧计数"的可能。
type Nft struct {
	confPath string

	mu       sync.Mutex
	inflight *nftReadCall
}

// nftReadCall 是一次进行中的读取；等待者共享同一结果。
type nftReadCall struct {
	wg   sync.WaitGroup
	snap Snapshot
	err  error
}

// NewNft 构造 nft 后端，confPath 用于 repair() 重建规则表。
func NewNft(confPath string) *Nft { return &Nft{confPath} }

func (n *Nft) Name() string { return "nft" }

// Read 读取 nftables 计数器快照。使用流式 JSON 解析，仅提取需要的计数器，
// 跳过 sbx_epoch_* / sbx_sys_* / sbx_ct_activate 等无关计数器，降低 CPU 开销。
func (n *Nft) Read(ctx context.Context) (Snapshot, error) {
	n.mu.Lock()
	if call := n.inflight; call != nil {
		n.mu.Unlock()
		call.wg.Wait()
		return call.snap, call.err
	}
	call := &nftReadCall{}
	call.wg.Add(1)
	n.inflight = call
	n.mu.Unlock()

	call.snap, call.err = n.readOnce(ctx)

	n.mu.Lock()
	n.inflight = nil
	n.mu.Unlock()
	call.wg.Done()
	return call.snap, call.err
}

// readOnce 执行一次真实读取。
func (n *Nft) readOnce(ctx context.Context) (Snapshot, error) {
	rc, out, errMsg := runCmdFn(ctx, "nft", "-j", "list", "counters", "table", "inet", NFTTable)
	if rc != 0 {
		msg := strings.TrimSpace(errMsg)
		// 只有明确的「目标不存在」才算 ErrLookup（可自愈）。禁止把任意 rc=1
		// （如 permission denied / syntax error）误判为规则不存在——否则
		// Collector 会误以为缺规则而反复 Repair。
		if IsMissingMsg(msg) {
			if msg == "" {
				msg = "nft table missing"
			}
			return nil, &ErrLookup{Msg: msg}
		}
		if msg == "" {
			msg = fmt.Sprintf("nft 退出码 %d", rc)
		}
		return nil, fmt.Errorf("nft 读取失败: %s", msg)
	}

	// 使用流式 json.Decoder 解析，跳过不需要的计数器。
	res := make(Snapshot)
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	// 解析外层 { "nftables": [...] }
	if err := skipToKey(dec, "nftables"); err != nil {
		return nil, fmt.Errorf("nft JSON 解析失败: %w", err)
	}
	if err := skipToArrayStart(dec); err != nil {
		return nil, fmt.Errorf("nft JSON 解析失败: %w", err)
	}

	for {
		// 尝试解析数组中的下一个元素
		var item struct {
			Counter *struct {
				Name    string `json:"name"`
				Bytes   int64  `json:"bytes"`
				Packets int64  `json:"packets"`
			} `json:"counter"`
		}
		// 如果到达数组末尾，退出循环
		if dec.More() {
			if err := dec.Decode(&item); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("nft JSON 解析失败: %w", err)
			}
		} else {
			break
		}

		if item.Counter == nil {
			continue
		}
		name := item.Counter.Name
		// 只提取 sbx_n<id>_(i|o) 格式的计数器（节点流量计数）。
		// 跳过 sbx_epoch_* / sbx_sys_* / sbx_ct_activate 等无关计数器。
		if !strings.HasPrefix(name, "sbx_n") {
			continue
		}
		res[name] = [2]int64{item.Counter.Bytes, item.Counter.Packets}
	}

	if len(res) == 0 {
		return nil, &ErrLookup{Msg: "nft table has no counters"}
	}
	return res, nil
}

// skipToKey 跳过 JSON 对象直到找到指定 key。
func skipToKey(dec *json.Decoder, key string) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			if delim == '{' {
				// 嵌套对象，递归跳过
				if err := skipObject(dec); err != nil {
					return err
				}
				continue
			}
			if delim == '[' {
				// 嵌套数组，递归跳过
				if err := skipArray(dec); err != nil {
					return err
				}
				continue
			}
		}
		if s, ok := tok.(string); ok && s == key {
			return nil
		}
	}
}

// skipObject 跳过当前 JSON 对象（调用方已消费 '{'）。
func skipObject(dec *json.Decoder) error {
	for dec.More() {
		// 跳过 key
		if _, err := dec.Token(); err != nil {
			return err
		}
		// 跳过 value
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	// 消费 '}'
	_, err := dec.Token()
	return err
}

// skipArray 跳过当前 JSON 数组（调用方已消费 '['）。
func skipArray(dec *json.Decoder) error {
	for dec.More() {
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// skipToArrayStart 跳过外层对象直到找到 nftables 数组的起始 '['。
// 前置条件：已消费 "nftables" key，接下来应该是 ':' 和 '['。
func skipToArrayStart(dec *json.Decoder) error {
	// 跳过 ':'
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			if delim == '[' {
				return nil
			}
			// 如果是 '{' 或 '['，跳过整个值
			if delim == '{' {
				if err := skipObject(dec); err != nil {
					return err
				}
			} else if delim == '[' {
				if err := skipArray(dec); err != nil {
					return err
				}
			}
			continue
		}
		// 基本类型值，继续
	}
}

// skipValue 跳过任意 JSON 值。
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			return skipObject(dec)
		case '[':
			return skipArray(dec)
		}
	}
	return nil
}

// Repair 用安装时生成的规则文件重建计数器表。
func (n *Nft) Repair(ctx context.Context) error {
	if n.confPath == "" {
		return fmt.Errorf("未配置 nft 规则文件")
	}
	if _, err := osStat(n.confPath); err != nil {
		return fmt.Errorf("规则文件不存在: %s", n.confPath)
	}
	rc, _, errMsg := runCmdFn(ctx, "nft", "-f", n.confPath)
	if rc != 0 {
		logRepair("重建 nft 计数器表:", strings.TrimSpace(errMsg))
		return fmt.Errorf("nft -f 失败: %s", strings.TrimSpace(errMsg))
	}
	logRepair("重建 nft 计数器表: ok", "")
	return nil
}
