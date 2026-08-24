package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
// 第一阶段保持与旧 Python 完全一致的行为；netlink 直读见 FUTURE_IMPROVEMENTS.md。
type Nft struct{ confPath string }

// NewNft 构造 nft 后端，confPath 用于 repair() 重建规则表。
func NewNft(confPath string) *Nft { return &Nft{confPath: confPath} }

func (n *Nft) Name() string { return "nft" }

func (n *Nft) Read(ctx context.Context) (Snapshot, error) {
	rc, out, errMsg := runCmdFn(ctx, "nft", "-j", "list", "counters", "table", "inet", NFTTable)
	if rc != 0 {
		msg := strings.TrimSpace(errMsg)
		if strings.Contains(msg, "No such file or directory") ||
			strings.Contains(msg, "does not exist") || rc == 1 {
			if msg == "" {
				msg = "nft table missing"
			}
			return nil, &ErrLookup{Msg: msg}
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
