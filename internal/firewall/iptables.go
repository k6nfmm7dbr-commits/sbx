package firewall

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

func logRepair(msg, detail string) {
	if detail == "" {
		slog.Info(msg)
	} else {
		slog.Warn(msg, "detail", detail)
	}
}

// iptLine 解析 `iptables -w -nvxL CHAIN` 输出中的 sbx 计数行：
// pkts bytes ... /* sbx:tag */ —— 与旧实现的正则逐字符一致。
var iptLine = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+.*?/\*\s*(sbx:[A-Za-z0-9_:.\-]+)\s*\*/`)

// Iptables 通过 exec iptables/ip6tables 读取自定义链计数（完整保留回退路径，
// 兼容 iptables-nft 与 iptables-legacy）。
type Iptables struct {
	scriptPath string

	// enabled 记录“曾经成功读到的 family”。一旦启用，后续任何一轮失败
	// 都必须让整轮 Read 失败（防止 partial snapshot 破坏 baseline）；
	// 从未成功的 family 视为不存在（纯 IPv4 环境不受影响）。
	mu      sync.Mutex
	enabled map[string]bool
}

// NewIptables 构造回退后端，scriptPath 用于 repair()。
func NewIptables(scriptPath string) *Iptables { return &Iptables{scriptPath: scriptPath} }

func (p *Iptables) Name() string { return "iptables" }

// readOne 读单个二进制（v4/v6）的两个链并聚合：tag@family -> [bytes,pkts]。
func (p *Iptables) readOne(ctx context.Context, binary, family string) (Snapshot, error) {
	res := Snapshot{}
	found := false
	for _, chain := range []string{IptChainIn, IptChainOut} {
		rc, out, _ := runCmdFn(ctx, binary, "-w", "-nvxL", chain)
		if rc != 0 {
			continue
		}
		found = true
		for _, line := range strings.Split(out, "\n") {
			m := iptLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			pkts, _ := strconv.ParseInt(m[1], 10, 64)
			byts, _ := strconv.ParseInt(m[2], 10, 64)
			tag := m[3]
			if strings.HasPrefix(tag, "sbx:epoch:") {
				// epoch 标记只需存在即可，不参与按 family 聚合
				res[tag] = [2]int64{0, 0}
				continue
			}
			key := tag + "@" + family
			prev := res[key]
			res[key] = [2]int64{prev[0] + byts, prev[1] + pkts}
		}
	}
	if !found {
		return nil, &ErrLookup{Msg: binary + " chains missing"}
	}
	return res, nil
}

func (p *Iptables) Read(ctx context.Context) (Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.enabled == nil {
		p.enabled = map[string]bool{}
	}
	res := Snapshot{}
	sawAny := false
	for _, pair := range [][2]string{{"iptables", "v4"}, {"ip6tables", "v6"}} {
		binary, fam := pair[0], pair[1]
		// 命令永久缺失：不参与统计（与旧行为一致，纯 IPv4 机器不受影响）。
		if !whichFn(binary) {
			continue
		}
		part, err := p.readOne(ctx, binary, fam)
		if err != nil {
			// 该 family 此前已成功纳入统计 → 本轮任何失败都必须整轮失败：
			// 绝不提交缺一半的 snapshot（partial snapshot 会摧毁 baseline，
			// 导致恢复后该 family 被当成新计数器全量重复入账）。
			// 尚未启用过的失败按“缺失”处理，避免把暂时故障误判成永久存在。
			if p.enabled[fam] {
				// 注意：此处刻意用 %s 切断错误链——底层多为 ErrLookup
				// （链缺失），若被 errors.As 穿透，Run 循环会把“已启用来源的
				// 暂时故障”误判为“规则不存在”而触发 repair 语义。
				return nil, fmt.Errorf("%s 读取失败(该来源此前已纳入统计, 本轮放弃提交): %v", binary, err)
			}
			continue
		}
		// 首次成功后该 family 成为必须项；进程生命周期内不降级
		// （临时 exec 失败不能被误判成“命令不存在”）。
		p.enabled[fam] = true
		for k, v := range part {
			res[k] = v
		}
		sawAny = true
	}
	if !sawAny {
		return nil, &ErrLookup{Msg: "iptables 计数链不存在"}
	}
	return res, nil
}

func (p *Iptables) Repair(ctx context.Context) error {
	if p.scriptPath == "" {
		return fmt.Errorf("未配置 iptables 脚本")
	}
	if _, err := osStat(p.scriptPath); err != nil {
		return fmt.Errorf("脚本不存在: %s", p.scriptPath)
	}
	rc, _, errMsg := runCmdFn(ctx, "sh", p.scriptPath, "apply")
	detail := strings.TrimSpace(errMsg)
	if rc != 0 {
		logRepair("重建 iptables 计数链:", detail)
		return fmt.Errorf("iptables.sh apply 失败: %s", detail)
	}
	logRepair("重建 iptables 计数链: ok", "")
	return nil
}

// ParseCounterName 对齐 parse_counter_name：
// nft 形如 sbx_n<id>_i / sbx_sys_o；iptables 形如 sbx:n1:i@v4（@family 后缀忽略）。
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

var (
	counterRe = regexp.MustCompile(`^sbx[_:](n(\d+)|sys)[_:]([io])$`)
	epochRe   = regexp.MustCompile(`^sbx[_:]epoch[_:](\d+)$`)
)

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

// SnapshotEpoch 返回快照中出现的世代号（旧实现取首个命中的键）。
func SnapshotEpoch(s Snapshot) (uint64, bool) {
	for name := range s {
		if e, ok := ParseEpochName(name); ok {
			return e, true
		}
	}
	return 0, false
}
