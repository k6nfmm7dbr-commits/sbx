package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx/internal/webui"
)

// assetBytes 从内嵌前端读取文件。
func assetBytes(name string) ([]byte, error) {
	f, err := webui.FS().Open(strings.TrimLeft(name, "/"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- 响应输出（对齐旧 _send/_json 的头与编码） ---------------------------

func (s *Server) send(w http.ResponseWriter, r *http.Request, code int, ctype string, body []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if r.Method != http.MethodHead && len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func (s *Server) sendText(w http.ResponseWriter, r *http.Request, code int, text string) {
	s.send(w, r, code, "text/plain; charset=utf-8", []byte(text))
}

func (s *Server) sendJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	data, err := fsx.MarshalCompact(v)
	if err != nil {
		slog.Error("JSON 序列化失败", "err", err)
		s.sendText(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.send(w, r, code, "application/json; charset=utf-8", data)
}

// ---- 鉴权 ---------------------------------------------------------------

func (s *Server) token() string { return s.cfg.Token }

// authorized 复刻旧 _authorized：Bearer 头 > ?token= > Cookie sbx_token；
// 服务端未配置 token 时完全开放；比较使用常量时间算法。
func (s *Server) authorized(r *http.Request) bool {
	token := s.token()
	if token == "" {
		return true
	}
	given := ""
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		given = auth[len("Bearer "):]
	}
	if given == "" {
		given = qsGet(r, "token")
	}
	if given == "" {
		given = cookieToken(r)
	}
	return constTimeEqual(given, token)
}

// cookieToken 提取 Cookie 中的 sbx_token 并做百分号解码（不把 + 当空格），
// 与 Python re.search + urllib.parse.unquote 的组合一致。
func cookieToken(r *http.Request) string {
	cookie := r.Header.Get("Cookie")
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "sbx_token=") {
			continue
		}
		raw := part[len("sbx_token="):]
		if raw == "" {
			return ""
		}
		if dec, err := nodes.PyUnquote(raw); err == nil {
			return dec
		}
		return raw
	}
	return ""
}

func constTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// 长度不同也要消耗比较时间，避免长度侧信道
		var sum byte
		for i := range a {
			sum |= a[i] ^ 0
		}
		_ = sum
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
