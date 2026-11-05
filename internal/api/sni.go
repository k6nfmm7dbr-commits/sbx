package api

import (
	"errors"
	"strings"
)

// sniMaxLen 遵循 DNS 域名总长上限（253 字节，不含末尾点）。
const sniMaxLen = 253

// validateSNI 校验节点 SNI / 证书域名的字符范围。
//
// SNI 会作为**独立 argv** 传给 sing-box 的 `generate tls-keypair <sni>`，
// 也会拼进配置的 server_name 与 handshake.server。允许任意字符有两个问题：
//   - 以 `-` 开头的值会被 sing-box CLI 当作选项解析（CWE-88 参数注入，
//     例如 `--help` / `--config=/etc/shadow`），虽然不经 shell、不是 RCE；
//   - 空白、引号、逗号、shell 元字符进入下游配置或脚本时可能破坏结构。
//
// 这里按域名白名单收敛：仅字母、数字、点、连字符，长度上限，禁止以
// `-` 或 `.` 开头、禁止连续的点。空值由调用方决定（创建节点时会填默认
// 域名），此处放行。
func validateSNI(sni string) error {
	s := strings.TrimSpace(sni)
	if s == "" {
		return nil
	}
	if len(s) > sniMaxLen {
		return errors.New("SNI 过长（最多 253 字符）")
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") {
		return errors.New("SNI 不得以连字符或点开头")
	}
	if strings.Contains(s, "..") {
		return errors.New("SNI 不得包含连续的点")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return errors.New("SNI 只能包含字母、数字、点和连字符")
	}
	return nil
}
