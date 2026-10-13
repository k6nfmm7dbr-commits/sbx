// Package notify 实现流量日报的定时外推：
// 每天在配置的时刻把「截至当日」的完整流量账本渲染成一张飞书卡片推到群里，
// 并在检测到上次是异常退出（崩溃/断电）时补推一条「今日截至当前」的卡片。
//
// 设计边界（必须保持）：
//   - 推送失败绝不影响采集、策略与 HTTP 服务；
//   - 本包对 daily/totals/samples/counter_state/node_policy 一律只读，
//     只写 meta 表里 notify_* 前缀的键；
//   - 凭据不得出现在日志、错误或任何面向用户的输出里（见本文件）。
package notify

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// reFeishuHookPath 匹配飞书 webhook 路径里的 hook token。
var reFeishuHookPath = regexp.MustCompile(`/open-apis/bot/v2/hook/[A-Za-z0-9_-]+`)

// reBareFeishuHook 匹配裸文本里的飞书 webhook 地址（保留前缀，只抹 token）。
// 用于错误信息中残留完整 URL 的兜底场景。
var reBareFeishuHook = regexp.MustCompile(`(open\.feishu\.cn/open-apis/bot/v2/hook/)[A-Za-z0-9_-]+`)

// RedactURL 对 URL 做脱敏，保留足够的排查信息（scheme / host / 路径结构 / query 的键名），
// 抹掉所有可能承载凭据的部分：
//   - userinfo → ***
//   - query 的值 → ***（键名保留）
//   - fragment 整体丢弃
//   - 路径里的飞书 hook token（/open-apis/bot/v2/hook/<token>）→ ***
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
	return reFeishuHookPath.ReplaceAllString(p, "/open-apis/bot/v2/hook/***")
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
	return reBareFeishuHook.ReplaceAllString(s, "${1}***")
}

// RedactSecret 脱敏单个密钥。飞书签名密钥是用户自设的普通字符串，
// 没有任何"可用于识别的非敏感部分"，因此一律替换，绝不回显。
func RedactSecret(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "***"
}

// SafeErr 把错误转成可安全写入日志的字符串。
//
// 这是本包最容易踩的坑：net/http 会把请求错误包装成 *url.Error，而它的 Error()
// **包含完整 URL**——直接 slog.Warn("...", "err", err) 就会把 hook token 打进日志。
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
