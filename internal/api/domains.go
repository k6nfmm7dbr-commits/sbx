package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// handleNodeDomains 返回某个节点在最近 N 天里访问过的域名及其流量，按流量倒序。
//
// 数据来自 sing-box 的连接表（每 2 秒采样一次），因此有一个必须如实告知的
// 特性：只统计活过采样间隔的连接。响应里的 unclassified 就是「节点总流量减去
// 已归类部分」，让调用方（面板）能把这个偏差显示出来——否则用户会误以为
// 列表里的数字就是全部流量。
func (s *Server) handleNodeDomains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, err := strconv.ParseInt(qsGet(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "节点 id 无效"})
		return
	}
	days, ok := clampQueryInt(r, "days", 1, 1, 30)
	if !ok {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "days 需在 1–30"})
		return
	}
	limit, ok := clampQueryInt(r, "limit", 100, 1, 500)
	if !ok {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "limit 需在 1–500"})
		return
	}

	scope := "node:" + strconv.FormatInt(id, 10)
	today := traffic.TodayAt(s.cfg.TZ, time.Now())
	from := today
	if days > 1 {
		if t, perr := time.ParseInLocation("2006-01-02", today, traffic.Location(s.cfg.TZ)); perr == nil {
			from = t.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
		}
	}

	rows, err := traffic.QTopDomains(s.db.DB, from, scope, limit)
	if err != nil {
		s.failInternal(w, r, codeSummaryFailed, err)
		return
	}
	total, err := traffic.QScopeTrafficRange(s.db.DB, from, scope)
	if err != nil {
		s.failInternal(w, r, codeSummaryFailed, err)
		return
	}
	classified, err := traffic.QDomainClassified(s.db.DB, from, scope)
	if err != nil {
		s.failInternal(w, r, codeSummaryFailed, err)
		return
	}
	unclassified := total - classified
	if unclassified < 0 {
		// 理论上不会出现（采样量不应超过计数器口径）；真出现说明数据异常，
		// 夹到 0 而不是展示负数。
		unclassified = 0
	}
	s.sendJSON(w, r, http.StatusOK, map[string]any{
		"scope":        scope,
		"from":         from,
		"days":         days,
		"total":        total,
		"classified":   classified,
		"unclassified": unclassified,
		"domains":      rows,
	})
}

// clampQueryInt 读取整型查询参数并做范围校验；缺省返回 def。
func clampQueryInt(r *http.Request, key string, def, lo, hi int) (int, bool) {
	raw := qsGet(r, key)
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}
