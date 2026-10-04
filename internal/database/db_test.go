package database

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesSchema(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	wantTables := map[string]bool{"meta": false, "counter_state": false,
		"daily": false, "totals": false, "samples": false, "node_policy": false}
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if _, ok := wantTables[name]; ok {
			wantTables[name] = true
		}
	}
	rows.Close()
	for name, seen := range wantTables {
		if !seen {
			t.Errorf("缺少表 %s", name)
		}
	}

	cols, err := tableColumns(db, "node_policy")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["paused"] || cols["quota_enabled"] || cols["quota_limit_bytes"] || cols["quota_reset_baseline"] {
		t.Fatalf("fresh node_policy schema must contain paused and no quota columns: %v", cols)
	}

	var idx int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_samples_ts'").Scan(&idx); err != nil || idx != 1 {
		t.Errorf("idx_samples_ts 索引缺失 (err=%v idx=%d)", err, idx)
	}
}

func TestPragmasApplied(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode=%s, 期望 wal", mode)
	}
	var busy int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy != 30000 {
		t.Errorf("busy_timeout=%d, 期望 30000", busy)
	}
}

// TestMigrateLegacySamples 无损迁移旧格式库：无 duration_ms/valid 列的
// samples 自动补列，且历史样本 valid 置 0（不能用于速率计算）。
func TestMigrateLegacySamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 手工降级出“旧库”结构
	for _, q := range []string{
		"DROP TABLE samples",
		"CREATE TABLE samples (ts INTEGER NOT NULL, scope TEXT NOT NULL," +
			" rx INTEGER NOT NULL DEFAULT 0, tx INTEGER NOT NULL DEFAULT 0," +
			" PRIMARY KEY (ts, scope))",
		"INSERT INTO samples(ts,scope,rx,tx) VALUES(100,'node:1',5,6)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	db2, err := Open(path) // 重新打开触发迁移
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	cols := map[string]bool{}
	rows, err := db2.Query("PRAGMA table_info(samples)")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		_ = rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk)
		cols[name] = true
	}
	rows.Close()
	if !cols["duration_ms"] || !cols["valid"] {
		t.Fatalf("迁移未补列: %v", cols)
	}
	var valid int
	if err := db2.QueryRow("SELECT valid FROM samples WHERE ts=100").Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid != 0 {
		t.Errorf("历史样本应置 valid=0, got %d", valid)
	}
}

func TestMigrateLegacyNodePolicyDropsQuotaAndPreservesRemainingPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_np.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DROP TABLE node_policy",
		"CREATE TABLE node_policy (node_id TEXT PRIMARY KEY," +
			" quota_enabled INTEGER NOT NULL DEFAULT 0," +
			" quota_limit_bytes INTEGER NOT NULL DEFAULT 0," +
			" quota_reset_baseline INTEGER NOT NULL DEFAULT 0," +
			" ip_limit_enabled INTEGER NOT NULL DEFAULT 0," +
			" ip_limit_max INTEGER NOT NULL DEFAULT 0," +
			" rate_limit_enabled INTEGER NOT NULL DEFAULT 0," +
			" rate_limit_mbps INTEGER NOT NULL DEFAULT 0)",
		"INSERT INTO node_policy(node_id,quota_enabled,quota_limit_bytes,quota_reset_baseline,ip_limit_enabled,ip_limit_max,rate_limit_enabled,rate_limit_mbps) VALUES('7',1,1073741824,100,1,3,1,50)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	cols, err := tableColumns(db2, "node_policy")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"paused", "ip_limit_enabled", "ip_limit_max", "rate_limit_enabled", "rate_limit_mbps"} {
		if !cols[col] {
			t.Errorf("迁移后缺少保留列 %s: %v", col, cols)
		}
	}
	for _, col := range []string{"quota_enabled", "quota_limit_bytes", "quota_reset_baseline"} {
		if cols[col] {
			t.Errorf("配额列未移除 %s: %v", col, cols)
		}
	}
	var ipOn, ipMax, rateOn, mbps, paused int
	if err := db2.QueryRow("SELECT paused,ip_limit_enabled,ip_limit_max,rate_limit_enabled,rate_limit_mbps FROM node_policy WHERE node_id='7'").
		Scan(&paused, &ipOn, &ipMax, &rateOn, &mbps); err != nil {
		t.Fatal(err)
	}
	if paused != 0 || ipOn != 1 || ipMax != 3 || rateOn != 1 || mbps != 50 {
		t.Errorf("迁移后必须保留暂停默认与 IP/限速策略: paused=%d ip=%d/%d rate=%d/%d", paused, ipOn, ipMax, rateOn, mbps)
	}
}

func TestTransactionAtomicity(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('a',1,1,1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('b',2,2,2,2), ('a',9,9,9,9)"); err == nil {
		t.Error("主键冲突应失败")
	} else if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM totals").Scan(&n)
	if n != 0 {
		t.Errorf("回滚后应为空, got %d", n)
	}
}

func TestMigratePreRateNodePolicyAddsNewFieldsAndDropsQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre_rate_np.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DROP TABLE node_policy",
		"CREATE TABLE node_policy (node_id TEXT PRIMARY KEY, quota_enabled INTEGER NOT NULL DEFAULT 0, quota_limit_bytes INTEGER NOT NULL DEFAULT 0, quota_reset_baseline INTEGER NOT NULL DEFAULT 0, ip_limit_enabled INTEGER NOT NULL DEFAULT 0, ip_limit_max INTEGER NOT NULL DEFAULT 0)",
		"INSERT INTO node_policy(node_id,quota_enabled,quota_limit_bytes,quota_reset_baseline,ip_limit_enabled,ip_limit_max) VALUES('9',1,2000,100,1,4)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	cols, err := tableColumns(db2, "node_policy")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"paused", "rate_limit_enabled", "rate_limit_mbps", "ip_limit_enabled", "ip_limit_max"} {
		if !cols[col] {
			t.Errorf("pre-rate migration missing %s: %v", col, cols)
		}
	}
	for _, col := range []string{"quota_enabled", "quota_limit_bytes", "quota_reset_baseline"} {
		if cols[col] {
			t.Errorf("pre-rate migration kept removed quota column %s", col)
		}
	}
	var paused, ipOn, ipMax, rateOn, mbps int
	if err := db2.QueryRow("SELECT paused,ip_limit_enabled,ip_limit_max,rate_limit_enabled,rate_limit_mbps FROM node_policy WHERE node_id='9'").
		Scan(&paused, &ipOn, &ipMax, &rateOn, &mbps); err != nil {
		t.Fatal(err)
	}
	if paused != 0 || ipOn != 1 || ipMax != 4 || rateOn != 0 || mbps != 0 {
		t.Errorf("pre-rate migration values incorrect: paused=%d ip=%d/%d rate=%d/%d", paused, ipOn, ipMax, rateOn, mbps)
	}
}
