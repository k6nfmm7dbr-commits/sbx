package database

import (
	"path/filepath"
	"strings"
	"testing"
)

// domain_daily 的 (scope, day) 索引是单节点域名查询的性能关键：
// 没有它，查询只能沿 (day, ...) 主键扫窗口内全部节点的行。
// 用 EXPLAIN QUERY PLAN 钉住，防止日后误删。
func TestDomainDailyScopeDayIndexUsed(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO domain_daily(day,scope,domain,rx,tx,hits)
		VALUES('2026-10-15','node:1','a.example.com',1,2,1)`); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query("EXPLAIN QUERY PLAN " +
		"SELECT domain, SUM(rx+tx) AS total FROM domain_daily " +
		"WHERE day >= '2026-10-01' AND day <= '2026-10-15' AND scope='node:1' " +
		"GROUP BY domain ORDER BY total DESC LIMIT 100")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var a, b, c, d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plan += d + "\n"
	}
	if !strings.Contains(plan, "idx_domain_daily_scope_day") {
		t.Fatalf("查询计划未使用 (scope,day) 索引:\n%s", plan)
	}
}
