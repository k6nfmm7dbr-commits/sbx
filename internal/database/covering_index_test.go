package database

import (
	"strings"
	"testing"
)

// 覆盖索引回归：趋势查询的计划必须走覆盖索引（不回表），这是 v3.0.12
// 的性能优化点（真机实测：全节点趋势 4.1ms→1.9ms，单节点趋势 0.83ms→0.14ms，
// 写路径无可测回归）。索引由 Open 时的迁移逻辑自动补建，存量库同样生效。
func TestDailyCoveringIndexes(t *testing.T) {
	db, err := Open(t.TempDir() + "/idx.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 模拟 30 天 × 5 scope 的数据
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for d := 0; d < 30; d++ {
		for s := 0; s < 5; s++ {
			scope := "system"
			if s > 0 {
				scope = "node:" + itoa(s)
			}
			if _, err := tx.Exec(
				"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)",
				dayStr(2026, 1, d+1), scope, d*100, d*200, 10, 20); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, query, wantIdx string
		args                 []any
	}{
		{
			name: "全节点趋势聚合必须用 day 覆盖索引",
			query: "SELECT day, SUM(rx) rx, SUM(tx) tx, SUM(rx_pkts) rx_pkts, SUM(tx_pkts) tx_pkts " +
				"FROM daily WHERE scope LIKE 'node:%' GROUP BY day ORDER BY day DESC LIMIT 180",
			wantIdx: "idx_daily_day_vals",
		},
		{
			name:    "单节点趋势必须用 scope 覆盖索引",
			query:   "SELECT day,rx,tx,rx_pkts,tx_pkts FROM daily WHERE scope=? ORDER BY day DESC LIMIT 180",
			args:    []any{"node:3"},
			wantIdx: "idx_daily_scope_vals",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, err := db.Query("EXPLAIN QUERY PLAN "+c.query, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var a, b, d int
				var detail string
				if err := rows.Scan(&a, &b, &d, &detail); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(detail)
				plan.WriteString("\n")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			got := plan.String()
			if !strings.Contains(got, "USING COVERING INDEX "+c.wantIdx) {
				t.Fatalf("查询计划未使用覆盖索引 %s:\n%s", c.wantIdx, got)
			}
			if strings.Contains(got, "SCAN daily USING INDEX sqlite_autoindex") {
				t.Fatalf("查询计划回退到主键索引(会回表):\n%s", got)
			}
		})
	}
}

// 索引补建对存量库幂等：重复 Open 不报错、不重复建。
func TestDailyCoveringIndexesIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		db, err := Open(dir + "/reopen.db")
		if err != nil {
			t.Fatalf("第 %d 次 Open 失败: %v", i+1, err)
		}
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN " +
				"('idx_daily_day_vals','idx_daily_scope_vals')").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("第 %d 次 Open 后覆盖索引数=%d, want 2", i+1, n)
		}
		db.Close()
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func dayStr(y, m, d int) string {
	return itoa(y) + "-" + pad2(m) + "-" + pad2(d)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
