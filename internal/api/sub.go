package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// 订阅路由的设计约束（改动前请先读完）：
//
//  1. 订阅令牌与面板令牌**必须**是两把独立的锁。订阅 URL 一定要粘进
//     Shadowrocket / sing-box 这类第三方客户端，等于离开本机保管。
//     如果复用面板令牌，任何一次"把订阅地址发给朋友"都等于交出面板。
//  2. 订阅路由不走 Cookie 鉴权——客户端不会带 Cookie，只认 URL 里的令牌。
//  3. 令牌永远只出现在**路径**里，不接受 ?token=（会进客户端日志与 Referer）。
//  4. 令牌错误一律 404，不回 401：不告诉扫描者"这个路径存在、只是密码错了"。

// ensureSubToken 幂等地取到订阅令牌，为空时生成并落盘。
// 双检锁而非 sync.Once：首次生成失败（磁盘只读等）后还能重试，
// 而 sync.Once 会把失败永久固化到进程生命周期结束。
func (s *Server) ensureSubToken() (string, error) {
	s.subMu.RLock()
	tok := s.subToken
	s.subMu.RUnlock()
	if tok != "" {
		return tok, nil
	}

	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.subToken != "" {
		return s.subToken, nil
	}
	tok, err := config.EnsureSubToken()
	if err != nil {
		return "", err
	}
	s.subToken = tok
	return tok, nil
}

func (s *Server) setSubToken(tok string) {
	s.subMu.Lock()
	s.subToken = tok
	s.subMu.Unlock()
}

// handleSub 处理 GET /sub/<token>[?target=singbox]。
func (s *Server) handleSub(w http.ResponseWriter, r *http.Request, given string) {
	want, err := s.ensureSubToken()
	if err != nil || !tokenEqual(given, want) {
		// 统一 404：既覆盖"令牌错"，也覆盖"尚未生成"。
		s.sendText(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.apiLimiter.allowRequest(r, time.Now()) {
		s.sendText(w, r, http.StatusTooManyRequests, "too many requests")
		return
	}

	list, err := nodes.LoadPanelNodesRepair(s.cfg.NodesFile)
	if err != nil {
		s.sendText(w, r, http.StatusServiceUnavailable, "nodes file unavailable")
		return
	}

	target := strings.ToLower(qsGet(r, "target"))
	switch target {
	case "", "base64", "uri":
		store := s.nodeStore()
		links := nodes.BuildSubscriptionURIs(store, list)
		if len(links) == 0 {
			s.sendText(w, r, http.StatusNotFound, "no nodes")
			return
		}
		s.send(w, r, http.StatusOK, "text/plain; charset=utf-8",
			[]byte(nodes.EncodeSubscription(links)))

	case "singbox", "sing-box":
		store := s.nodeStore()
		profile, skipped, perr := nodes.BuildSingboxProfile(store, list, store.ShareHost())
		if perr != nil {
			s.sendText(w, r, http.StatusConflict, perr.Error())
			return
		}
		if len(skipped) > 0 {
			// 头部只放数量：中文说明不适合进 HTTP 头（会被各种代理截断/改写），
			// 细节由面板的 /api/sub 返回给用户看。
			w.Header().Set("X-Sbx-Skipped-Nodes", strconv.Itoa(len(skipped)))
		}
		s.sendJSON(w, r, http.StatusOK, profile)

	default:
		s.sendText(w, r, http.StatusBadRequest, "unknown target")
	}
}

// handleSubInfo 返回面板内展示用的订阅信息（走 Cookie 鉴权）。
func (s *Server) handleSubInfo(w http.ResponseWriter, r *http.Request) {
	tok, err := s.ensureSubToken()
	if err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError,
			map[string]string{"error": "生成订阅令牌失败: " + err.Error()})
		return
	}

	base := "http://" + r.Host
	if s.cfg.SecureCookie {
		base = "https://" + r.Host
	}

	list, _ := nodes.LoadPanelNodesRepair(s.cfg.NodesFile)
	store := s.nodeStore()
	profile, skipped, perr := nodes.BuildSingboxProfile(store, list, store.ShareHost())

	resp := map[string]any{
		"url":         base + "/sub/" + tok,
		"singbox_url": base + "/sub/" + tok + "?target=singbox",
		"token":       tok,
		"node_count":  len(list),
		"share_host":  store.ShareHost(),
		"skipped":     skipped,
	}
	if perr != nil {
		resp["singbox_error"] = perr.Error()
	} else {
		ob, _ := profile["outbounds"].([]any)
		// 末尾两个是 selector 与 direct，节点数要扣掉。
		if len(ob) >= 2 {
			resp["exportable"] = len(ob) - 2
		}
	}
	s.sendJSON(w, r, http.StatusOK, resp)
}

// handleSubReset 轮换订阅令牌（POST /api/sub/reset）。
func (s *Server) handleSubReset(w http.ResponseWriter, r *http.Request) {
	tok, err := config.ResetSubToken()
	if err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError,
			map[string]string{"error": "重置订阅令牌失败: " + err.Error()})
		return
	}
	s.setSubToken(tok)
	s.handleSubInfo(w, r)
}
