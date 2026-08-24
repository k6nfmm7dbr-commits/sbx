package traffic

import (
	"context"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
)

// ipt 双栈计数名：parse 后同属 node:1/rx，基线按完整名字存放在 counter_state。
func dualSnap(v4Bytes, v6Bytes int64) firewall.Snapshot {
	snap := firewall.Snapshot{
		"sbx:epoch_5": {0, 0},
	}
	if v4Bytes >= 0 {
		snap["sbx:n1:i@v4"] = [2]int64{v4Bytes, 10}
	}
	if v6Bytes >= 0 {
		snap["sbx:n1:i@v6"] = [2]int64{v6Bytes, 10}
	}
	return snap
}

func ctrState(t *testing.T, env *testEnv) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
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
		out[n] = b
	}
	return out
}

func totalsRX(t *testing.T, env *testEnv) (rx, tx int64) {
	t.Helper()
	tot, err := QTotals(env.db.DB)
	if err != nil {
		t.Fatal(err)
	}
	return tot["node:1"].Rx, tot["node:1"].Tx
}

// 场景复刻：服务重启后，历史基线含 v4+v6；新一轮上游把“v6 读取失败”
// 当成 v4-only 完整快照返回。守卫必须整轮拒绝，保住 v6 基线；
// 恢复后只累计真实 delta，绝不允许 v6 全量重复入账。
func TestRestartPartialSnapshotGuard(t *testing.T) {
	env := newEnv(t, 2)

	// ---- 阶段一：建立跨重启基线（旧进程）----
	bA := &scriptBackend{steps: []step{
		{snap: dualSnap(1000, 1000)}, // 首见：全量入账、无速率
		{snap: dualSnap(1100, 1050)}, // delta +100/+50，写有效样本
	}}
	env.coll.SetBackend(bA)
	env.advance(0)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rx1, tx1 := totalsRX(t, env)
	st1 := ctrState(t, env)
	if st1["sbx:n1:i@v4"] != 1100 || st1["sbx:n1:i@v6"] != 1050 {
		t.Fatalf("前置基线错误: %v", st1)
	}
	fp1 := fingerprint(t, env)

	// ---- 阶段二：模拟 systemctl restart —— 全新的 provider/collector ----
	// 重启首轮：v4 成功(1200)，v6 读取失败被上游当作“不存在”→ 只返回 v4
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: dualSnap(1200, -1)}}})
	env.advance(2000)
	err := env.coll.Tick(context.Background())
	if err == nil {
		t.Fatal("缺失既有家族的快照必须整轮拒绝")
	}
	if !strings.Contains(err.Error(), "疑似部分读取") {
		t.Errorf("错误信息应说明部分读取: %v", err)
	}
	fp2 := fingerprint(t, env)
	if !fpEqual(fp1, fp2) {
		t.Fatalf("拒绝轮修改了数据库!\n before=%+v\n after=%+v", fp1, fp2)
	}
	st2 := ctrState(t, env)
	if st2["sbx:n1:i@v6"] != 1050 {
		t.Errorf("v6 基线必须原样保留, got %v", st2)
	}

	// 恢复轮：v4=1300, v6=1150 → 相对上一成功基线只累计 +200/+100
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: dualSnap(1300, 1150)}}})
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rx2, tx2 := totalsRX(t, env)
	if d := rx2 - rx1; d != 300 { // v4 delta 200 + v6 delta 100
		t.Fatalf("恢复轮 rx 增量=%d, 期望 300（绝不能出现 v6 全量重计）", d)
	}
	if d := tx2 - tx1; d != 0 {
		t.Errorf("tx 不应有变化, got +%d", d)
	}
	// 基线推进到最新值
	st3 := ctrState(t, env)
	if st3["sbx:n1:i@v4"] != 1300 || st3["sbx:n1:i@v6"] != 1150 {
		t.Errorf("恢复后基线错误: %v", st3)
	}
}

// 反方向：缺 v4（v4 READ ERROR）同样不得提交。
func TestRestartPartialSnapshotGuardReversed(t *testing.T) {
	env := newEnv(t, 2)
	bA := &scriptBackend{steps: []step{
		{snap: dualSnap(1000, 1000)},
		{snap: dualSnap(1100, 1050)},
	}}
	env.coll.SetBackend(bA)
	env.advance(0)
	_ = env.coll.Tick(context.Background())
	env.advance(2000)
	_ = env.coll.Tick(context.Background())
	fp1 := fingerprint(t, env)

	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: dualSnap(-1, 1200)}}}) // 只剩 v6
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err == nil {
		t.Fatal("缺失 v4 的快照必须整轮拒绝")
	}
	if fp := fingerprint(t, env); !fpEqual(fp1, fp) {
		t.Fatal("反方向拒绝轮修改了数据库")
	}
}

// 纯 IPv4 机器：历史上从未出现过 v6 计数器 → 永远不触发守卫，正常工作。
func TestPureV4UnaffectedByGuard(t *testing.T) {
	env := newEnv(t, 2)
	v4Only := func(b int64) firewall.Snapshot {
		return firewall.Snapshot{"sbx:epoch_5": {0, 0}, "sbx:n1:i@v4": {b, 3}}
	}
	b := &scriptBackend{steps: []step{
		{snap: v4Only(500)},
		{snap: v4Only(800)},
		{snap: v4Only(1200)},
	}}
	env.coll.SetBackend(b)
	for i := 0; i < 3; i++ {
		env.advance(2000)
		if err := env.coll.Tick(context.Background()); err != nil {
			t.Fatalf("纯 v4 第 %d 轮不应失败: %v", i+1, err)
		}
	}
	rx, _ := totalsRX(t, env)
	if rx != 500+300+400 {
		t.Errorf("纯 v4 累计错误: %d", rx)
	}
}

// 合法 epoch 切换（规则重建后计数器集合变小）：不受守卫影响。
func TestEpochChangeWithFewerCountersAllowed(t *testing.T) {
	env := newEnv(t, 2)
	// 旧世代：双栈
	bA := &scriptBackend{steps: []step{
		{snap: dualSnap(1000, 1000)},
		{snap: dualSnap(1100, 1050)},
	}}
	env.coll.SetBackend(bA)
	env.advance(0)
	_ = env.coll.Tick(context.Background())
	env.advance(2000)
	_ = env.coll.Tick(context.Background())

	// 新世代（epoch_9）：管理员重建为仅 v4 规则
	newGen := firewall.Snapshot{"sbx:epoch_9": {0, 0}, "sbx:n1:i@v4": {70, 1}}
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: newGen}}})
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatalf("合法 epoch 切换不应被守卫拦截: %v", err)
	}
	// 新世代从零基线：70 全量入账
	var ep string
	if err := env.db.QueryRow("SELECT v FROM meta WHERE k='epoch'").Scan(&ep); err != nil || ep != "9" {
		t.Errorf("meta.epoch 应为 9, got %q (%v)", ep, err)
	}
	tot, _ := QTotals(env.db.DB)
	if tot["node:1"].Rx < 70 {
		t.Errorf("换代后应至少入账新值 70: %+v", tot["node:1"])
	}
}
