package nodes

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// GenerateHex 生成 n 字节密码学安全随机数据的十六进制字符串（2n 字符）。
func GenerateHex(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("字节数必须 > 0")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateBase64 生成 n 字节随机数据的标准 Base64 字符串。
func GenerateBase64(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("字节数必须 > 0")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
