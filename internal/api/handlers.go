package api

import (
	"database/sql"
	"encoding/json"
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
		sum, err := traffic.BuildSummary(s.cfg, s.db.DB, s.src)
		if err != nil {
			s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.attachPolicyToSummary(sum)
		s.sendJSON(w, r, http.StatusOK, sum)

	case "/api/live":
		live, err := traffic.BuildLive(s.cfg, s.db.DB, s.src)
		if err != nil {
			s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.attachPolicyToLive(live)
		s.sendJSON(w, r, http.StatusOK, live)

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
		rows, err := traffic.QDaily(s.db.DB, days, scope)
		if err != nil {
			s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if rows == nil {
			rows = []traffic.DailyRow{}
		}
		s.sendJSON(w, r, http.StatusOK, map[string]any{"days": rows})

	case "/api/nodes":
		// 只返回脱敏后的 PublicNodeDTO，绝不下发 password/uuid/private_key 等。
		list := nodes.LoadPanelNodes(s.cfg.NodesFile)
		s.sendJSON(w, r, http.StatusOK, map[string]any{"nodes": nodes.PublicNodes(list)})

	case "/api/export":
		s.handleExport(w, r)

	default:
		// 策略子路由：/api/nodes/<id>/{policy|quota/reset|active-ips}
		if handled := s.tryPolicyRoute(w, r, route); handled {
			return
		}
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// isPolicyPostRoute 判断路由是否是需要 POST 的策略子路由（quota/reset）。
func isPolicyPostRoute(route string) bool {
	return strings.HasSuffix(route, "/quota/reset")
}

// attachPolicyToSummary 把策略状态合并进 summary 的节点列表（供前端卡片展示）。
func (s *Server) attachPolicyToSummary(sum *traffic.Summary) {
	if s.policy == nil {
		return
	}
	states, _ := s.policy.Snapshot()
	for i := range sum.Nodes {
		id := strconv.FormatInt(toI64(sum.Nodes[i].ID), 10)
		st, ok := states[id]
		if !ok {
			continue
		}
		sum.Nodes[i].QuotaEnabled = st.QuotaEnabled
		sum.Nodes[i].QuotaLimit = st.QuotaLimit
		sum.Nodes[i].QuotaUsed = st.QuotaUsed
		// 未启用时 state 留空（配合 omitempty 不输出），启用时才有 ok/exceeded。
		if st.QuotaEnabled {
			sum.Nodes[i].QuotaState = st.QuotaState
		}
		sum.Nodes[i].IPLimitOn = st.IPLimitOn
		sum.Nodes[i].IPLimitMax = st.IPLimitMax
		sum.Nodes[i].ActiveIPs = st.ActiveIPs
		if st.IPLimitOn {
			sum.Nodes[i].IPLimitState = st.IPLimitState
		}
		// 定时重置：开启时下发周期/时刻/下次时间（供节点卡片倒计时）。
		if st.ResetEnabled {
			sum.Nodes[i].ResetEnabled = true
			sum.Nodes[i].ResetPeriod = st.ResetPeriod
			sum.Nodes[i].ResetTime = st.ResetTime
			sum.Nodes[i].ResetNextAt = st.ResetNextAt
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

// attachPolicyToLive 把策略状态（在线 IP 数 + IP 限制）合并进 live 的节点列表，
// 供前端 2s 高频轮询刷新节点卡片的「在线 IP」显示。
func (s *Server) attachPolicyToLive(live *traffic.Live) {
	if s.policy == nil {
		return
	}
	states, _ := s.policy.Snapshot()
	for i := range live.Nodes {
		id := strconv.FormatInt(toI64(live.Nodes[i].ID), 10)
		if st, ok := states[id]; ok {
			live.Nodes[i].ActiveIPs = st.ActiveIPs
			live.Nodes[i].IPLimitOn = st.IPLimitOn
			live.Nodes[i].IPLimitMax = st.IPLimitMax
			if st.ResetEnabled {
				live.Nodes[i].ResetEnabled = true
				live.Nodes[i].ResetNextAt = st.ResetNextAt
			}
		}
	}
}
func (s *Server) tryPolicyRoute(w http.ResponseWriter, r *http.Request, route string) bool {
	prefix := "/api/nodes/"
	if !strings.HasPrefix(route, prefix) {
		return false
	}
	rest := route[len(prefix):]
	// rest 形如 "<id>/policy" 或 "<id>/quota/reset" 或 "<id>/active-ips"
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return false
	}
	idStr := rest[:slash]
	sub := rest[slash+1:]
	if sub != "policy" && sub != "quota/reset" && sub != "active-ips" {
		return false
	}
	s.handlePolicyAPI(w, r, idStr, sub)
	return true
}
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	rows, err := queryExportRows(s.db.DB)
	if err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var b strings.Builder
	b.WriteString("day,scope,rx_bytes,tx_bytes,rx_pkts,tx_pkts\n")
	for _, row := range rows {
		b.WriteString(row.day)
		b.WriteByte(',')
		b.WriteString(row.scope)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(row.rx, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(row.tx, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(row.rxPkts, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(row.txPkts, 10))
		b.WriteByte('\n')
	}
	w.Header().Set("Content-Disposition", "attachment; filename=sbx-traffic.csv")
	s.send(w, r, http.StatusOK, "text/csv; charset=utf-8", []byte(b.String()))
}

type exportRow struct {
	day, scope string
	rx, tx     int64
	rxPkts     int64
	txPkts     int64
}

func queryExportRows(db *sql.DB) ([]exportRow, error) {
	rows, err := db.Query(
		"SELECT day,scope,rx,tx,rx_pkts,tx_pkts FROM daily ORDER BY day,scope")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []exportRow{}
	for rows.Next() {
		var e exportRow
		if err := rows.Scan(&e.day, &e.scope, &e.rx, &e.tx, &e.rxPkts, &e.txPkts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
