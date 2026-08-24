package nodes

import (
	"net/url"
	"strings"
)

// PyQuote 复刻 urllib.parse.quote：始终保留 A-Za-z0-9_.-~，
// 另外保留 safe 中的字符；其余按 UTF-8 字节转成大写十六进制。
func PyQuote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		const hex = "0123456789ABCDEF"
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xF])
	}
	return b.String()
}

const pyHex = "0123456789ABCDEF"

// PyQuotePlus 复刻 urllib.parse.quote_plus（urlencode 的默认编码方式）：
// 空格变 '+'，其余同 quote(safe="")。不能用“先 quote 再把 %20 换成 +”的
// 捷径——必须逐字节编码，避免与真正的 %20 混淆。
func PyQuotePlus(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case isUnreserved(c):
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(pyHex[c>>4])
			b.WriteByte(pyHex[c&0xF])
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '_' || c == '.' || c == '-' || c == '~':
		return true
	}
	return false
}

// PyUnquote 百分号解码（尽力而为，非法序列原样保留）。
func PyUnquote(s string) (string, error) {
	out, err := url.PathUnescape(s)
	if err != nil {
		return s, err
	}
	return out, nil
}

// QueryPair 是有序查询参数（Python urlencode 保持字典插入序）。
type QueryPair struct{ K, V string }

// EncodeQuery 按插入序编码查询串。
func EncodeQuery(pairs []QueryPair) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, PyQuotePlus(p.K)+"="+PyQuotePlus(p.V))
	}
	return strings.Join(parts, "&")
}
