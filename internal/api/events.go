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

	// last 缓存已推送内容的序列化结果：既做去重判据，又直接复用为发送 payload。
	// payload 本身来自 ssePayloads() 的跨连接共享缓存（同一策略版本的所有连接
	// 共用一次序列化结果），本连接的 last 只负责「这个节点我这轮推过没有」。
	last := make(map[string]string, len(snap))
	{
		payloads, _ := s.ssePayloads()
		for id, pl := range payloads {
			last[id] = pl
		}
	}

	notify, unsub := s.policy.Subscribe()
	defer unsub()
	// 只靠 notify 唤醒（reconcile 每轮都会向所有订阅者扇出 signal），
	// 无状态变化时不再做任何全量快照与序列化。fallbackTick 仅作保险，
	// 万一 notify 通道因故未触发也能在 5s 内自愈。
	fallback := time.NewTicker(5 * time.Second)
	defer fallback.Stop()
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
		case <-fallback.C:
		}

		payloads, _ := s.ssePayloads()
		for id, payload := range payloads {
			if payload == "" || last[id] == payload {
				continue
			}
			last[id] = payload
			if !writeSSE(w, flusher, "node", []byte(payload)) {
				return // 客户端断开
			}
		}
		// 已删除节点：清掉缓存，避免残留。
		for id := range last {
			if _, ok := payloads[id]; !ok {
				delete(last, id)
			}
		}
	}
}

// marshalSnap 序列化节点快照；失败返回空串（调用方跳过该节点）。
//
// 测试可替换 marshalSnapFn 统计调用次数（验证跨连接共享缓存生效）。
var marshalSnapFn = marshalSnap

func marshalSnap(ns policy.NodeIPSnapshot) string {
	b, err := json.Marshal(ns)
	if err != nil {
		return ""
	}
	return string(b)
}

// ssePayloads 返回当前策略版本对应的「nodeID → 序列化 payload」缓存。
//
// 为什么需要它：reconcile 每秒发布一次新快照并向所有 SSE 订阅者广播，旧实现里
// **每个连接**各自调用 IPStateSnapshot() 并对**每个节点**做一次完整 JSON
// marshal（哪怕内容没变，也要先序列化才能做去重比较）。序列化成本随
// 「节点数 × 在线 IP 数 × SSE 连接数」相乘增长——50 节点 × 5 个浏览器标签页
// 时，每秒要白做 4 份一模一样的全量 marshal。改为按 policy.Version() 缓存
// 一份：同一版本的所有连接共享同一份 payload，序列化只发生一次。
//
// 正确性依据：Version() 在每次 reconcile 发布新快照时单调自增（mu 保护），
// 版本不变则快照必然不变——缓存不可能返回过期内容。
func (s *Server) ssePayloads() (map[string]string, uint64) {
	ver := s.policy.Version()
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	if s.sseVer == ver && s.sseCache != nil {
		return s.sseCache, ver
	}
	snap := s.policy.IPStateSnapshot()
	payloads := make(map[string]string, len(snap))
	for id, ns := range snap {
		payloads[id] = marshalSnapFn(ns)
	}
	s.sseVer = ver
	s.sseCache = payloads
	return payloads, ver
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
