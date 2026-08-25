package nodes

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Shadowsocks 2022 标准 method 名（sing-box 规范）。
const (
	SS2022Method128 = "2022-blake3-aes-128-gcm"
	SS2022Method256 = "2022-blake3-aes-256-gcm"
)

// ss2022KeySize 返回 method 对应的原始 key 字节数；未知 method 返回 -1。
func ss2022KeySize(method string) int {
	switch method {
	case SS2022Method128:
		return 16
	case SS2022Method256:
		return 32
	default:
		return -1
	}
}

// IsSS2022Method 报告 method 是否为合法的 Shadowsocks 2022 方法。
func IsSS2022Method(method string) bool { return ss2022KeySize(method) > 0 }

// GenerateSS2022Password 用 crypto/rand 生成 method 对应的 Base64 密码：
//
//	2022-blake3-aes-128-gcm → 16 字节
//	2022-blake3-aes-256-gcm → 32 字节
//
// 未知 method 返回错误——绝不 fallback 到 128。
func GenerateSS2022Password(method string) (string, error) {
	n := ss2022KeySize(method)
	if n < 0 {
		return "", fmt.Errorf("未知的 Shadowsocks 2022 method: %s", method)
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 key 失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}
