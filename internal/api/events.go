package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
)

// handleEvents 提供 Server-Sent Events：单向实时推送节点在线 IP 状态。
// 鉴权复用现有 authorized（Bearer / HttpOnly Cookie），绝不接受 ?token=。
// 首次连接立即下发完整 snapshot；之后仅推送发生变化的节点。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.policy == nil {
		s.sendJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "policy 未初始化"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "streaming 不支持"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 清除 http.Server 的 WriteTimeout 对长连接写的截止时间，避免 SSE 60s 被掐断。
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	// 首包完整 snapshot。
	snap := s.policy.IPStateSnapshot()
	nodes := make([]policy.NodeIPSnapshot, 0, len(snap))
	for _, ns := range snap {
		nodes = append(nodes, ns)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	connectPayload, _ := json.Marshal(map[string]any{"type": "snapshot", "nodes": nodes})
	if !writeSSE(w, flusher, "snapshot", connectPayload) {
		return
	}

	last := map[string]string{}
	for id, ns := range snap {
		last[id] = nodeSnapHash(ns)
	}

	notify := s.policy.Notify()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !writeSSE(w, flusher, "ping", nil) {
				return
			}
			continue
		case <-notify:
		case <-ticker.C:
		}

		cur := s.policy.IPStateSnapshot()
		for id, ns := range cur {
			h := nodeSnapHash(ns)
			if last[id] == h {
				continue
			}
			last[id] = h
			p, err := json.Marshal(ns)
			if err != nil {
				continue
			}
			if !writeSSE(w, flusher, "node", p) {
				return // 客户端断开
			}
		}
		// 已删除节点：清掉缓存，避免残留。
		for id := range last {
			if _, ok := cur[id]; !ok {
				delete(last, id)
			}
		}
	}
}

// writeSSE 写一条 SSE 事件并 flush。返回 false 表示写入失败（客户端已断开）。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data []byte) bool {
	if data == nil {
		// heartbeat：只写注释行
		if _, err := w.Write([]byte(": ping\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if _, err := w.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return false
	}
	if _, err := w.Write(data); err != nil {
		return false
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// nodeSnapHash 生成节点快照的稳定字符串（用于去重，避免重复推送不变状态）。
func nodeSnapHash(ns policy.NodeIPSnapshot) string {
	b, _ := json.Marshal(ns)
	return string(b)
}
