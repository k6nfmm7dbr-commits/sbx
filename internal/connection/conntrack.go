package connection

import (
	"os"
	"strconv"
	"strings"
)

// ConntrackFlow 是一条 /proc/net/nf_conntrack 中「已建立」的 TCP 流摘要。
// 只保留流向本机监听端口方向的流的客户端身份，用于按字节增量判断是否还有流量。
type ConntrackFlow struct {
	Proto   string // 固定 "tcp"
	DstPort int    // 本机监听端口（dport 第一次出现）
	SrcIP   string // 客户端源 IP（src 第一次出现）
	SrcPort int    // 客户端源端口（sport 第一次出现）
	Bytes   int64  // 双向 bytes= 之和（累计值）
}

const defaultConntrackPath = "/proc/net/nf_conntrack"

// ReadConntrack 读取并解析 conntrack 的 TCP ESTABLISHED 流。
// 文件不存在 / 读取失败 / 关闭 conntrack 时返回空切片（仁慈回退，不视为错误）：
// 上层见空结果即回退到 /proc ESTABLISHED 口径。
func ReadConntrack(path string) []ConntrackFlow {
	if path == "" {
		path = defaultConntrackPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return ParseConntrack(string(b))
}

// ParseConntrack 解析 conntrack 文本，只返回 tcp + ESTABLISHED 的流。
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
		// 固定头部：l3  l4  proto  num  timeout  state ...
		if len(fields) < 6 || fields[2] != "tcp" || fields[5] != "ESTABLISHED" {
			continue
		}
		var f ConntrackFlow
		f.Proto = "tcp"
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
