package api

import (
	"io"
	"net/http"
	"net/url"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// maxLoginBody 限制登录表单体大小，防止异常请求占用内存。
const maxLoginBody = 64 << 10

// handlePost 仅处理 /login：POST 提交令牌，成功后下发 HttpOnly 会话 Cookie
// 并重定向；令牌不再出现在 URL 里（与旧实现一致）。
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, route string) {
	if route != "/login" {
		s.sendText(w, r, http.StatusNotFound, "not found")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBody+1))
	if err != nil {
		s.redirect(w, "/login?error=1")
		return
	}
	// 超限必须 413 拒绝，不得截断后继续解析（否则可能构造部分令牌绕过）。
	if len(body) > maxLoginBody {
		s.sendText(w, r, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	vals, perr := url.ParseQuery(string(body))
	given := ""
	if perr == nil {
		for _, v := range vals["token"] {
			if v != "" {
				given = v
				break
			}
		}
	}
	token := s.token()
	if token != "" && tokenEqual(given, token) {
		cookie := &http.Cookie{
			Name:     "sbx_token",
			Value:    nodes.PyQuote(token, "/"),
			Path:     "/",
			MaxAge:   604800,
			HttpOnly: true,
			// Lax 而非 Strict：iOS Safari 从外部链接/书签/回访直接打开面板时，
			// Strict 会让 Cookie 不随顶级导航发送，表现就像“没记住登录”。
			// Lax 仍能阻止跨站 POST/隐式子请求携带 Cookie（防 CSRF），
			// 但允许顶级 GET 导航携带，兼顾安全与“同设备免重复登录”。
			SameSite: http.SameSiteLaxMode,
		}
		// 仅当用户显式配置 secure_cookie（前端套了 HTTPS 反代）时才加 Secure，
		// 不能无条件 Secure=true，否则纯 HTTP 直连登录会失效。
		if s.cfg.SecureCookie {
			cookie.Secure = true
		}
		http.SetCookie(w, cookie)
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)
		return
	}
	s.redirect(w, "/login?error=1")
}

func (s *Server) redirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}
