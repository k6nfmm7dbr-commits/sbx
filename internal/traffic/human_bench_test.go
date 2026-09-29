package traffic

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// benchDailyDB 造一张"长期运行"的 daily 表：days 天 × scopes 个 scope。
// 真实场景：单节点 VPS 跑一年 ≈ 365 行；50 节点跑 3 年 ≈ 5 万行。
func benchDailyDB(tb testing.TB, days, scopes int) *database.DB {
	tb.Helper()
	db, err := database.Open(tb.TempDir() + "/bench.db")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })

	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	d0 := TodayAt("Asia/Shanghai", TimeNow())
	rows := make([][]any, 0, days*scopes)
	for d := 0; d < days; d++ {
		day := shiftDay(d0, -d)
		for s := 0; s < scopes; s++ {
			scope := fmt.Sprintf("node:%d", s+1)
			if s == 0 {
				scope = "system"
			}
			rows = append(rows, []any{day, scope, int64(d * 1000), int64(d * 2000), 10, 20})
		}
	}
	st, err := tx.Prepare("INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)")
	if err != nil {
		tb.Fatal(err)
	}
	for _, r := range rows {
		if _, err := st.Exec(r...); err != nil {
			tb.Fatal(err)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return db
}

func shiftDay(day string, delta int) string {
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, delta).Format("2006-01-02")
}

// BenchmarkQDailyAllScope 面板 180 天趋势（scope 为空 → 全节点聚合）。
func BenchmarkQDailyAllScope(b *testing.B) {
	for _, days := range []int{90, 180, 365, 1095} {
		for _, scopes := range []int{2, 10, 50} {
			b.Run(fmt.Sprintf("days=%d/scopes=%d", days, scopes), func(b *testing.B) {
				db := benchDailyDB(b, days, scopes)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rows, err := QDaily(db.DB, 180, "")
					if err != nil || len(rows) == 0 {
						b.Fatalf("err=%v rows=%d", err, len(rows))
					}
				}
			})
		}
	}
}

// BenchmarkQDailyOneScope 单节点趋势（scope 指定）。
func BenchmarkQDailyOneScope(b *testing.B) {
	db := benchDailyDB(b, 1095, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := QDaily(db.DB, 180, "node:7")
		if err != nil || len(rows) == 0 {
			b.Fatalf("err=%v rows=%d", err, len(rows))
		}
	}
}

var _ = sql.ErrNoRows
