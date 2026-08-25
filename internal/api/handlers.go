package api

import (
	"database/sql"
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
		s.sendJSON(w, r, http.StatusOK, sum)

	case "/api/live":
		live, err := traffic.BuildLive(s.cfg, s.db.DB, s.src)
		if err != nil {
			s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
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
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleExport 输出全量 daily 的 CSV（与旧实现逐字节同构）。
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


