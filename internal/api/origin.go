package api

import (
	"net/http"
	"net/url"
	"strings"
)

// originMatchesRequestHost 拒绝 Origin/Referer 主机名与 Host 不一致的浏览器请求。
// 先比较完整 authority（host:port）；若反代在 TLS 终止后暴露不同端口，再退化为主机名比较，
// 兼容浏览器 :443 与 Go 后端端口不同的部署，同时仍拒绝来自其他主机的请求。
// 两个浏览器来源头都不存在时放行，以兼容 curl、监控和自动化客户端。
func originMatchesRequestHost(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil {
		return false
	}
	hostPart := func(authority string) (host, port string) {
		a := url.URL{Host: authority}
		host = strings.TrimSuffix(strings.ToLower(a.Hostname()), ".")
		port = a.Port()
		return
	}
	requestHost, requestPort := hostPart(r.Host)
	originHost, originPort := hostPart(u.Host)
	if requestHost == "" || originHost == "" {
		return false
	}
	if requestHost == originHost && requestPort == originPort {
		return true
	}
	return requestHost == originHost
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	default:
		return false
	}
}
