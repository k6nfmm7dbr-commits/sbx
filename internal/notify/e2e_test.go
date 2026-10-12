package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// TestEndToEndPushThenReplay 是"推送即备份"这一主张的闭环验证：
// 采集数据 → 渲染 → 经真实 HTTP 送出 → 从收到的消息里提取可回放数据 →
// 写进一个全新的空库 → 与源库逐行比对。
//
// 如果这个测试通过，就意味着"服务器炸了但聊天记录还在"时确实能恢复出数据。
func TestEndToEndPushThenReplay(t *testing.T) {
	day := "2026-10-11"
	want := map[string][4]int64{
		"node:hk01": {12_990_000_000, 8_610_000_000, 111, 222},
		"node:jp02": {10_550_000_000, 6_980_000_000, 333, 444},
		"node:sg03": {8_440_000_000, 5_600_000_000, 0, 0},
	}

	srcDir := t.TempDir()
	srcDB, err := database.Open(filepath.Join(srcDir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer srcDB.Close()
	for scope, v := range want {
		mustExec(t, srcDB, "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)",
			day, scope, v[0], v[1], v[2], v[3])
	}

	var (
		mu       sync.Mutex
		received [][]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			Replay []string `json:"replay"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		received = append(received, p.Replay)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		DB:        filepath.Join(srcDir, "traffic.db"),
		NodesFile: filepath.Join(srcDir, "nodes.json"),
		TZ:        "UTC", Interval: 2,
		Notify: &config.NotifyConfig{
			Enabled: true, IntervalMinutes: 30, DailyAt: "00:05", TimeoutSeconds: 5,
		},
	}
	mgr, err := NewManager(cfg, srcDB, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr.now = func() time.Time { return time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC) }
	mgr.backoffFn = func(int) time.Duration { return time.Millisecond }
	mgr.channels = []Channel{&webhookChannel{
		channelBase: channelBase{name: "recv"},
		url:         srv.URL, maxBytes: 8192, client: srv.Client(),
	}}

	mgr.sendDaily(context.Background(), day)

	mu.Lock()
	gotReplay := received
	mu.Unlock()
	if len(gotReplay) != 1 {
		t.Fatalf("应收到 1 次日结推送, 实得 %d", len(gotReplay))
	}
	if len(gotReplay[0]) == 0 {
		t.Fatal("推送内容里没有可回放数据")
	}

	// ---- 目标库：完全空库，仅凭推送内容重建 ----
	dstDB, err := database.Open(filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dstDB.Close()

	res, err := ReplayInto(dstDB.DB, strings.Join(gotReplay[0], "\n"), ReplayOptions{})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if res.Days != 1 || res.FromDaily != 1 {
		t.Fatalf("应恢复出 1 天日结数据, 实得 %+v", res)
	}
	if len(res.Errors) != 0 {
		t.Errorf("恢复过程出现错误: %v", res.Errors)
	}

	got := readDaily(t, dstDB, day)
	if len(got) != len(want) {
		t.Fatalf("恢复出的行数不符: %d vs %d (%v)", len(got), len(want), got)
	}
	for scope, v := range want {
		g, ok := got[scope]
		if !ok {
			t.Errorf("缺少 scope %s", scope)
			continue
		}
		if g != v {
			t.Errorf("%s 数据不符:\n  got  %v\n  want %v", scope, g, v)
		}
	}
}

// TestHeartbeatRecoveryUsesReservedScope：机毁当日只有心跳数据时，
// 恢复出的总量**绝不能**以 node:<id> 形式入库——那会被面板的
// `scope LIKE 'node:%'` 聚合误计，让日总量凭空翻倍。
func TestHeartbeatRecoveryUsesReservedScope(t *testing.T) {
	line := EncodeReplay(ReplayRecord{
		K: KindHeartbeat, D: "2026-10-12", T: "14:30", I: 0, N: 1,
		R: []ReplayRow{{Scope: ReplayScopeTotal, Rx: 5_000_000, Tx: 9_000_000}},
	})

	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := ReplayInto(db.DB, line, ReplayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FromHB != 1 || res.FromDaily != 0 {
		t.Fatalf("应识别为心跳来源: %+v", res)
	}

	rows := readDaily(t, db, "2026-10-12")
	if _, ok := rows[RecoveredScopeTotal]; !ok {
		t.Errorf("心跳数据应写入 %s, 实得 %v", RecoveredScopeTotal, rows)
	}
	for scope := range rows {
		if strings.HasPrefix(scope, "node:") {
			t.Fatalf("心跳恢复绝不能产生 node: 行（会污染面板聚合）: %s", scope)
		}
	}
}

// TestReplayPrefersDailyOverHeartbeat：同一天两者都在时，必须用终值。
func TestReplayPrefersDailyOverHeartbeat(t *testing.T) {
	hb := EncodeReplay(ReplayRecord{
		K: KindHeartbeat, D: "2026-10-11", T: "20:00", I: 0, N: 1,
		R: []ReplayRow{{Scope: ReplayScopeTotal, Rx: 999, Tx: 999}},
	})
	daily := EncodeReplay(ReplayRecord{
		K: KindDaily, D: "2026-10-11", I: 0, N: 1,
		R: []ReplayRow{{Scope: "node:a", Rx: 111, Tx: 222}},
	})

	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := ReplayInto(db.DB, hb+"\n"+daily, ReplayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FromDaily != 1 || res.FromHB != 0 {
		t.Fatalf("有日结时应以日结为准: %+v", res)
	}
	rows := readDaily(t, db, "2026-10-11")
	if v, ok := rows["node:a"]; !ok || v[0] != 111 {
		t.Errorf("应恢复节点级终值, 实得 %v", rows)
	}
	if _, ok := rows[RecoveredScopeTotal]; ok {
		t.Error("有日结时不应再写入心跳兜底数据")
	}
}

// TestReplayDryRunDoesNotWrite：--dry-run 必须真的不落盘。
func TestReplayDryRunDoesNotWrite(t *testing.T) {
	line := EncodeReplay(ReplayRecord{
		K: KindDaily, D: "2026-10-11", I: 0, N: 1,
		R: []ReplayRow{{Scope: "node:a", Rx: 111, Tx: 222}},
	})
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := ReplayInto(db.DB, line, ReplayOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Days != 1 {
		t.Errorf("dry-run 仍应报告解析结果: %+v", res)
	}
	if rows := readDaily(t, db, "2026-10-11"); len(rows) != 0 {
		t.Errorf("dry-run 不应写库, 实得 %v", rows)
	}
}

// TestReplayDoesNotTouchTotals：累计无法从日数据反推，
// 伪造一个"看起来对"的累计比留空更危险。
func TestReplayDoesNotTouchTotals(t *testing.T) {
	line := EncodeReplay(ReplayRecord{
		K: KindDaily, D: "2026-10-11", I: 0, N: 1,
		R: []ReplayRow{{Scope: "node:a", Rx: 111, Tx: 222}},
	})
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?)",
		"node:a", 1, 2, 3, 4)

	if _, err := ReplayInto(db.DB, line, ReplayOptions{}); err != nil {
		t.Fatal(err)
	}

	var rx int64
	if err := db.QueryRow("SELECT rx FROM totals WHERE scope=?", "node:a").Scan(&rx); err != nil {
		t.Fatal(err)
	}
	if rx != 1 {
		t.Errorf("恢复不得改动 totals, 实得 rx=%d", rx)
	}
}

// TestReplayOnlyMissingAndOverwrite：三种写入模式的语义差异。
func TestReplayOnlyMissingAndOverwrite(t *testing.T) {
	line := EncodeReplay(ReplayRecord{
		K: KindDaily, D: "2026-10-11", I: 0, N: 1,
		R: []ReplayRow{{Scope: "node:a", Rx: 100, Tx: 200}},
	})

	open := func() *database.DB {
		db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		mustExec(t, db, "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)",
			"2026-10-11", "node:a", 500, 600, 0, 0)
		return db
	}

	// 默认：取 MAX，较大的既有值应保留
	db1 := open()
	if _, err := ReplayInto(db1.DB, line, ReplayOptions{}); err != nil {
		t.Fatal(err)
	}
	if v := readDaily(t, db1, "2026-10-11")["node:a"]; v[0] != 500 {
		t.Errorf("默认模式应保留较大值, 实得 %v", v)
	}

	// --overwrite：强制覆盖
	db2 := open()
	if _, err := ReplayInto(db2.DB, line, ReplayOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if v := readDaily(t, db2, "2026-10-11")["node:a"]; v[0] != 100 {
		t.Errorf("overwrite 模式应强制覆盖, 实得 %v", v)
	}

	// --only-missing：已有行不动
	db3 := open()
	if _, err := ReplayInto(db3.DB, line, ReplayOptions{OnlyMissing: true}); err != nil {
		t.Fatal(err)
	}
	if v := readDaily(t, db3, "2026-10-11")["node:a"]; v[0] != 500 {
		t.Errorf("only-missing 模式不应改动已有行, 实得 %v", v)
	}
}

// TestReplayAcceptsWebhookJSONL：通用 webhook 的接收方常把 payload 按行落盘，
// 其中的 SBX1 行是被 JSON 转义过的字符串。若恢复只认纯文本，用户会看到
// "解析出 0 天、跳过损坏片段 N 处"却完全不知道为什么。
func TestReplayAcceptsWebhookJSONL(t *testing.T) {
	ch := &webhookChannel{channelBase: channelBase{name: "wh"}, maxBytes: 8192}
	snap := mkSnapshot(KindDaily, 3)
	blocks, _ := Render(snap, ch.Caps())
	payload := ch.buildPayload(Message{Event: "daily", Day: snap.Day, Blocks: blocks, Snap: snap})
	line, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := ReplayInto(db.DB, string(line)+"\n", ReplayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 0 {
		t.Errorf("JSONL 输入不应被计为损坏片段: %d", res.Skipped)
	}
	if res.FromDaily != 1 {
		t.Fatalf("应从 JSONL 恢复出日结, 实得 %+v", res)
	}
	if rows := readDaily(t, db, snap.Day); len(rows) != len(snap.Nodes) {
		t.Errorf("恢复行数不符: %d vs %d", len(rows), len(snap.Nodes))
	}
}

func readDaily(t *testing.T, db *database.DB, day string) map[string][4]int64 {
	t.Helper()
	rows, err := db.Query("SELECT scope,rx,tx,rx_pkts,tx_pkts FROM daily WHERE day=?", day)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][4]int64{}
	for rows.Next() {
		var scope string
		var v [4]int64
		if err := rows.Scan(&scope, &v[0], &v[1], &v[2], &v[3]); err != nil {
			t.Fatal(err)
		}
		out[scope] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
