package firewall

import (
	"log/slog"
	"os"
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

// 计数器名曾是正则解析（^sbx[_:](n(\d+)|sys)[_:]([io])$），但这里表达的是
// 固定格式而不是模式：每轮采集都要对快照里全部计数器调用一次，正则要分配
// 子匹配切片。改为字节切分后实测 133ns/1alloc → 15.5ns/0alloc（约 8.6 倍）。
//
// 等价性由 counters_test.go 的对照测试保证：参考正则仍保留在测试里，
// 对包含混合分隔符在内的全部边界形态逐一比对。
//
// 两个分隔符必须各自独立判定（允许 sbx_n1:i 这种混合形态）——
// 原正则的两个 [_:] 并不互相约束，手写版若要求分隔符一致会悄悄改变行为。
const counterPrefix = "sbx"

// splitCounterBase 去掉 @family 后缀并校验 sbx 前缀，返回分隔符与其余部分。
func splitCounterBase(name string) (rest string, sep byte, ok bool) {
	base := name
	if i := strings.IndexByte(base, '@'); i >= 0 {
		base = base[:i]
	}
	if len(base) <= len(counterPrefix) || !strings.HasPrefix(base, counterPrefix) {
		return "", 0, false
	}
	sep = base[len(counterPrefix)]
	if sep != '_' && sep != ':' {
		return "", 0, false
	}
	return base[len(counterPrefix)+1:], sep, true
}

// isDigits 判定纯 ASCII 数字，与正则的 \d 口径一致（Go 的 \d 不含 Unicode 数字）。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseCounterName 解析计数器名：nft 形如 sbx_n<id>_i / sbx_sys_o。
// `@family` 后缀（历史 iptables 形态遗留在 counter_state 里的键）被忽略。
// 返回 scope("node:<id>"|"system")、方向 rx/tx。
func ParseCounterName(name string) (scope, direction string, ok bool) {
	rest, _, valid := splitCounterBase(name)
	if !valid {
		return "", "", false
	}
	// 方向是最后一个分隔符之后的单字符；LastIndexAny 同时找 '_' 与 ':'，
	// 因此混合分隔符形态与原正则行为一致。
	i := strings.LastIndexAny(rest, "_:")
	if i < 0 || len(rest)-i-1 != 1 {
		return "", "", false
	}
	switch rest[i+1] {
	case 'i':
		direction = "rx"
	case 'o':
		direction = "tx"
	default:
		return "", "", false
	}
	mid := rest[:i]
	if mid == "sys" {
		return "system", direction, true
	}
	// 节点形态为 n<digits>，且中段不得再含分隔符（等价于正则的 $ 锚定）。
	if len(mid) < 2 || mid[0] != 'n' || strings.ContainsAny(mid, "_:") {
		return "", "", false
	}
	if !isDigits(mid[1:]) {
		return "", "", false
	}
	return "node:" + mid[1:], direction, true
}

// ParseEpochName 提取规则集世代标记编号；非 epoch 计数器返回 false。
func ParseEpochName(name string) (uint64, bool) {
	rest, _, ok := splitCounterBase(name)
	if !ok || !strings.HasPrefix(rest, "epoch") {
		return 0, false
	}
	rest = rest[len("epoch"):]
	if len(rest) < 2 || (rest[0] != '_' && rest[0] != ':') {
		return 0, false
	}
	digits := rest[1:]
	if !isDigits(digits) {
		return 0, false
	}
	// 溢出时与原实现一致地判为无效，而不是截断成一个错误的世代号。
	v, err := strconv.ParseUint(digits, 10, 64)
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
