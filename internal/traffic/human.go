package traffic

import (
	"database/sql"
	"fmt"
	"math"
)

// Human 以 1024 进制输出 "%.2f KiB" 风格（与旧 human() 一致）。
func Human(n int64) string {
	f := float64(n)
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"} {
		if math.Abs(f) < 1024 || unit == "PiB" {
			return fmt.Sprintf("%.2f %s", f, unit)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.2f PiB", f)
}

// DailyRow 对齐 daily 表行 / q_daily 返回结构。
type DailyRow struct {
	Day    string `json:"day"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
	RxPkts int64  `json:"rx_pkts"`
	TxPkts int64  `json:"tx_pkts"`
}

// TotalsRow 对齐 totals 表行。
type TotalsRow struct {
	Scope  string
	Rx     int64
	Tx     int64
	RxPkts int64
	TxPkts int64
}

// Rate 是字节/秒速率。
type Rate struct {
	Rx float64 `json:"rx"`
	Tx float64 `json:"tx"`
}

// QDaily 查询每日流量；scope 为空时聚合全部 node:* scope。
// 结果按日期升序（旧实现 DESC LIMIT 后反转）。
func QDaily(db *sql.DB, days int, scope string) ([]DailyRow, error) {
	var rows *sql.Rows
	var err error
	if scope != "" {
		rows, err = db.Query(
			"SELECT day,rx,tx,rx_pkts,tx_pkts FROM daily WHERE scope=? ORDER BY day DESC LIMIT ?",
			scope, days)
	} else {
		rows, err = db.Query(
			"SELECT day, SUM(rx) rx, SUM(tx) tx, SUM(rx_pkts) rx_pkts, SUM(tx_pkts) tx_pkts "+
				"FROM daily WHERE scope LIKE 'node:%' GROUP BY day ORDER BY day DESC LIMIT ?",
			days)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DailyRow{}
	for rows.Next() {
		var r DailyRow
		if err := rows.Scan(&r.Day, &r.Rx, &r.Tx, &r.RxPkts, &r.TxPkts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转：旧->新
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// QTotals 返回 {scope: row}。
func QTotals(db *sql.DB) (map[string]TotalsRow, error) {
	rows, err := db.Query("SELECT scope,rx,tx,rx_pkts,tx_pkts FROM totals")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TotalsRow{}
	for rows.Next() {
		var r TotalsRow
		if err := rows.Scan(&r.Scope, &r.Rx, &r.Tx, &r.RxPkts, &r.TxPkts); err != nil {
			return nil, err
		}
		out[r.Scope] = r
	}
	return out, rows.Err()
}

// sampleLatestTS 取最近一条有效样本的秒级时间戳。
func sampleLatestTS(db *sql.DB) (int64, bool, error) {
	var m sql.NullInt64
	err := db.QueryRow("SELECT MAX(ts) AS m FROM samples WHERE valid=1 AND duration_ms>0").Scan(&m)
	if err != nil {
		return 0, false, err
	}
	if !m.Valid || m.Int64 == 0 {
		return 0, false, nil
	}
	return m.Int64, true, nil
}

// QRate 最近有效样本的加权平均速率（字节/真实采样耗时）。
// 样本过期（超过 max(15, iv*4) 秒）时返回空表 —— 与旧实现一致，
// 让前端把速率显示回落为“未知”而不是冻结的旧值。
func QRate(db *sql.DB, interval int, nowUnix int64) (map[string]Rate, error) {
	iv := interval
	if iv < 1 {
		iv = 1
	}
	window := 8
	if iv*4 > window {
		window = iv * 4
	}
	staleLimit := int64(15)
	if int64(iv*4) > staleLimit {
		staleLimit = int64(iv * 4)
	}

	latest, ok, err := sampleLatestTS(db)
	if err != nil || !ok {
		return map[string]Rate{}, err
	}
	if nowUnix-latest > staleLimit {
		return map[string]Rate{}, nil
	}
	rows, err := db.Query(
		"SELECT scope, SUM(rx) rx, SUM(tx) tx, SUM(duration_ms) dur FROM samples "+
			"WHERE valid=1 AND duration_ms>0 AND ts>? AND ts<=? GROUP BY scope",
		latest-int64(window), latest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Rate{}
	for rows.Next() {
		var scope string
		var rx, tx, dur sql.NullInt64
		if err := rows.Scan(&scope, &rx, &tx, &dur); err != nil {
			return nil, err
		}
		sec := float64(dur.Int64) / 1000.0
		if sec < 0.001 {
			sec = 0.001
		}
		out[scope] = Rate{Rx: float64(rx.Int64) / sec, Tx: float64(tx.Int64) / sec}
	}
	return out, rows.Err()
}
