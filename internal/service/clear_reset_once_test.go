package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// ---- Clear：最终采样失败必须取消清除（数据保护） ---------------------------

func TestClearAbortsWhenFinalSampleFails(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, validNode)
	readBaseline := seedBaseline(t, a.dbPath)
	want := readBaseline(t)

	// list(-j) 失败(非 Lookup)；delete table 不应被执行到
	nftScript := `if [ "$1" = "-j" ]; then echo "simulated provider failure" >&2; exit 2; fi` + "\nexit 0\n"
	a.installStubs(t, nftScript)

	if rc := Clear(); rc == 0 {
		t.Fatal("最终采样失败时 Clear 必须返回非 0")
	}
	calls := stubCalls(t, a)
	for _, c := range calls {
		if strings.HasPrefix(c, "nft delete") {
			t.Fatalf("采样失败后仍删除了计数表: %v", calls)
		}
	}
	got := readBaseline(t)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("基线 %s 被改动: %q -> %q", k, v, got[k])
		}
	}
}

// ---- Clear：LookupError（规则本就不存在）仍允许清除 ------------------------

func TestClearAllowsLookupError(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, validNode)

	// 首次运行：计数表不存在
	nftScript := `if [ "$1" = "-j" ]; then echo "nft: table does not exist" >&2; exit 1; fi` + "\nexit 0\n"
	a.installStubs(t, nftScript)

	if rc := Clear(); rc != 0 {
		t.Fatal("LookupError(首次运行)必须允许 Clear")
	}
	found := false
	for _, c := range stubCalls(t, a) {
		if strings.HasPrefix(c, "nft delete") {
			found = true
		}
	}
	if !found {
		t.Fatal("LookupError 路径应执行计数表清理")
	}
}

// ---- Clear：正常路径回归 ----------------------------------------------------

func TestClearHappyPath(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, validNode)

	nftScript := `if [ "$1" = "-j" ]; then cat <<'JSON'
` + nftListJSON() + `
JSON
exit 0
fi
exit 0
`
	a.installStubs(t, nftScript)
	if rc := Clear(); rc != 0 {
		t.Fatal("正常路径 Clear 应成功")
	}
	found := false
	for _, c := range stubCalls(t, a) {
		if strings.HasPrefix(c, "nft delete") {
			found = true
		}
	}
	if !found {
		t.Fatal("正常路径应执行计数表清理")
	}
}

// ---- Reset：panel.json 损坏时必须失败且 DB 完全不变 -------------------------

func TestResetRejectsCorruptConfig(t *testing.T) {
	a := setupApp(t, "ipt")
	a.writeConf("127.0.0.1", "t")
	// 预置统计数据
	db, err := database.Open(a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts)
		VALUES('2026-08-25','node:1',10,20,1,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts)
		VALUES('node:1',10,20,1,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid)
		VALUES(1756000000,'node:1',10,20,2000,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// 配置随后损坏
	mustWriteFile(t, a.confPath, "{ invalid json")

	if err := Reset(""); err == nil {
		t.Fatal("损坏配置下 Reset 必须返回错误")
	}
	if err := Reset("node:1"); err == nil {
		t.Fatal("损坏配置下 Reset(scope) 必须返回错误")
	}

	db2, err := database.Open(a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, q := range []string{
		`SELECT COUNT(*) FROM daily`,
		`SELECT COUNT(*) FROM totals`,
		`SELECT COUNT(*) FROM samples`,
	} {
		var n int
		if err := db2.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s 数据被改动: count=%d", q, n)
		}
	}
}

// ---- Once：panel.json 损坏时必须失败且不写任何数据库 ------------------------

func TestOnceRejectsCorruptConfig(t *testing.T) {
	a := setupApp(t, "ipt")
	a.writeConf("127.0.0.1", "t")
	readBaseline := seedBaseline(t, a.dbPath)
	want := readBaseline(t)

	mustWriteFile(t, a.confPath, "{ invalid json")

	// provider 桩：任何调用都说明流程未被配置闸门拦住
	a.installStubs(t, "exit 2")

	if err := Once(); err == nil {
		t.Fatal("损坏配置下 Once 必须返回错误")
	}
	if calls := stubCalls(t, a); len(calls) != 0 {
		t.Fatalf("配置闸门未生效, provider 被调用: %v", calls)
	}
	got := readBaseline(t)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("数据库 %s 被改动: %q -> %q", k, v, got[k])
		}
	}
}

// ---- 防回归：samples 目录不落任何新文件（Once 损坏配置时零写入） -----------

func TestOnceCorruptConfigWritesNothing(t *testing.T) {
	a := setupApp(t, "ipt")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.confPath, "{ invalid")
	_ = Once()
	entries, err := filepath.Glob(filepath.Join(a.appDir, "*.db-wal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("损坏配置下 Once 产生了数据库写入痕迹: %v", entries)
	}
}
