package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// qsGet 模拟 Python parse_qs 的行为：空值参数视为未提供。
func qsGet(r *http.Request, key string) string {
	vals := r.URL.Query()[key]
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// handleAPI 处理 /api/* 路由（统一鉴权）。
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request, route string) {
	if !s.authorized(r) {
		s.sendJSON(w, r, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	switch route {
	case "/api/summary":
		s.serveCachedJSON(w, r, codeSummaryFailed, s.cacheKey("summary"), func() (any, error) {
			sum, err := traffic.BuildSummary(s.cfg, s.db.DB, s.src)
			if err != nil {
				return nil, err
			}
			s.attachPolicyToSummary(sum)
			return sum, nil
		})

	case "/api/live":
		s.serveCachedJSON(w, r, codeLiveFailed, s.cacheKey("live"), func() (any, error) {
			live, err := traffic.BuildLive(s.cfg, s.db.DB, s.src)
			if err != nil {
				return nil, err
			}
			s.attachPolicyToLive(live)
			return live, nil
		})

	case "/api/events":
		s.handleEvents(w, r)

	case "/api/daily":
		daysRaw := qsGet(r, "days")
		days := 30
		if daysRaw != "" {
			n, err := strconv.Atoi(daysRaw)
			if err != nil {
				// 用户参数错误是 400，不是 500（内部错误）。
				s.sendJSON(w, r, http.StatusBadRequest,
					map[string]string{"error": "invalid literal for int(): " + strconv.Quote(daysRaw)})
				return
			}
			days = n
		}
		if days < 1 {
			days = 1
		}
		if days > 365 {
			days = 365
		}
		scope := qsGet(r, "scope")
		s.serveCachedJSON(w, r, codeDailyFailed,
			s.cacheKey("daily", strconv.Itoa(days), scope), func() (any, error) {
				rows, err := traffic.QDaily(s.db.DB, days, scope)
				if err != nil {
					return nil, err
				}
				if rows == nil {
					rows = []traffic.DailyRow{}
				}
				return map[string]any{"days": rows}, nil
			})

	case "/api/nodes":
		if r.Method == http.MethodPost {
			s.createNode(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		// 面板管理页需要严格节点列表；损坏时明确报错，不能表现为空列表。
		list, err := nodes.LoadPanelNodesStrict(s.cfg.NodesFile)
		if err != nil {
			s.failUnavailable(w, r, codeNodesFileUnavailable, "", "节点配置文件不可用", err)
			return
		}
		public := nodes.PublicNodes(list)
		if s.policy != nil {
			states, _ := s.policy.Snapshot()
			for i := range public {
				if st, ok := states[strconv.FormatInt(public[i].ID, 10)]; ok {
					public[i].Paused = st.Paused
					public[i].IPLimitOn = st.IPLimitOn
					public[i].IPLimitMax = st.IPLimitMax
					public[i].RateLimitOn = st.RateLimitOn
					public[i].RateLimitMbps = st.RateLimitMbps
				}
			}
		}
		s.sendJSON(w, r, http.StatusOK, map[string]any{"nodes": public})

	case "/api/export":
		s.handleExport(w, r)

	default:
		if handled := s.tryNodeAdminRoute(w, r, route); handled {
			return
		}
		// 策略子路由：/api/nodes/<id>/{policy|active-ips|ip-state}
		if handled := s.tryPolicyRoute(w, r, route); handled {
			return
		}
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// attachPolicyToSummary 把策略状态合并进 summary 的节点列表（供前端卡片展示）。
func (s *Server) attachPolicyToSummary(sum *traffic.Summary) {
	if s.policy == nil {
		return
	}
	// enforcement 错误（如 nft 应用失败）必须透出给面板，
	// 策略 API 校验 nodes.json 不可用时必须正确报告文件问题，而非误报节点不存在。
	sum.PolicyError = s.policy.LastError()
	states, _ := s.policy.Snapshot()
	for i := range sum.Nodes {
		id := strconv.FormatInt(toI64(sum.Nodes[i].ID), 10)
		st, ok := states[id]
		if !ok {
			continue
		}
		sum.Nodes[i].Paused = st.Paused
		sum.Nodes[i].IPLimitOn = st.IPLimitOn
		sum.Nodes[i].IPLimitMax = st.IPLimitMax
		sum.Nodes[i].ActiveIPs = st.ActiveIPs
		if st.IPLimitOn {
			sum.Nodes[i].IPLimitState = st.IPLimitState
		}
		sum.Nodes[i].RateLimitOn = st.RateLimitOn
		if st.RateLimitOn {
			sum.Nodes[i].RateLimitMbps = st.RateLimitMbps
		}
	}
}

func toI64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}

// attachPolicyToLive 把策略状态（在线 IP 数 + IP 限制）合并进 live 的节点列表。
// 连接数始终由 traffic.Collector 的 socket 统计结果提供；不能在这里用策略模块
// 的按 IP 聚合结果覆盖，否则同一 NAT IP 下的多条 TCP 会被错误压成 1。
func (s *Server) attachPolicyToLive(live *traffic.Live) {
	if s.policy == nil {
		return
	}
	states, _ := s.policy.Snapshot()
	for i := range live.Nodes {
		id := strconv.FormatInt(toI64(live.Nodes[i].ID), 10)
		if st, ok := states[id]; ok {
			live.Nodes[i].Paused = st.Paused
			live.Nodes[i].ActiveIPs = st.ActiveIPs
			live.Nodes[i].IPLimitOn = st.IPLimitOn
			live.Nodes[i].IPLimitMax = st.IPLimitMax
		}
	}
}
func (s *Server) tryPolicyRoute(w http.ResponseWriter, r *http.Request, route string) bool {
	prefix := "/api/nodes/"
	if !strings.HasPrefix(route, prefix) {
		return false
	}
	rest := route[len(prefix):]
	// rest 形如 "<id>/policy"、"<id>/active-ips" 或 "<id>/ip-state"
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return false
	}
	idStr := rest[:slash]
	sub := rest[slash+1:]
	if sub != "policy" && sub != "active-ips" && sub != "ip-state" {
		return false
	}
	s.handlePolicyAPI(w, r, idStr, sub)
	return true
}
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	// 流式导出：逐行从数据库读、逐行写响应，不在内存里攒整份 CSV。
	// daily 表无清理策略、随运行年限增长（50 节点 × 3 年 ≈ 5.5 万行），
	// 旧实现「全表读进 []exportRow + strings.Builder 拼完再写」在大库上有
	// 数 MB 到数十 MB 的瞬时内存尖峰；流式后驻留内存与单行等价。
	rows, err := s.db.QueryContext(r.Context(),
		"SELECT day,scope,rx,tx,rx_pkts,tx_pkts FROM daily ORDER BY day,scope")
	if err != nil {
		s.failInternal(w, r, codeExportFailed, err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Disposition", "attachment; filename=sbx-traffic.csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// 与 s.send() 的安全响应头保持一致（send() 面向整包 body，这里流式写出）。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	var buf [512]byte
	line := buf[:0]
	writeLine := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false // 客户端断开等：停止导出，错误随连接终止呈现
		}
		return true
	}
	line = append(line[:0], "day,scope,rx_bytes,tx_bytes,rx_pkts,tx_pkts\n"...)
	if !writeLine(line) {
		return
	}
	for rows.Next() {
		var e exportRow
		if err := rows.Scan(&e.day, &e.scope, &e.rx, &e.tx, &e.rxPkts, &e.txPkts); err != nil {
			// 响应头已发出，无法改写状态码：截断输出即"导出不完整"，
			// CSV 语义上比 500 更诚实（客户端拿到的就是残缺文件）。记录错误
			// 供运维诊断，避免数据库扫描故障静默表现为成功下载。
			slog.Warn("CSV 导出扫描中断", "error_code", codeExportFailed, "err", err)
			return
		}
		line = line[:0]
		line = append(line, e.day...)
		line = append(line, ',')
		line = append(line, e.scope...)
		line = append(line, ',')
		line = strconv.AppendInt(line, e.rx, 10)
		line = append(line, ',')
		line = strconv.AppendInt(line, e.tx, 10)
		line = append(line, ',')
		line = strconv.AppendInt(line, e.rxPkts, 10)
		line = append(line, ',')
		line = strconv.AppendInt(line, e.txPkts, 10)
		line = append(line, '\n')
		if !writeLine(line) {
			return
		}
	}
	if err := rows.Err(); err != nil {
		// QueryContext 会在请求断开时取消；客户端断开导致的取消无需再记成服务错误。
		if r.Context().Err() == nil {
			slog.Warn("CSV 导出查询中断", "error_code", codeExportFailed, "err", err)
		}
	}
}

type exportRow struct {
	day, scope string
	rx, tx     int64
	rxPkts     int64
	txPkts     int64
}
