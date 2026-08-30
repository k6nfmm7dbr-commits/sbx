package api

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// maxLoginBody 限制登录表单体大小，防止异常请求占用内存。
const maxLoginBody = 64 << 10

// 登录失败节流参数：同一来源 IP 连续失败达 loginFailBurst 次后，
// 每次尝试强制等待 loginFailDelay，并在 loginFailWindow 内累计。
// 32 hex token 暴破本不现实，但节流能挡住日志刷屏与凭据喷洒。
const (
	loginFailBurst  = 5
	loginFailWindow = 5 * time.Minute
	loginFailDelay  = 2 * time.Second
	loginFailMaxLen = 4096 // 追踪表上限，防内存被大量伪造源 IP 撑大
)

type loginFailState struct {
	count int
	last  time.Time
}

var (
	loginFailMu sync.Mutex
	loginFails  = map[string]*loginFailState{}
)

// loginClientKey 取来源标识。刻意只用 RemoteAddr 的 IP 部分，不信任
// X-Forwarded-For（可伪造，会让攻击者绕过节流并污染表）。
func loginClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginThrottle 返回本次请求应额外等待的时长。
func loginThrottle(key string, now time.Time) time.Duration {
	loginFailMu.Lock()
	defer loginFailMu.Unlock()
	gcLoginFailsLocked(now)
	st, ok := loginFails[key]
	if !ok || now.Sub(st.last) > loginFailWindow {
		return 0
	}
	if st.count < loginFailBurst {
		return 0
	}
	return loginFailDelay
}

func loginRecordFail(key string, now time.Time) {
	loginFailMu.Lock()
	defer loginFailMu.Unlock()
	gcLoginFailsLocked(now)
	st, ok := loginFails[key]
	if !ok || now.Sub(st.last) > loginFailWindow {
		if len(loginFails) >= loginFailMaxLen {
			return // 表已满：放弃记录而不是无界增长（失败仍会被拒绝）
		}
		loginFails[key] = &loginFailState{count: 1, last: now}
		return
	}
	st.count++
	st.last = now
}

func loginRecordSuccess(key string) {
	loginFailMu.Lock()
	defer loginFailMu.Unlock()
	delete(loginFails, key)
}

// gcLoginFailsLocked 清理过期条目（调用方持锁）。
func gcLoginFailsLocked(now time.Time) {
	for k, st := range loginFails {
		if now.Sub(st.last) > loginFailWindow {
			delete(loginFails, k)
		}
	}
}

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
	key := loginClientKey(r)
	now := time.Now()

	token := s.token()
	if token != "" && tokenEqual(given, token) {
		// 成功路径绝不延迟：节流只惩罚失败尝试。
		// （早期实现把 sleep 放在校验之前，导致手滑几次后即使输对也要等 2s，
		//  真机实测确认了这个问题；对攻击者的限速效果两种写法等价，
		//  因为失败请求同样会占住连接 2s。）
		loginRecordSuccess(key)
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
	// 失败：先记账，再按累计失败次数施加延迟（挡凭据喷洒与日志刷屏）。
	loginRecordFail(key, now)
	if d := loginThrottle(key, now); d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	s.redirect(w, "/login?error=1")
}

func (s *Server) redirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}
