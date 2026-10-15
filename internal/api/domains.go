package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// domainQueryMaxDays 是域名查询允许的最大回溯天数，与 traffic 包的域名保留期对齐
// （更早的日期必然没有数据）。
const domainQueryMaxDays = 180

// handleNodeDomains 返回某个节点在某段时间里访问过的域名及其流量，按流量倒序。
//
// 两个互斥的查询方式：`day=YYYY-MM-DD` 查指定的一天（面板日历用这个），
// `days=N` 查最近 N 天。
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
	limit, ok := clampQueryInt(r, "limit", 100, 1, 500)
	if !ok {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "limit 需在 1–500"})
		return
	}

	loc := traffic.Location(s.cfg.TZ)
	today := traffic.TodayAt(s.cfg.TZ, time.Now())
	from, to, days := today, today, 1
	// day 优先：面板日历选中某一天时带的就是它，语义是「就这一天」。
	// 不给 day 则退回原来的「最近 days 天」。
	if raw := strings.TrimSpace(qsGet(r, "day")); raw != "" {
		if t, perr := time.ParseInLocation("2006-01-02", raw, loc); perr != nil || t.Format("2006-01-02") != raw {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "day 需为 YYYY-MM-DD"})
			return
		}
		from, to = raw, raw
	} else {
		n, valid := clampQueryInt(r, "days", 1, 1, domainQueryMaxDays)
		if !valid {
			s.sendJSON(w, r, http.StatusBadRequest,
				map[string]string{"error": "days 需在 1–180"})
			return
		}
		days = n
		if n > 1 {
			if t, perr := time.ParseInLocation("2006-01-02", today, loc); perr == nil {
				from = t.AddDate(0, 0, -(n - 1)).Format("2006-01-02")
			}
		}
	}

	scope := "node:" + strconv.FormatInt(id, 10)
	rows, err := traffic.QTopDomains(s.db.DB, from, to, scope, limit)
	if err != nil {
		s.failInternal(w, r, codeSummaryFailed, err)
		return
	}
	total, err := traffic.QScopeTrafficRange(s.db.DB, from, to, scope)
	if err != nil {
		s.failInternal(w, r, codeSummaryFailed, err)
		return
	}
	classified, err := traffic.QDomainClassified(s.db.DB, from, to, scope)
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
		"to":           to,
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
