package connection

import (
	"net"
	"strings"
)

// parseRemoteIP 解析 /proc/net/{tcp,udp}[6] 的 rem_address 中的 IP 部分。
// IPv4 是 8 位 hex（小端 4 字节），IPv6 是 32 位 hex（小端每 4 字节一组）。
// 返回规范化的 IP 字符串（如 "1.2.3.4" / "2001:db8::1"）；解析失败返回空串。
func parseRemoteIP(rem string) string {
	addr := rem
	if i := strings.LastIndexByte(rem, ':'); i >= 0 {
		addr = rem[:i]
	}
	switch len(addr) {
	case 8: // IPv4：4 字节小端
		var b [4]byte
		for i := 0; i < 4; i++ {
			v, err := hexByte(addr[i*2 : i*2+2])
			if err != nil {
				return ""
			}
			b[3-i] = v // 小端 → 网络序
		}
		return net.IP(b[:]).String()
	case 32: // IPv6：16 字节，每 4 字节（一个 32-bit word）内小端
		var b [16]byte
		for g := 0; g < 4; g++ {
			// 每个 word 的 4 字节是 little-endian，需反转字节序
			for i := 0; i < 4; i++ {
				v, err := hexByte(addr[g*8+i*2 : g*8+i*2+2])
				if err != nil {
					return ""
				}
				b[g*4+(3-i)] = v
			}
		}
		return net.IP(b[:]).String()
	default:
		return ""
	}
}

func hexByte(s string) (byte, error) {
	var v byte
	for i := 0; i < 2; i++ {
		v <<= 4
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v |= c - '0'
		case c >= 'a' && c <= 'f':
			v |= c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v |= c - 'A' + 10
		default:
			return 0, errBadHex
		}
	}
	return v, nil
}

type badHexError struct{}

func (badHexError) Error() string { return "bad hex" }

var errBadHex = badHexError{}
