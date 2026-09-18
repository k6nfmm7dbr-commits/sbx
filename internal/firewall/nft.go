package firewall

import (
	"context"
	"encoding/json"
	"fmt"
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
func NewNft(confPath string) *Nft { return &Nft{confPath: confPath} }

func (n *Nft) Name() string { return "nft" }

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
	var doc nftDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("nft JSON 解析失败: %w", err)
	}
	res := Snapshot{}
	for _, item := range doc.Nftables {
		c := item.Counter
		if c == nil {
			continue
		}
		res[c.Name] = [2]int64{c.Bytes, c.Packets}
	}
	if len(res) == 0 {
		return nil, &ErrLookup{Msg: "nft table has no counters"}
	}
	return res, nil
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
