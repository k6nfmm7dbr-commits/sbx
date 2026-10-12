package api

import (
	"log/slog"
	"net/http"
)

// auditEvent 记录低敏感安全审计字段，统一包含事件、请求关联 ID、RemoteAddr IP、方法、路径和结果。
// 绝对红线：审计字段及其调用方参数不得包含面板 token、节点 password/PSK/UUID、
// Reality 私钥或其它可用于连接节点的凭据；禁止记录请求体、Cookie 与 Authorization。
// 来源 IP 只取 RemoteAddr，绝不信任可伪造的 X-Forwarded-For。
func auditEvent(r *http.Request, event, result string, fields ...any) {
	attrs := []any{
		"event", event,
		"request_id", requestID(r),
		"source_ip", loginClientKey(r),
		"method", r.Method,
		"path", r.URL.Path,
		"result", result,
	}
	attrs = append(attrs, fields...)
	slog.Info("security audit", attrs...)
}
