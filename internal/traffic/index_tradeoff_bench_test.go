package traffic

import (
	"fmt"
	"testing"
)

// 索引方案对比：写路径（commitTick 的真实事务内 upsert）vs 读路径。
// idx1 = (day,scope,rx,tx,rx_pkts,tx_pkts)   服务全节点趋势聚合
// idx2 = (scope,day,rx,tx,rx_pkts,tx_pkts)   服务单节点趋势（scope 等值）
func BenchmarkIndexTradeoff(b *testing.B) {
	type variant struct {
		name    string
		indexes []string
	}
	variants := []variant{
		{"现状_无覆盖索引", nil},
		{"仅idx1_day先", []string{"CREATE INDEX i1 ON daily(day,scope,rx,tx,rx_pkts,tx_pkts)"}},
		{"仅idx2_scope先", []string{"CREATE INDEX i2 ON daily(scope,day,rx,tx,rx_pkts,tx_pkts)"}},
		{"双索引", []string{
			"CREATE INDEX i1 ON daily(day,scope,rx,tx,rx_pkts,tx_pkts)",
			"CREATE INDEX i2 ON daily(scope,day,rx,tx,rx_pkts,tx_pkts)",
		}},
	}
	const qAll = "SELECT day, SUM(rx) rx, SUM(tx) tx, SUM(rx_pkts) rx_pkts, SUM(tx_pkts) tx_pkts " +
		"FROM daily WHERE scope LIKE 'node:%' GROUP BY day ORDER BY day DESC LIMIT 180"
	const qOne = "SELECT day,rx,tx,rx_pkts,tx_pkts FROM daily WHERE scope=? ORDER BY day DESC LIMIT 180"

	for _, v := range variants {
		b.Run(v.name+"/写", func(b *testing.B) {
			db := benchDailyDB(b, 30, 1)
			for _, idx := range v.indexes {
				if _, err := db.Exec(idx); err != nil {
					b.Fatal(err)
				}
			}
			day := TodayAt("Asia/Shanghai", TimeNow())
			ts := TimeNow().Unix()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				tx, err := db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				stD, _ := tx.Prepare(sqlUpsertDaily)
				stT, _ := tx.Prepare(sqlUpsertTotals)
				stS, _ := tx.Prepare(sqlUpsertSample)
				for s := 0; s < 50; s++ {
					scope := fmt.Sprintf("node:%d", s+1)
					stD.Exec(day, scope, 100, 200, 3, 4)
					stT.Exec(scope, 100, 200, 3, 4)
					stS.Exec(ts, scope, 100, 200, 2000)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
				stD.Close()
				stT.Close()
				stS.Close()
			}
		})
		b.Run(v.name+"/读_全节点趋势", func(b *testing.B) {
			db := benchDailyDB(b, 1095, 50)
			for _, idx := range v.indexes {
				if _, err := db.Exec(idx); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := db.Query(qAll)
				if err != nil {
					b.Fatal(err)
				}
				n := 0
				for rows.Next() {
					var d string
					var a, c, e, f int64
					if err := rows.Scan(&d, &a, &c, &e, &f); err != nil {
						b.Fatal(err)
					}
					n++
				}
				rows.Close()
				if n == 0 {
					b.Fatal("no rows")
				}
			}
		})
		b.Run(v.name+"/读_单节点趋势", func(b *testing.B) {
			db := benchDailyDB(b, 1095, 50)
			for _, idx := range v.indexes {
				if _, err := db.Exec(idx); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := db.Query(qOne, "node:7")
				if err != nil {
					b.Fatal(err)
				}
				n := 0
				for rows.Next() {
					var d string
					var a, c, e, f int64
					if err := rows.Scan(&d, &a, &c, &e, &f); err != nil {
						b.Fatal(err)
					}
					n++
				}
				rows.Close()
				if n == 0 {
					b.Fatal("no rows")
				}
			}
		})
	}
}
