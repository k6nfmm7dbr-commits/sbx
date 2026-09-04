package firewall

import (
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// osStat 是 os.Stat 的包内别名（便于测试替换/统一错误处理）。
func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

func logRepair(msg, detail string) {
	if detail == "" {
		slog.Info(msg)
	} else {
		slog.Warn(msg, "detail", detail)
	}
}

var (
	// counterRe 匹配流量计数器名。历史上 iptables 后端用冒号分隔
	// （sbx:n1:i），nftables 用下划线（sbx_n1_i）；两种分隔符都继续接受，
	// 这样升级前用旧后端写入的 counter_state 行仍能被正确解析、累计不断档。
	counterRe = regexp.MustCompile(`^sbx[_:](n(\d+)|sys)[_:]([io])$`)
	epochRe   = regexp.MustCompile(`^sbx[_:]epoch[_:](\d+)$`)
)

// ParseCounterName 解析计数器名：nft 形如 sbx_n<id>_i / sbx_sys_o。
// `@family` 后缀（历史 iptables 形态遗留在 counter_state 里的键）被忽略。
// 返回 scope("node:<id>"|"system")、方向 rx/tx。
func ParseCounterName(name string) (scope, direction string, ok bool) {
	base := name
	if i := strings.IndexByte(name, '@'); i >= 0 {
		base = name[:i]
	}
	m := counterRe.FindStringSubmatch(base)
	if m == nil {
		return "", "", false
	}
	if m[1] == "sys" {
		scope = "system"
	} else {
		scope = "node:" + m[2]
	}
	if m[3] == "i" {
		direction = "rx"
	} else {
		direction = "tx"
	}
	return scope, direction, true
}

// ParseEpochName 提取规则集世代标记编号；非 epoch 计数器返回 false。
func ParseEpochName(name string) (uint64, bool) {
	base := name
	if i := strings.IndexByte(name, '@'); i >= 0 {
		base = name[:i]
	}
	m := epochRe.FindStringSubmatch(base)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// SnapshotEpoch 返回快照中出现的世代号（取首个命中的键）。
func SnapshotEpoch(s Snapshot) (uint64, bool) {
	for name := range s {
		if e, ok := ParseEpochName(name); ok {
			return e, true
		}
	}
	return 0, false
}
