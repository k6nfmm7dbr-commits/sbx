package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
)

// scriptBackend 按脚本逐步返回快照或错误，模拟 provider 部分失败。
type scriptBackend struct {
	steps []step
	i     int
}

type step struct {
	snap firewall.Snapshot
	err  error
}

func (b *scriptBackend) Name() string { return "iptables" }
func (b *scriptBackend) Read(context.Context) (firewall.Snapshot, error) {
	if b.i >= len(b.steps) {
		return nil, &firewall.ErrLookup{Msg: "脚本耗尽"}
	}
	s := b.steps[b.i]
	b.i++
	return s.snap, s.err
}
func (b *scriptBackend) Repair(context.Context) error { return nil }

// dbFingerprint 导出与 baseline 相关的全部状态，用于断言“失败轮零写入”。
type dbFingerprint struct {
	totals       map[string]TotalsRow
	counterState map[string][2]int64
	metaEpoch    string
	validSamples int64
	allSamples   int64
}

func fingerprint(t *testing.T, env *testEnv) dbFingerprint {
	t.Helper()
	fp := dbFingerprint{totals: map[string]TotalsRow{}, counterState: map[string][2]int64{}}
	tot, err := QTotals(env.db.DB)
	if err != nil {
		t.Fatal(err)
	}
	fp.totals = tot
	rows, err := env.db.Query("SELECT name,last_bytes FROM counter_state")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var b int64
		if err := rows.Scan(&n, &b); err != nil {
			t.Fatal(err)
		}
		fp.counterState[n] = [2]int64{b, 0}
	}
	var v sqlNullStringHelper
	_ = v
	_ = env.db.QueryRow("SELECT COALESCE((SELECT v FROM meta WHERE k='epoch'),'')").Scan(&fp.metaEpoch)
	_ = env.db.QueryRow("SELECT COUNT(*) FROM samples WHERE valid=1").Scan(&fp.validSamples)
	_ = env.db.QueryRow("SELECT COUNT(*) FROM samples").Scan(&fp.allSamples)
	return fp
}

type sqlNullStringHelper struct{}

// 场景复刻（问题报告原文）：
//
//	Tick1: v4=1000, v6=1000        → 双双首见入账
//	Tick2: v4 正常但 v6 READ ERROR → 整轮失败，baseline 不动、任何表都不写
//	Tick3: v4=2000, v6=2000        → 只累计真实 delta(1000)，绝不把 2000 全量重复入账
func TestPartialProviderSnapshotNeverCommits(t *testing.T) {
	env := newEnv(t, 2)

	errBoom := context.DeadlineExceeded // 任意非 Lookup 错误
	b := &scriptBackend{steps: []step{
		{snap: firewall.Snapshot{
			"sbx_n1_i": {1000, 10}, "sbx_n1_o": {1000, 10},
			"sbx_epoch_7": {0, 0},
		}},
		{err: errBoom},
		{snap: firewall.Snapshot{
			"sbx_n1_i": {2000, 20}, "sbx_n1_o": {2000, 20},
			"sbx_epoch_7": {0, 0},
		}},
	}}
	env.coll.SetBackend(b)

	// Tick1：首见全量入账，无有效样本
	env.advance(0)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	tot, _ := QTotals(env.db.DB)
	if tot["node:1"].Rx != 1000 || tot["node:1"].Tx != 1000 {
		t.Fatalf("首见应各入账 1000: %+v", tot["node:1"])
	}
	fpAfterTick1 := fingerprint(t, env)

	// Tick2：provider 失败 → 完全不 commit（daily/totals/samples/counter_state/meta 全不动）
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err == nil {
		t.Fatal("provider 失败必须返回错误")
	}
	fpAfterTick2 := fingerprint(t, env)
	if !fpEqual(fpAfterTick1, fpAfterTick2) {
		t.Fatalf("失败轮修改了数据库!\n tick1=%+v\n tick2=%+v", fpAfterTick1, fpAfterTick2)
	}
	// 说明：last_error 由 Run 循环负责置位；本测试直接调 Tick，
	// 因此这里只断言“零写入”语义（上面 fingerprint 已覆盖）。

	// Tick3：恢复 → 只累计 delta=1000，且重新变 healthy
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	tot, _ = QTotals(env.db.DB)
	if tot["node:1"].Rx != 2000 || tot["node:1"].Tx != 2000 {
		t.Fatalf("恢复轮只应累计 delta: rx=%d tx=%d (期望各 2000)",
			tot["node:1"].Rx, tot["node:1"].Tx)
	}
	if st := env.coll.Snapshot(); st.Error != "" {
		t.Errorf("恢复后 last_error 应清空, got %q", st.Error)
	}
	// counter_state 基线推进到 2000
	fp := fingerprint(t, env)
	if fp.counterState["sbx_n1_i"][0] != 2000 {
		t.Errorf("基线应为 2000: %+v", fp.counterState)
	}
	// 有效样本仅 Tick3 一轮（首见/失败轮均无）
	if fp.validSamples != 1 || fp.allSamples != 1 {
		t.Errorf("样本数 valid=%d all=%d, 期望 1/1", fp.validSamples, fp.allSamples)
	}
}

// 反方向 + 多轮交错：成功→成功→失败→失败→成功，累计严格等于成功轮差分之和。
func TestPartialProviderInterleavedFailures(t *testing.T) {
	env := newEnv(t, 2)
	errBoom := context.DeadlineExceeded
	mk := func(i, o int64) firewall.Snapshot {
		return firewall.Snapshot{"sbx_n1_i": {i, 5}, "sbx_n1_o": {o, 5}}
	}
	b := &scriptBackend{steps: []step{
		{snap: mk(100, 100)}, // 首见
		{snap: mk(160, 140)}, // delta 60 / 40
		{err: errBoom},       // 失败：不 commit
		{err: errBoom},       // 连续失败：仍不 commit
		{snap: mk(220, 190)}, // delta 相对上一成功轮(160,140) = 60 / 50
	}}
	env.coll.SetBackend(b)
	ctx := context.Background()
	for i, st := range b.steps {
		env.advance(2000)
		err := env.coll.Tick(ctx)
		if st.err != nil && err == nil {
			t.Fatalf("step %d 应失败", i)
		}
		if st.err == nil && err != nil {
			t.Fatalf("step %d 不应失败: %v", i, err)
		}
	}
	tot, _ := QTotals(env.db.DB)
	rx, tx := tot["node:1"].Rx, tot["node:1"].Tx
	if rx != 100+60+60 || tx != 100+40+50 {
		t.Fatalf("累计错误 rx=%d tx=%d (期望 220/190)", rx, tx)
	}
	// 中间两轮失败不得产生任何样本或基线变动痕迹：
	// 最终基线 = 最后一次成功快照
	fp := fingerprint(t, env)
	if fp.counterState["sbx_n1_i"][0] != 220 || fp.counterState["sbx_n1_o"][0] != 190 {
		t.Errorf("最终基线错误: %+v", fp.counterState)
	}
	if fp.validSamples != 2 {
		t.Errorf("有效样本应为 2 个(两次成功增量轮), got %d", fp.validSamples)
	}
	_ = time.Now
}

func fpEqual(a, b dbFingerprint) bool {
	if len(a.totals) != len(b.totals) || len(a.counterState) != len(b.counterState) {
		return false
	}
	for k, v := range a.totals {
		if b.totals[k] != v {
			return false
		}
	}
	for k, v := range a.counterState {
		if b.counterState[k][0] != v[0] {
			return false
		}
	}
	return a.metaEpoch == b.metaEpoch &&
		a.validSamples == b.validSamples && a.allSamples == b.allSamples
}
