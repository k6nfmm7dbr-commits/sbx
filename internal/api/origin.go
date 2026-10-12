package api

import (
	"net/http"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

// originAllowed 判断状态变更（非安全方法）请求的来源是否可信。
//
// 规则（默认 fail-closed）：
//  1. Origin 与 Referer 都不存在 → 放行。curl、监控、自动化客户端不会带这两个头，
//     拦住它们会直接打断 scripts/e2e_remote.sh 这类无人值守用法。
//  2. 默认只接受"与请求 Host 一致（host 与端口都必须相同）"的来源。
//     刻意**不再**做端口无关的主机名回退：Cookie 的 SameSite 只按 scheme+可注册域名
//     判定同 site（忽略端口），所以"同主机名不同端口"在浏览器看来仍会带上 Cookie——
//     攻击者只要能在与面板同一主机名的其它端口提供内容（同机多服务、同租户等），
//     就能借端口回退绕过这层 CSRF 兜底。
//  3. 反向代理改写了 Host（浏览器发出 https://panel.example.com，转发到后端时
//     Host 变成 panel.example.com:8080）时严格比较会拒绝请求。这类部署必须显式在
//     panel.json 配置 trusted_origin，用声明式白名单取代隐式放宽——"配置了才放宽"
//     比"猜部署形态"安全得多。
//
// 比较基准统一经 config.CanonicalOrigin 规范化（scheme/host 小写、去尾点、省略默认
// 端口），否则"浏览器省略默认端口"与"Host 带端口"的组合会产生等价地址误判。
// 与请求 Host 比较时只取 authority，不比较 scheme：面板可能同时经 http 与 https
// 可达，且 schemeful same-site 已由浏览器侧保证不会带上跨 scheme 的 Cookie。
func (s *Server) originAllowed(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return true
	}
	canon, err := config.CanonicalOrigin(raw)
	if err != nil {
		return false
	}
	if authorityOf(canon) == normalizeHostLiteral(r.Host) {
		return true
	}
	if s != nil && s.cfg != nil && s.cfg.TrustedOrigin != "" && canon == s.cfg.TrustedOrigin {
		return true
	}
	return false
}

// authorityOf 取出 canonical origin 的 "host[:port]" 部分。
// 入参必须是 config.CanonicalOrigin 的返回值（必然含 "://"），因此不会越界。
func authorityOf(canonical string) string {
	if i := strings.Index(canonical, "://"); i >= 0 {
		return canonical[i+3:]
	}
	return canonical
}

// normalizeHostLiteral 归一化请求 Host 字面量：小写 + 去掉末尾的点。
// 与 config.CanonicalOrigin 对 host 的处理保持一致，避免大小写/尾点造成误判。
func normalizeHostLiteral(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// requestOrigin 返回请求携带的原始来源，仅用于服务端审计日志的诊断字段。
// 它由外部控制，绝不可回显给客户端。
func requestOrigin(r *http.Request) string {
	if v := r.Header.Get("Origin"); v != "" {
		return v
	}
	return r.Header.Get("Referer")
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	default:
		return false
	}
}

// originErrorText 是 403 的对外文案。带上排查线索是刻意的：反代改写 Host 端口导致的
// 403 无法从"cross-site request blocked"里看出来，用户会卡死且无从下手。
// 它不含任何内部路径、配置内容或来源信息。
const originErrorText = "cross-site request blocked; 若为反向代理部署，请将 panel.json 的 trusted_origin 配置为浏览器访问面板的公开地址"

// originAuditFields 组装跨站拦截的审计字段（含被拒来源，便于运维定位反代配置问题）。
func originAuditFields(r *http.Request) []any {
	return []any{"error_code", codeCrossSiteBlocked, "origin", requestOrigin(r)}
}
