// Package api 实现面板 HTTP 服务：路由、鉴权、静态资源与 JSON API，
// 与旧 Python ThreadingHTTPServer 版本对外行为保持一致。
package api

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// New 构造配置好超时参数的 HTTP Server（不启动监听）。
// 超时设置：慢头部/慢请求不会长期占用连接；写超时放宽以兼容 CSV 导出。
func New(cfg *config.Config, db *database.DB, src traffic.LiveSource, pol *policy.Service) (*Server, *http.Server) {
	s := &Server{cfg: cfg, db: db, src: src, policy: pol}
	hs := &http.Server{
		// IPv6 监听地址必须用 net.JoinHostPort 拼接（"::" → "[::]:8080"），
		// 不能用 cfg.Listen + ":" + port 得到非法 ":::8080"。
		Addr:              net.JoinHostPort(cfg.Listen, strconv.Itoa(cfg.Port)),
		Handler:           s.recoverMiddleware(s),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	return s, hs
}

// Server 持有面板运行依赖。
type Server struct {
	cfg    *config.Config
	db     *database.DB
	src    traffic.LiveSource
	policy *policy.Service
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("HTTP 处理器异常", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimRight(r.URL.Path, "/")
	if route == "" {
		route = "/"
	}

	// 与旧实现一致：do_GET 才有 /api 路由与统一鉴权；
	// do_POST 只处理 /login，其它路径一律 404 文本。
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if strings.HasPrefix(route, "/api/") {
			s.handleAPI(w, r, route)
			return
		}
		s.handleGet(w, r, route)
	case http.MethodPost:
		// 仅策略的 quota/reset 需要 POST；其余 /api 路径维持旧行为（404 文本）。
		if strings.HasPrefix(route, "/api/nodes/") && isPolicyPostRoute(route) {
			s.handleAPI(w, r, route)
			return
		}
		s.handlePost(w, r, route)
	case http.MethodPut:
		if strings.HasPrefix(route, "/api/") {
			s.handleAPI(w, r, route)
			return
		}
		s.sendText(w, r, http.StatusNotFound, "not found")
	default:
		s.sendText(w, r, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, route string) {
	switch route {
	case "/healthz":
		s.sendJSON(w, r, http.StatusOK, map[string]any{"ok": true})

	case "/", "/index.html":
		if !s.authorized(r) {
			s.serveAsset(w, r, "login.html", "text/html; charset=utf-8")
			return
		}
		data, err := assetBytes("index.html")
		if err != nil {
			s.sendText(w, r, http.StatusInternalServerError, "web assets missing")
			return
		}
		s.send(w, r, http.StatusOK, "text/html; charset=utf-8", data)

	case "/app.js":
		s.serveAsset(w, r, "app.js", "application/javascript; charset=utf-8")

	case "/login.js":
		s.serveAsset(w, r, "login.js", "application/javascript; charset=utf-8")

	case "/style.css":
		s.serveAsset(w, r, "style.css", "text/css; charset=utf-8")

	case "/login":
		// 兼容性修复：登录失败重定向到 /login?error=1 后，旧实现返回 404
		// 导致用户卡死（login.html 的错误提示逻辑从未生效）。这里改为
		// 渲染登录页，行为与前端代码的既有意图一致。详见 docs/AUDIT.md §6。
		s.serveAsset(w, r, "login.html", "text/html; charset=utf-8")

	default:
		s.sendText(w, r, http.StatusNotFound, "not found")
	}
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name, ctype string) {
	data, err := assetBytes(name)
	if err != nil {
		s.sendText(w, r, http.StatusNotFound, "not found")
		return
	}
	s.send(w, r, http.StatusOK, ctype, data)
}
