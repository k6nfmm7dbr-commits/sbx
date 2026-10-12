// Package notify 实现流量数据的定时外推与"异地留痕"：
// 按固定节奏把当日/前日流量推送到 IM 或 Webhook，并在每条消息里内嵌一段
// 可机器解析的数据（SBX1 行），使聊天记录本身成为可回放的备份。
//
// 设计边界（必须保持）：
//   - 推送失败绝不影响采集、策略与 HTTP 服务；
//   - 本包对 daily/totals/samples/counter_state/node_policy 一律只读，
//     只写 meta 表里 notify_* 前缀的三个键；
//   - 凭据不得出现在日志、错误或任何面向用户的输出里（见 redact.go）。
package notify

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// reBareBotToken 匹配裸文本里的 Telegram bot token（<数字>:<token>）。
var reBareBotToken = regexp.MustCompile(`[0-9]{5,}:[A-Za-z0-9_-]{20,}`)

// reBareSendKey 匹配裸文本里的 Server酱 sendkey。
var reBareSendKey = regexp.MustCompile(`SCT[A-Za-z0-9]{6,}`)

// reBotPath 匹配 Telegram API 路径里的 bot token。
var reBotPath = regexp.MustCompile(`/bot[0-9]+:[A-Za-z0-9_-]+`)

// reSendKeyPath 匹配 Server酱 路径里的 sendkey。
var reSendKeyPath = regexp.MustCompile(`/SCT[A-Za-z0-9]+`)

// RedactURL 对 URL 做脱敏，保留足够的排查信息（scheme / host / 路径结构 / query 的键名），
// 抹掉所有可能承载凭据的部分：
//   - userinfo → ***
//   - query 的值 → ***（键名保留）
//   - fragment 整体丢弃
//   - 路径里的 Telegram bot token（/bot<id>:<token>）与 Server酱 sendkey（/SCT...）→ ***
//
// 无法解析为 URL 时退化为对裸凭据模式的正则替换。
func RedactURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return redactBare(s)
	}
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != nil {
		b.WriteString("***@")
	}
	b.WriteString(u.Host)
	b.WriteString(redactPath(u.Path))
	if u.RawQuery != "" {
		b.WriteString("?")
		b.WriteString(redactQuery(u.RawQuery))
	}
	return b.String()
}

func redactPath(p string) string {
	if p == "" {
		return ""
	}
	if reBotPath.MatchString(p) {
		p = reBotPath.ReplaceAllString(p, "/bot***")
	}
	if reSendKeyPath.MatchString(p) {
		p = reSendKeyPath.ReplaceAllString(p, "/***")
	}
	return p
}

func redactQuery(q string) string {
	parts := strings.Split(q, "&")
	for i, kv := range parts {
		if eq := strings.Index(kv, "="); eq >= 0 {
			parts[i] = kv[:eq] + "=***"
		}
	}
	return strings.Join(parts, "&")
}

// redactBare 对不是合法 URL 的文本做兜底脱敏。
func redactBare(s string) string {
	s = reBareBotToken.ReplaceAllString(s, "***")
	return reBareSendKey.ReplaceAllString(s, "***")
}

// RedactSecret 脱敏单个凭据。Telegram bot token 保留 bot id（便于判断是哪个 bot），
// 其余一律替换，绝不回显 token 本体。
func RedactSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, ":"); i > 0 && isAllDigits(s[:i]) {
		return s[:i] + ":***"
	}
	return "***"
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SafeErr 把错误转成可安全写入日志的字符串。
//
// 这是本包最容易踩的坑：net/http 会把请求错误包装成 *url.Error，而它的 Error()
// **包含完整 URL**——直接 slog.Warn("...", "err", err) 就会把 ?key=... 打进日志。
// 因此所有进入日志的错误都必须经过这里。
func SafeErr(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		cause := ""
		if ue.Err != nil {
			cause = redactBare(ue.Err.Error())
		}
		return ue.Op + " " + RedactURL(ue.URL) + ": " + describeCause(cause)
	}
	return redactBare(err.Error())
}

// describeCause 把常见网络错误折叠成简短描述；未知错误原样返回（已脱敏）。
func describeCause(cause string) string {
	switch {
	case cause == "":
		return "未知错误"
	case strings.Contains(cause, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(cause, "no such host"):
		return "域名解析失败"
	case strings.Contains(cause, "i/o timeout") || strings.Contains(cause, "context deadline exceeded"):
		return "超时"
	case strings.Contains(cause, "certificate"):
		return "TLS 证书校验失败"
	}
	return cause
}

// IsTimeout 判断错误是否为超时（重试策略依赖它）。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
