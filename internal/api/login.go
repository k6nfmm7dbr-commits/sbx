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
	if token != "" && constTimeEqual(given, token) {
		http.SetCookie(w, &http.Cookie{
			Name:     "sbx_token",
			Value:    nodes.PyQuote(token, "/"),
			Path:     "/",
			MaxAge:   604800,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
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
