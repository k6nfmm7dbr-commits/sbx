package connection

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ConntrackFlow 是一条 /proc/net/nf_conntrack 流摘要（TCP 保留连接状态）。
type ConntrackFlow struct {
	Proto   string // "tcp" / "udp"
	State   string // tcp: ESTABLISHED / SYN_SENT / SYN_RECV；udp: "udp"
	DstPort int    // 本机监听端口（dport 第一次出现）
	SrcIP   string // 客户端源 IP（src 第一次出现）
	SrcPort int    // 客户端源端口（sport 第一次出现）
	Bytes   int64  // 双向 bytes= 之和（累计值）
}

// 保留的连接状态：ESTABLISHED 是活跃，SYN_SENT/SYN_RECV 是握手候选。
// TIME_WAIT/CLOSE/FIN_WAIT/CLOSE_WAIT/LAST_ACK 等都是已死/收尾状态，被丢弃。
func keepTCPState(s string) bool {
	switch s {
	case "ESTABLISHED", "SYN_SENT", "SYN_RECV":
		return true
	}
	return false
}

// ConntrackResult 是 conntrack 读取结果。必须区分三种情况：
//   - Available=true, Flows 可能为空：conntrack 正常，只是当前 0 条相关流；
//   - Available=false, Err==nil：conntrack 根本不可用（模块未加载 / 文件不存在）；
//   - Err!=nil：读取失败（不完整），此时 Partial=true。
type ConntrackResult struct {
	Flows     []ConntrackFlow
	Available bool
	Partial   bool
	Err       error

	// Entries 是 conntrack 文件里的**总条目数**（未经协议/状态过滤）。
	// 用途：区分「conntrack 正常但当前没有相关流」与「conntrack 根本没在跟踪」。
	Entries int

	// Inactive 表示「文件可读但整表 0 条」——内核没在真正跟踪连接。
	// 出现在没有任何引用 ct 的 netfilter 规则的干净机器上：nf_conntrack 模块
	// 已加载、/proc/net/nf_conntrack 存在可读，但内核不建条目，内容恒为空。
	// 此时 Available 一并置 false（它作为数据源确实不可用），Inactive 仅供
	// 上层区分「模块缺失」与「未激活跟踪」以给出精准提示。
	//
	// 一台有网络活动的服务器不可能一条 conntrack 都没有（SSH/DNS 自身就会
	// 产生条目），因此「整表为 0」是可靠判据，不会与「conntrack 正常但当前
	// 无客户端连接」混淆——后者表里仍有其它流，Entries > 0。
	Inactive bool
}

const defaultConntrackPath = "/proc/net/nf_conntrack"

// ReadConntrack 读取并解析 conntrack 的 TCP/UDP 已建立流。
func ReadConntrack(path string) ConntrackResult {
	if path == "" {
		path = defaultConntrackPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 内核未加载 nf_conntrack（或精简系统）：明确「不可用」，不是 0 条流。
			return ConntrackResult{Available: false}
		}
		return ConntrackResult{Available: false, Partial: true, Err: err}
	}
	text := string(b)
	entries := countEntries(text)
	if entries == 0 {
		// 文件存在可读却一条都没有：内核未真正跟踪连接（缺少引用 ct 的规则）。
		// 作为判活数据源它不可用，必须让上层回退 /proc；Inactive 用于区分
		// 「模块缺失」（Available=false, Inactive=false）与本情况，以给出精准提示。
		return ConntrackResult{Available: false, Inactive: true}
	}
	return ConntrackResult{
		Flows:     ParseConntrack(text),
		Available: true,
		Entries:   entries,
	}
}

// countEntries 统计 conntrack 文件的非空行数（= 内核当前跟踪的条目总数）。
func countEntries(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// ParseConntrack 解析 conntrack 文本，保留 tcp ESTABLISHED 与 udp 已建立流。
// 每行形如：
//
//	ipv4  2 tcp  6 7199 ESTABLISHED src=1.2.3.4 dst=5.6.7.8 sport=35740
//	    dport=8844 packets=1671 bytes=1906488 src=5.6.7.8 dst=1.2.3.4 sport=8844
//	    dport=35740 packets=1696 bytes=141552 [ASSURED] mark=0 zone=0 use=2
func ParseConntrack(text string) []ConntrackFlow {
	var out []ConntrackFlow
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// 固定头部：l3 l4 proto num [timeout] [state] src=...
		// TCP 有 state 字段（ESTABLISHED/SYN_SENT/...），UDP 没有。
		if len(fields) < 6 {
			continue
		}
		proto := fields[2]
		var state string
		switch proto {
		case "tcp":
			state = fields[5]
			if !keepTCPState(state) {
				continue
			}
		case "udp":
			state = "udp"
		default:
			continue
		}

		var f ConntrackFlow
		f.Proto = proto
		f.State = state
		var gotSrc, gotSport, gotDport bool
		for _, field := range fields {
			switch {
			case strings.HasPrefix(field, "src=") && !gotSrc:
				f.SrcIP = field[len("src="):]
				gotSrc = true
			case strings.HasPrefix(field, "sport=") && !gotSport:
				f.SrcPort, _ = strconv.Atoi(field[len("sport="):])
				gotSport = true
			case strings.HasPrefix(field, "dport=") && !gotDport:
				f.DstPort, _ = strconv.Atoi(field[len("dport="):])
				gotDport = true
			case strings.HasPrefix(field, "bytes="):
				if v, err := strconv.ParseInt(field[len("bytes="):], 10, 64); err == nil {
					f.Bytes += v
				}
			}
		}
		if f.SrcIP == "" || f.DstPort == 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}
