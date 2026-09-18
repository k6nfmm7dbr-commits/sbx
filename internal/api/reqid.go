package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
)

// ctxKey 是本包私有的 context 键类型（避免与其它包撞键）。
type ctxKey int

const reqIDKey ctxKey = 0

// reqSeq 是进程内单调序号，保证 ID 严格唯一（即便 rand 不可用）。
var reqSeq uint64

// newRequestID 生成请求关联 ID：随机前缀 + 单调序号。
// 随机部分避免被外部枚举/碰撞，序号部分保证同进程内唯一且无需依赖 rand 成功。
func newRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + strconv.FormatUint(atomic.AddUint64(&reqSeq, 1), 10)
	}
	return hex.EncodeToString(b[:]) + "-" + strconv.FormatUint(atomic.AddUint64(&reqSeq, 1), 10)
}

// requestID 取回当前请求的关联 ID（未注入时返回空串）。
func requestID(r *http.Request) string {
	v, _ := r.Context().Value(reqIDKey).(string)
	return v
}

// withRequestID 注入关联 ID 并回写 X-Request-Id 响应头。
//
// 用途：客户端只拿到通用错误文案，真正的细节在服务端日志里；用户报障时把
// 响应头/响应体里的 request_id 给维护者，即可在日志中精确定位那一次请求。
// 该中间件必须位于 recoverMiddleware **外层**，这样 panic 日志也能带上 ID。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		w.Header().Set("X-Request-Id", rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqIDKey, rid)))
	})
}

// logRequestError 统一错误日志字段（request_id / error_code / node_id / path）。
func logRequestError(msg, rid, code, nodeID string, r *http.Request, err error) {
	attrs := []any{
		"request_id", rid,
		"error_code", code,
		"method", r.Method,
		"path", r.URL.Path,
	}
	if nodeID != "" {
		attrs = append(attrs, "node_id", nodeID)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	slog.Error(msg, attrs...)
}
