// Package connection 从 /proc/net/{tcp,udp}[6] 统计连接数并按节点端口归属。
// 直接读内核文本表，不 exec ss/netstat，保持轻量。
package connection

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

var (
	tcpProcFiles = []string{"/proc/net/tcp", "/proc/net/tcp6"}
	udpProcFiles = []string{"/proc/net/udp", "/proc/net/udp6"}
)

const tcpEstablished = "01" // /proc/net/tcp 的 ESTABLISHED 状态码

// Conns 对齐旧 {"tcp": int|None, "udp": int|None}；nil 表示该协议不适用。
type Conns struct {
	TCP *int
	UDP *int
}

// Keep 判定一行是否计入：st 为状态字段，rem 为远端地址。
type Keep func(st, rem string) bool

// ParseLocalPorts 解析 /proc 文本，返回满足 keep 的本地端口列表。
// 行格式: sl local_address rem_address st ...
func ParseLocalPorts(text string, keep Keep) []int {
	var ports []int
	lines := strings.Split(text, "\n")
	for _, line := range lines[min(1, len(lines)):] { // 跳过表头
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		local, rem, st := parts[1], parts[2], parts[3]
		i := strings.IndexByte(local, ':')
		if i < 0 {
			continue
		}
		if !keep(st, rem) {
			continue
		}
		p, err := strconv.ParseInt(local[i+1:], 16, 64)
		if err != nil {
			continue
		}
		ports = append(ports, int(p))
	}
	return ports
}

// RemConnected 远端地址非全零 = 该 socket 已与对端建立会话。
// 逐字符复刻旧 _rem_connected（含无冒号的防御分支）。
func RemConnected(rem string) bool {
	ip := rem
	port := "0"
	if i := strings.LastIndexByte(rem, ':'); i >= 0 {
		ip, port = rem[:i], rem[i+1:]
	}
	stripped := strings.ReplaceAll(ip, "0", "")
	return stripped != "" || !(port == "0" || port == "0000")
}

// CountByPort 读多个 /proc 文件，聚合每个本地端口的命中数。
// 返回 (hits, partial)：partial 表示至少一个文件「存在但读取失败」
// （如权限/临时 I/O 故障）。文件不存在（os.ErrNotExist）不算失败——
// 纯 IPv4 机器没有 /proc/net/tcp6 属正常。
func CountByPort(files []string, keep Keep, readFile func(string) (string, error)) (map[int]int, bool) {
	hits := map[int]int{}
	partial := false
	for _, path := range files {
		text, err := readFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			partial = true
			continue
		}
		for _, p := range ParseLocalPorts(text, keep) {
			hits[p]++
		}
	}
	return hits, partial
}

func readOSFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sumHits(hits map[int]int, lo, hi int64) int {
	total := 0
	for p, c := range hits {
		if int64(p) >= lo && int64(p) <= hi {
			total += c
		}
	}
	return total
}

// CountResult 连接数统计结果。
type CountResult struct {
	Conns   map[string]Conns
	Partial bool // 存在 /proc 文件读取失败，结果可能不完整
}

// CountForNodes 返回 {node_id_string: Conns}：
// tcp 仅 TCP 类协议统计 ESTABLISHED 数，udp 仅 UDP 类协议统计已建立会话数，
// 其余为 nil。节点端口非法时返回错误（对齐旧实现抛异常路径）。
func CountForNodes(list []nodes.Node) (CountResult, error) {
	return countForNodes(list, readOSFile)
}

func countForNodes(list []nodes.Node, readFile func(string) (string, error)) (CountResult, error) {
	tcpHits, tcpPartial := CountByPort(tcpProcFiles, func(st, rem string) bool { return st == tcpEstablished }, readFile)
	udpHits, udpPartial := CountByPort(udpProcFiles, func(_, rem string) bool { return RemConnected(rem) }, readFile)

	result := make(map[string]Conns, len(list))
	for _, n := range list {
		ranges := nodes.ParsePorts(n)
		if len(ranges) == 0 {
			return CountResult{}, fmt.Errorf("节点端口非法")
		}
		protos := nodes.Protocols(n)
		var pair Conns
		hasTCP, hasUDP := false, false
		for _, pr := range protos {
			if pr == "tcp" {
				hasTCP = true
			} else if pr == "udp" {
				hasUDP = true
			}
		}
		if hasTCP {
			v := 0
			for _, r := range ranges {
				v += sumHits(tcpHits, r[0], r[1])
			}
			pair.TCP = &v
		}
		if hasUDP {
			v := 0
			for _, r := range ranges {
				v += sumHits(udpHits, r[0], r[1])
			}
			pair.UDP = &v
		}
		result[nodes.IDString(n)] = pair
	}
	return CountResult{Conns: result, Partial: tcpPartial || udpPartial}, nil
}
