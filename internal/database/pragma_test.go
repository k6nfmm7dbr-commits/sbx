package database

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

// 审计项「SQLite 写入优化」要求锁定 PRAGMA 档位，防止将来被误改回默认
// （默认 journal_mode=delete + synchronous=FULL 会让每次 2s 采集都强制 fsync）。
func TestPragmaProfile(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	read := func(name string) string {
		var v string
		if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
			t.Fatalf("读 PRAGMA %s 失败: %v", name, err)
		}
		return v
	}
	if got := read("journal_mode"); got != "wal" {
		t.Errorf("journal_mode 应为 wal, got %q", got)
	}
	// synchronous: 0=OFF 1=NORMAL 2=FULL 3=EXTRA
	if got := read("synchronous"); got != "1" {
		t.Errorf("synchronous 应为 1(NORMAL), got %q", got)
	}
	if got := read("busy_timeout"); got != "30000" {
		t.Errorf("busy_timeout 应为 30000, got %q", got)
	}
	if got := read("journal_size_limit"); got != "8388608" {
		t.Errorf("journal_size_limit 应为 8388608, got %q", got)
	}
}

// 覆盖值必须走白名单：任意字符串不得被拼进 PRAGMA 语句。
func TestSynchronousOverrideWhitelist(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, ok := range []string{"FULL", "normal", " Extra "} {
		if err := db.applySynchronous(ok); err != nil {
			t.Errorf("%q 应被接受: %v", ok, err)
		}
	}
	for _, bad := range []string{"2; DROP TABLE totals", "", "FULLY", "NORMAL;"} {
		if err := db.applySynchronous(bad); err == nil {
			t.Errorf("%q 应被拒绝（白名单）", bad)
		}
	}
}

// 环境变量覆盖路径：合法值生效，非法值不致命（保持 NORMAL）。
func TestSynchronousEnvOverride(t *testing.T) {
	t.Setenv("SBX_SQLITE_SYNCHRONOUS", "FULL")
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("PRAGMA synchronous").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "2" {
		t.Errorf("SBX_SQLITE_SYNCHRONOUS=FULL 应生效(2), got %q", v)
	}
}

// 写入性能基线：采集每轮提交 1 次事务、写入 N 个 scope。
// 用基准而不是"断言耗时"，避免 CI 抖动导致 flaky；关注点是 allocs/op 与
// 是否走了单事务批量路径（逐条自动提交会慢一个数量级）。
func BenchmarkCommitTickBatch(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	const scopes = 50
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatal(err)
		}
		st, err := tx.PrepareContext(ctx,
			"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?) "+
				"ON CONFLICT(scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx")
		if err != nil {
			b.Fatal(err)
		}
		for s := 0; s < scopes; s++ {
			if _, err := st.ExecContext(ctx, "node:"+strconv.Itoa(s), 1, 2, 3, 4); err != nil {
				b.Fatal(err)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}
