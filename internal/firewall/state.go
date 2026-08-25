package firewall

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

// effectiveBackend 状态文件路径。默认 /run/sbx（tmpfs，重启即清空；
// 环境变量 SBX_RUN_DIR 用于测试/沙箱覆盖）。
func effectiveBackendPath() string {
	dir := os.Getenv("SBX_RUN_DIR")
	if dir == "" {
		dir = "/run/sbx"
	}
	return filepath.Join(dir, "effective-backend")
}

// WriteEffectiveBackend 原子记录「实际应用成功的防火墙后端」（nft / iptables）。
// Apply 成功后调用；Collector/Repair/Clear 据此读取同一后端，
// 避免 backend=auto 下「实际规则是 iptables、Collector 却重新探测出 nft」的分叉。
func WriteEffectiveBackend(b string) error {
	if b != "nft" && b != "iptables" {
		return &ErrLookup{Msg: "非法后端: " + b}
	}
	if err := os.MkdirAll(filepath.Dir(effectiveBackendPath()), 0o755); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(effectiveBackendPath(), []byte(b+"\n"), 0o644)
}

// ReadEffectiveBackend 读取持久化的后端。内容只能是 nft / iptables；
// 文件不存在或损坏返回 ("", false)——调用方需按探测逻辑兜底。
func ReadEffectiveBackend() (string, bool) {
	data, err := os.ReadFile(effectiveBackendPath())
	if err != nil {
		return "", false
	}
	b := strings.TrimSpace(string(data))
	if b != "nft" && b != "iptables" {
		return "", false
	}
	return b, true
}

// normalizeBackend 归一化配置：nft/nftables → nft；ipt/iptables → iptables；其它 → auto。
func normalizeBackend(forced string) string {
	switch strings.ToLower(strings.TrimSpace(forced)) {
	case "nft", "nftables":
		return "nft"
	case "ipt", "iptables":
		return "iptables"
	default:
		return "auto"
	}
}

// probeBackend 探测：nft 命令存在且 list tables 成功 → nft；否则 iptables；
// 都没有 → nft（由后续 Read 报错自愈）。
func probeBackend() string {
	if whichFn("nft") {
		if rc, _, _ := runCmdFn(context.Background(), "nft", "list", "tables"); rc == 0 {
			return "nft"
		}
	}
	if whichFn("iptables") {
		return "iptables"
	}
	return "nft"
}

// EffectiveBackend 决定本次使用的后端：
//   - forced=nft/iptables：直接返回（绝不 fallback、不读状态）
//   - forced=auto：优先读持久化状态；无状态时探测。
//
// 这是 Collector / Repair / Clear 共享的单一决策入口；Apply 在真正成功后
// 调用 WriteEffectiveBackend 落盘，从而保证「Apply 后端 == Collector 后端」。
func EffectiveBackend(forced string) string {
	switch normalizeBackend(forced) {
	case "nft":
		return "nft"
	case "iptables":
		return "iptables"
	}
	if b, ok := ReadEffectiveBackend(); ok {
		return b
	}
	return probeBackend()
}

// IsAutoBackend 报告配置是否为 auto（决定 Apply 是否允许 fallback）。
func IsAutoBackend(forced string) bool { return normalizeBackend(forced) == "auto" }

// IsMissingMsg 判断 nft/iptables 错误信息是否为「目标不存在」类（可自愈）。
// 供 nft.go 分类 ErrLookup，以及 service.Clear 判断「删除时表/链本就不存在」。
func IsMissingMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no such file or directory") ||
		strings.Contains(m, "does not exist") ||
		strings.Contains(m, "no such table") ||
		strings.Contains(m, "no such chain")
}
