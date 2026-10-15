package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

func mustExec(t *testing.T, srv *Server, query string, args ...any) {
	t.Helper()
	if _, err := srv.db.DB.Exec(query, args...); err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
}

func TestNodeDomainsEndpoint(t *testing.T) {
	ts, _, _, _, srv := newNodeCRUDTestServer(t)
	today := traffic.TodayAt(srv.cfg.TZ, time.Now())

	// 节点 1：总流量 3000（nftables 口径），其中 1100 归类到两个域名
	mustExec(t, srv, `INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,0,0)`,
		today, "node:1", 1000, 2000)
	mustExec(t, srv, `INSERT INTO domain_daily(day,scope,domain,rx,tx,hits) VALUES(?,?,?,?,?,?)`,
		today, "node:1", "a.com", 300, 700, 5)
	mustExec(t, srv, `INSERT INTO domain_daily(day,scope,domain,rx,tx,hits) VALUES(?,?,?,?,?,?)`,
		today, "node:1", "b.com", 50, 50, 2)
	// 另一个节点的数据绝不能混进来
	mustExec(t, srv, `INSERT INTO domain_daily(day,scope,domain,rx,tx,hits) VALUES(?,?,?,?,?,?)`,
		today, "node:2", "c.com", 999, 999, 9)

	code, body := doJSON(t, ts, http.MethodGet, "/api/node-domains?id=1", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %#v", code, body)
	}
	if body["scope"] != "node:1" || body["from"] != today {
		t.Errorf("scope/from 不对: %#v", body)
	}
	if got := num(t, body["total"]); got != 3000 {
		t.Errorf("total 应为 3000，实得 %v", got)
	}
	if got := num(t, body["classified"]); got != 1100 {
		t.Errorf("classified 应为 1100，实得 %v", got)
	}
	if got := num(t, body["unclassified"]); got != 1900 {
		t.Errorf("unclassified 应为 1900，实得 %v", got)
	}

	rows, _ := body["domains"].([]any)
	if len(rows) != 2 {
		t.Fatalf("应返回 2 个域名，实得 %d: %#v", len(rows), body["domains"])
	}
	first, _ := rows[0].(map[string]any)
	if first["domain"] != "a.com" {
		t.Errorf("应按流量倒序，首位应是 a.com，实得 %#v", first)
	}
	if num(t, first["total"]) != 1000 || num(t, first["hits"]) != 5 {
		t.Errorf("a.com 的流量/次数不对: %#v", first)
	}
	for _, raw := range rows {
		if m, _ := raw.(map[string]any); m["domain"] == "c.com" {
			t.Error("不应包含其它节点的域名")
		}
	}
}

// 没有域名数据时也要返回可用的结构（前端直接遍历），且未归类等于总流量。
func TestNodeDomainsEndpointWithoutDomainData(t *testing.T) {
	ts, _, _, _, srv := newNodeCRUDTestServer(t)
	today := traffic.TodayAt(srv.cfg.TZ, time.Now())
	mustExec(t, srv, `INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,0,0)`,
		today, "node:1", 500, 500)

	code, body := doJSON(t, ts, http.MethodGet, "/api/node-domains?id=1", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d", code)
	}
	if rows, ok := body["domains"].([]any); !ok || len(rows) != 0 {
		t.Errorf("应返回空数组，实得 %#v", body["domains"])
	}
	if got := num(t, body["unclassified"]); got != 1000 {
		t.Errorf("没有域名数据时未归类应等于总流量，实得 %v", got)
	}
}

func TestNodeDomainsEndpointValidatesParams(t *testing.T) {
	ts, _, _, _, _ := newNodeCRUDTestServer(t)
	cases := []string{
		"/api/node-domains",
		"/api/node-domains?id=0",
		"/api/node-domains?id=-1",
		"/api/node-domains?id=abc",
		"/api/node-domains?id=1&days=0",
		// 上限与域名保留期对齐（180 天）；31 天曾经是非法值，现在合法，别写回去
		"/api/node-domains?id=1&days=181",
		"/api/node-domains?id=1&limit=0",
		"/api/node-domains?id=1&limit=9999",
		// day 必须是规范日期
		"/api/node-domains?id=1&day=2026-1-2",
		"/api/node-domains?id=1&day=not-a-date",
		"/api/node-domains?id=1&day=2026-13-01",
	}
	for _, path := range cases {
		if code, _ := doJSON(t, ts, http.MethodGet, path, ""); code != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实得 %d", path, code)
		}
	}
}

// days 大于 1 时应聚合区间，且区间起点回推正确。
func TestNodeDomainsEndpointRange(t *testing.T) {
	ts, _, _, _, srv := newNodeCRUDTestServer(t)
	today := traffic.TodayAt(srv.cfg.TZ, time.Now())
	yesterday := time.Now().In(traffic.Location(srv.cfg.TZ)).AddDate(0, 0, -1).Format("2006-01-02")

	mustExec(t, srv, `INSERT INTO domain_daily(day,scope,domain,rx,tx,hits) VALUES(?,?,?,?,?,?)`,
		today, "node:1", "a.com", 100, 100, 1)
	mustExec(t, srv, `INSERT INTO domain_daily(day,scope,domain,rx,tx,hits) VALUES(?,?,?,?,?,?)`,
		yesterday, "node:1", "a.com", 900, 900, 9)

	code, body := doJSON(t, ts, http.MethodGet, "/api/node-domains?id=1&days=2", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d", code)
	}
	if body["from"] != yesterday {
		t.Errorf("days=2 的起点应是昨天(%s)，实得 %v", yesterday, body["from"])
	}
	rows, _ := body["domains"].([]any)
	if len(rows) != 1 {
		t.Fatalf("同一域名应被聚合，实得 %#v", rows)
	}
	row, _ := rows[0].(map[string]any)
	if num(t, row["total"]) != 2000 || num(t, row["hits"]) != 10 {
		t.Errorf("应累加区间内数据: %#v", row)
	}

	// days=1 时只应看到今天的部分
	_, todayOnly := doJSON(t, ts, http.MethodGet, "/api/node-domains?id=1&days=1", "")
	rows2, _ := todayOnly["domains"].([]any)
	if len(rows2) != 1 {
		t.Fatalf("实得 %#v", rows2)
	}
	if got := num(t, rows2[0].(map[string]any)["total"]); got != 200 {
		t.Errorf("days=1 应只有今天的 200，实得 %v", got)
	}
}

// num 把 JSON 解出的数字统一成 int64（可能是 float64 或 json.Number）。
func num(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case nil:
		return 0
	default:
		t.Fatalf("不是数字: %#v", v)
		return 0
	}
}

var _ = fmt.Sprintf
