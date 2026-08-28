package connection

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ConntrackFlow 是一条 /proc/net/nf_conntrack 中的「已建立」流摘要。
// 只保留流向本机监听端口方向的流的客户端身份，用于按字节增量判断是否还有流量。
type ConntrackFlow struct {
	Proto   string // "tcp" / "udp"
	DstPort int    // 本机监听端口（dport 第一次出现）
	SrcIP   string // 客户端源 IP（src 第一次出现）
	SrcPort int    // 客户端源端口（sport 第一次出现）
	Bytes   int64  // 双向 bytes= 之和（累计值）
}

// ConntrackResult 是 conntrack 读取结果。必须区分三种情况：
//   - Available=true, Flows 可能为空：conntrack 正常，只是当前 0 条流；
//   - Available=false, Err==nil：conntrack 根本不可用（模块未加载 / 文件不存在）；
//   - Err!=nil：读取失败（不完整），此时 Partial=true。
type ConntrackResult struct {
	Flows     []ConntrackFlow
	Available bool
	Partial   bool
	Err       error
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
	return ConntrackResult{
		Flows:     ParseConntrack(string(b)),
		Available: true,
	}
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
		// TCP 有 state 字段（ESTABLISHED/TIME_WAIT/...），UDP 没有。
		if len(fields) < 6 {
			continue
		}
		proto := fields[2]
		switch proto {
		case "tcp":
			// 第 6 个字段（fields[5]）是连接状态；只保留 ESTABLISHED，忽略 TIME_WAIT/CLOSE_WAIT。
			if fields[5] != "ESTABLISHED" {
				continue
			}
		case "udp":
			// UDP 无 state 字段，继续。
		default:
			continue
		}

		var f ConntrackFlow
		f.Proto = proto
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
