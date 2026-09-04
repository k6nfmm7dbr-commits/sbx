package traffic

import (
	"context"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
)

// twoNodeSnap 构造双节点 nft 计数快照。传入 -1 表示该计数器「本轮缺失」，
// 用于模拟不完整读取（nft 因权限/瞬时错误/表被外部部分修改而少返回 counter）。
func twoNodeSnap(n1Bytes, n2Bytes int64) firewall.Snapshot {
	snap := firewall.Snapshot{"sbx_epoch_5": {0, 0}}
	if n1Bytes >= 0 {
		snap["sbx_n1_i"] = [2]int64{n1Bytes, 10}
	}
	if n2Bytes >= 0 {
		snap["sbx_n2_i"] = [2]int64{n2Bytes, 10}
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

func nodeTotals(t *testing.T, env *testEnv, scope string) (rx, tx int64) {
	t.Helper()
	tot, err := QTotals(env.db.DB)
	if err != nil {
		t.Fatal(err)
	}
	return tot[scope].Rx, tot[scope].Tx
}

// 场景复刻：服务重启后，历史基线含两个节点计数器；新一轮读取只返回其中一个
// （nft 部分读取）。守卫必须整轮拒绝，保住缺失计数器的基线；
// 恢复后只累计真实 delta，绝不允许缺失过的计数器全量重复入账。
func TestRestartPartialSnapshotGuard(t *testing.T) {
	env := newEnv(t, 2)

	// ---- 阶段一：建立跨重启基线（旧进程）----
	bA := &scriptBackend{steps: []step{
		{snap: twoNodeSnap(1000, 1000)}, // 首见：全量入账、无速率
		{snap: twoNodeSnap(1100, 1050)}, // delta +100/+50，写有效样本
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
	rx1n1, _ := nodeTotals(t, env, "node:1")
	rx1n2, _ := nodeTotals(t, env, "node:2")
	st1 := ctrState(t, env)
	if st1["sbx_n1_i"] != 1100 || st1["sbx_n2_i"] != 1050 {
		t.Fatalf("前置基线错误: %v", st1)
	}
	fp1 := fingerprint(t, env)

	// ---- 阶段二：模拟 systemctl restart —— 全新的 provider/collector ----
	// 重启首轮：n1 成功(1200)，n2 计数器在快照里缺失（部分读取）
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: twoNodeSnap(1200, -1)}}})
	env.advance(2000)
	err := env.coll.Tick(context.Background())
	if err == nil {
		t.Fatal("缺失既有计数器的快照必须整轮拒绝")
	}
	if !strings.Contains(err.Error(), "疑似部分读取") {
		t.Errorf("错误信息应说明部分读取: %v", err)
	}
	fp2 := fingerprint(t, env)
	if !fpEqual(fp1, fp2) {
		t.Fatalf("拒绝轮修改了数据库!\n before=%+v\n after=%+v", fp1, fp2)
	}
	st2 := ctrState(t, env)
	if st2["sbx_n2_i"] != 1050 {
		t.Errorf("缺失计数器的基线必须原样保留, got %v", st2)
	}

	// 恢复轮：n1=1300, n2=1150 → 相对上一成功基线只累计 +200/+100
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: twoNodeSnap(1300, 1150)}}})
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rx2n1, _ := nodeTotals(t, env, "node:1")
	rx2n2, _ := nodeTotals(t, env, "node:2")
	if d := rx2n1 - rx1n1; d != 200 {
		t.Fatalf("node1 恢复轮 rx 增量=%d, 期望 200", d)
	}
	if d := rx2n2 - rx1n2; d != 100 {
		t.Fatalf("node2 恢复轮 rx 增量=%d, 期望 100（绝不能全量重计）", d)
	}
	// 基线推进到最新值
	st3 := ctrState(t, env)
	if st3["sbx_n1_i"] != 1300 || st3["sbx_n2_i"] != 1150 {
		t.Errorf("恢复后基线错误: %v", st3)
	}
}

// 反方向：缺 n1 同样不得提交。
func TestRestartPartialSnapshotGuardReversed(t *testing.T) {
	env := newEnv(t, 2)
	bA := &scriptBackend{steps: []step{
		{snap: twoNodeSnap(1000, 1000)},
		{snap: twoNodeSnap(1100, 1050)},
	}}
	env.coll.SetBackend(bA)
	env.advance(0)
	_ = env.coll.Tick(context.Background())
	env.advance(2000)
	_ = env.coll.Tick(context.Background())
	fp1 := fingerprint(t, env)

	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: twoNodeSnap(-1, 1200)}}}) // 只剩 n2
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err == nil {
		t.Fatal("缺失 n1 的快照必须整轮拒绝")
	}
	if fp := fingerprint(t, env); !fpEqual(fp1, fp) {
		t.Fatal("反方向拒绝轮修改了数据库")
	}
}

// 单节点机器：历史上从未出现过第二个计数器 → 永远不触发守卫，正常工作。
func TestSingleCounterUnaffectedByGuard(t *testing.T) {
	env := newEnv(t, 2)
	one := func(b int64) firewall.Snapshot {
		return firewall.Snapshot{"sbx_epoch_5": {0, 0}, "sbx_n1_i": {b, 3}}
	}
	b := &scriptBackend{steps: []step{
		{snap: one(500)},
		{snap: one(800)},
		{snap: one(1200)},
	}}
	env.coll.SetBackend(b)
	for i := 0; i < 3; i++ {
		env.advance(2000)
		if err := env.coll.Tick(context.Background()); err != nil {
			t.Fatalf("单计数器第 %d 轮不应失败: %v", i+1, err)
		}
	}
	rx, _ := nodeTotals(t, env, "node:1")
	if rx != 500+300+400 {
		t.Errorf("单计数器累计错误: %d", rx)
	}
}

// 合法 epoch 切换（规则重建后计数器集合变小）：不受守卫影响。
func TestEpochChangeWithFewerCountersAllowed(t *testing.T) {
	env := newEnv(t, 2)
	bA := &scriptBackend{steps: []step{
		{snap: twoNodeSnap(1000, 1000)},
		{snap: twoNodeSnap(1100, 1050)},
	}}
	env.coll.SetBackend(bA)
	env.advance(0)
	_ = env.coll.Tick(context.Background())
	env.advance(2000)
	_ = env.coll.Tick(context.Background())

	// 新世代（epoch_9）：管理员删掉了 node2，规则重建后只剩 n1
	newGen := firewall.Snapshot{"sbx_epoch_9": {0, 0}, "sbx_n1_i": {70, 1}}
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: newGen}}})
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatalf("合法 epoch 切换不应被守卫拦截: %v", err)
	}
	var ep string
	if err := env.db.QueryRow("SELECT v FROM meta WHERE k='epoch'").Scan(&ep); err != nil || ep != "9" {
		t.Errorf("meta.epoch 应为 9, got %q (%v)", ep, err)
	}
	tot, _ := QTotals(env.db.DB)
	if tot["node:1"].Rx < 70 {
		t.Errorf("换代后应至少入账新值 70: %+v", tot["node:1"])
	}
}

// nftables-only 升级兼容（v3.0.9）：老库 counter_state 里可能残留旧后端写入的
// `sbx:n1:i@v4` 形态基线键。它们在 nft 快照里**必然**缺失，若计入
// partial-snapshot 守卫，升级后每轮 Tick 都会判定「部分读取」而永久拒绝提交
// （统计彻底停摆）。守卫必须跳过这类历史键。
//
// 触发条件真实存在：旧 Rules() 用同一个 epoch 同时生成 nft.conf 与 iptables.sh，
// 因此 iptables 后端记录的 meta.epoch 与 nft.conf 里的 epoch 相同 →
// 升级后 freshRuleset=false → 守卫生效 → 命中历史键。
func TestLegacyBaselineKeysSkippedByGuard(t *testing.T) {
	env := newEnv(t, 2)

	// 预置：模拟旧 iptables 后端留下的基线 + 同 epoch 的 meta
	if _, err := env.db.Exec(
		`INSERT INTO counter_state(name,last_bytes,last_pkts,updated_at) VALUES
		 ('sbx:n1:i@v4',1000,10,1700000000000),
		 ('sbx:n1:i@v6',500,5,1700000000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO meta(k,v) VALUES('epoch','5')`); err != nil {
		t.Fatal(err)
	}

	// 新一轮：nft 快照（epoch 同为 5 → freshRuleset=false → 守卫会运行）
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: twoNodeSnap(300, 200)}}})
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatalf("历史形态基线键不得触发部分读取守卫: %v", err)
	}

	// 提交成功后 counter_state 被整表重写，历史键自然消失
	st := ctrState(t, env)
	for _, k := range []string{"sbx:n1:i@v4", "sbx:n1:i@v6"} {
		if _, ok := st[k]; ok {
			t.Errorf("历史键应在首次成功提交后被清除, 仍存在: %s", k)
		}
	}
	if st["sbx_n1_i"] != 300 || st["sbx_n2_i"] != 200 {
		t.Errorf("新基线应正常写入: %v", st)
	}
}

// 守卫对 nft 形态的缺失仍然严格：确认上一条兼容不会把守卫整体削弱。
func TestGuardStillStrictForNFTKeys(t *testing.T) {
	env := newEnv(t, 2)
	env.coll.SetBackend(&scriptBackend{steps: []step{
		{snap: twoNodeSnap(100, 100)},
	}})
	env.advance(0)
	if err := env.coll.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 同 epoch 下 n2 缺失 → 必须拒绝
	env.coll.SetBackend(&scriptBackend{steps: []step{{snap: twoNodeSnap(200, -1)}}})
	env.advance(2000)
	if err := env.coll.Tick(context.Background()); err == nil {
		t.Fatal("nft 形态计数器缺失必须仍被守卫拦截")
	}
}
