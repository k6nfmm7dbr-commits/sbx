package traffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
)

// ---- 测试基建 ------------------------------------------------------------

type fakeBackend struct {
	name    string
	seq     []firewall.Snapshot
	repairs int
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Read(context.Context) (firewall.Snapshot, error) {
	if len(f.seq) == 0 {
		return firewall.Snapshot{}, &firewall.ErrLookup{Msg: "exhausted"}
	}
	s := f.seq[0]
	f.seq = f.seq[1:]
	return s, nil
}

func (f *fakeBackend) Repair(context.Context) error { f.repairs++; return nil }

type testEnv struct {
	t       *testing.T
	dir     string
	db      *database.DB
	cfg     *config.Config
	coll    *Collector
	backend *fakeBackend
	clockMS *int64
}

func newEnv(t *testing.T, interval int) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "traffic.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{
		DB: dbPath, NodesFile: filepath.Join(dir, "nodes.json"),
		NftConf: filepath.Join(dir, "nft.conf"),
		Listen:  "127.0.0.1", Port: 8080, Token: "",
		Interval: interval, TZ: "UTC",
	}
	e := &testEnv{t: t, dir: dir, db: db, cfg: cfg}
	ms := int64(1_700_000_000_000)
	e.clockMS = &ms
	e.backend = &fakeBackend{name: "nft"}
	c := NewCollector(cfg, db)
	c.SetBackend(e.backend)
	c.SetClock(func() time.Time { return time.UnixMilli(*e.clockMS) })
	e.coll = c
	t.Cleanup(func() { db.Close() })
	return e
}

func (e *testEnv) advance(ms int64) { *e.clockMS += ms }
func (e *testEnv) now() time.Time   { return time.UnixMilli(*e.clockMS) }

func (e *testEnv) push(m map[string][2]int64) {
	snap := firewall.Snapshot{}
	for k, v := range m {
		snap[k] = v
	}
	e.backend.seq = append(e.backend.seq, snap)
}

func (e *testEnv) tickOK() {
	e.t.Helper()
	if err := e.coll.Tick(context.Background()); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

// ---- 黄金回放：与旧 Python reference 输出逐行对拍 --------------------------

type goldenFile struct {
	Scenarios []goldenScenario `json:"scenarios"`
}

type goldenScenario struct {
	Name       string                `json:"name"`
	Interval   int                   `json:"interval"`
	TimesMs    []int64               `json:"times_ms"`
	Counters   []map[string][2]int64 `json:"counters"`
	ExpectedDB struct {
		Daily        []dailyRowG   `json:"daily"`
		Totals       []totalsRowG  `json:"totals"`
		Samples      []sampleRowG  `json:"samples"`
		CounterState []counterRowG `json:"counter_state"`
		Meta         []metaRowG    `json:"meta"`
	} `json:"expected_db"`

	rawExpected map[string]any
}

type dailyRowG struct {
	Day    string `json:"day"`
	Scope  string `json:"scope"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
	RxPkts int64  `json:"rx_pkts"`
	TxPkts int64  `json:"tx_pkts"`
}
type totalsRowG struct {
	Scope  string `json:"scope"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
	RxPkts int64  `json:"rx_pkts"`
	TxPkts int64  `json:"tx_pkts"`
}
type sampleRowG struct {
	TS         int64  `json:"ts"`
	Scope      string `json:"scope"`
	Rx         int64  `json:"rx"`
	Tx         int64  `json:"tx"`
	DurationMs int64  `json:"duration_ms"`
	Valid      int64  `json:"valid"`
}
type counterRowG struct {
	Name      string `json:"name"`
	LastBytes int64  `json:"last_bytes"`
	LastPkts  int64  `json:"last_pkts"`
	UpdatedAt int64  `json:"updated_at"`
}
type metaRowG struct {
	K string `json:"k"`
	V string `json:"v"`
}

func TestGoldenTrafficScenarios(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "traffic_scenarios.json"))
	if err != nil {
		t.Fatalf("读取金标夹具失败（internal/traffic/testdata/golden 静态快照）: %v", err)
	}
	var gf goldenFile
	if err := json.Unmarshal(data, &gf); err != nil {
		t.Fatal(err)
	}
	for _, sc := range gf.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			env := newEnv(t, sc.Interval)
			for i, snap := range sc.Counters {
				*env.clockMS = sc.TimesMs[i]
				env.push(snap)
				env.tickOK()
			}
			assertGoldenDB(t, env.db.DB, sc)
		})
	}
}

func assertGoldenDB(t *testing.T, db *sql.DB, sc goldenScenario) {
	t.Helper()
	expect := func(name string, gotCount int, wantCount int) {
		t.Helper()
		if gotCount != wantCount {
			t.Errorf("%s 行数不符: got %d want %d", name, gotCount, wantCount)
		}
	}
	rows := func(q string) *sql.Rows {
		r, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		return r // 仅用于计数/扫描的简单场景，这里改为逐表专用查询
	}
	_ = rows

	var n int

	// daily
	var d dailyRowG
	gotDaily := map[dailyRowG]bool{}
	dr, err := db.Query("SELECT day,scope,rx,tx,rx_pkts,tx_pkts FROM daily")
	if err != nil {
		t.Fatal(err)
	}
	for dr.Next() {
		if err := dr.Scan(&d.Day, &d.Scope, &d.Rx, &d.Tx, &d.RxPkts, &d.TxPkts); err != nil {
			t.Fatal(err)
		}
		gotDaily[d] = true
	}
	dr.Close()
	expect("daily", len(gotDaily), len(sc.ExpectedDB.Daily))
	for _, want := range sc.ExpectedDB.Daily {
		if !gotDaily[want] {
			t.Errorf("daily 缺少 %+v", want)
		}
	}

	// totals
	gotTotals := map[totalsRowG]bool{}
	tr, err := db.Query("SELECT scope,rx,tx,rx_pkts,tx_pkts FROM totals")
	if err != nil {
		t.Fatal(err)
	}
	var tv totalsRowG
	for tr.Next() {
		if err := tr.Scan(&tv.Scope, &tv.Rx, &tv.Tx, &tv.RxPkts, &tv.TxPkts); err != nil {
			t.Fatal(err)
		}
		gotTotals[tv] = true
	}
	tr.Close()
	expect("totals", len(gotTotals), len(sc.ExpectedDB.Totals))
	for _, want := range sc.ExpectedDB.Totals {
		if !gotTotals[want] {
			t.Errorf("totals 缺少 %+v", want)
		}
	}

	// samples
	gotSamples := map[sampleRowG]bool{}
	sr, err := db.Query("SELECT ts,scope,rx,tx,duration_ms,valid FROM samples")
	if err != nil {
		t.Fatal(err)
	}
	var sv sampleRowG
	for sr.Next() {
		if err := sr.Scan(&sv.TS, &sv.Scope, &sv.Rx, &sv.Tx, &sv.DurationMs, &sv.Valid); err != nil {
			t.Fatal(err)
		}
		gotSamples[sv] = true
	}
	sr.Close()
	expect("samples", len(gotSamples), len(sc.ExpectedDB.Samples))
	for _, want := range sc.ExpectedDB.Samples {
		if !gotSamples[want] {
			t.Errorf("samples 缺少 %+v", want)
		}
	}

	// counter_state
	gotCS := map[counterRowG]bool{}
	cr, err := db.Query("SELECT name,last_bytes,last_pkts,updated_at FROM counter_state")
	if err != nil {
		t.Fatal(err)
	}
	var cv counterRowG
	for cr.Next() {
		if err := cr.Scan(&cv.Name, &cv.LastBytes, &cv.LastPkts, &cv.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		gotCS[cv] = true
	}
	cr.Close()
	expect("counter_state", len(gotCS), len(sc.ExpectedDB.CounterState))
	for _, want := range sc.ExpectedDB.CounterState {
		if !gotCS[want] {
			t.Errorf("counter_state 缺少 %+v", want)
		}
	}

	// meta
	gotMeta := map[metaRowG]bool{}
	mr, err := db.Query("SELECT k,v FROM meta")
	if err != nil {
		t.Fatal(err)
	}
	var mv metaRowG
	for mr.Next() {
		if err := mr.Scan(&mv.K, &mv.V); err != nil {
			t.Fatal(err)
		}
		gotMeta[mv] = true
	}
	mr.Close()
	expect("meta", len(gotMeta), len(sc.ExpectedDB.Meta))
	for _, want := range sc.ExpectedDB.Meta {
		if !gotMeta[want] {
			t.Errorf("meta 缺少 %+v", want)
		}
	}

	n = 0
	_ = n
}

// ---- 单元行为 ------------------------------------------------------------

func TestCounterResetNeverNegative(t *testing.T) {
	env := newEnv(t, 2)
	env.push(map[string][2]int64{"sbx_n1_i": {1000, 100}, "sbx_n1_o": {500, 50}})
	env.tickOK()
	env.advance(2000)
	env.push(map[string][2]int64{"sbx_n1_i": {1500, 150}, "sbx_n1_o": {800, 80}})
	env.tickOK()
	env.advance(2000)
	// 归零：100 → 补记 100，绝不产生 -900
	env.push(map[string][2]int64{"sbx_n1_i": {100, 10}, "sbx_n1_o": {900, 90}})
	env.tickOK()

	totals, err := QTotals(env.db.DB)
	if err != nil {
		t.Fatal(err)
	}
	n1 := totals["node:1"]
	if n1.Rx != 1000+500+100 {
		t.Errorf("归零后 rx = %d, 期望 1600", n1.Rx)
	}
	if n1.Tx != 500+300+100 {
		t.Errorf("tx = %d, 期望 900", n1.Tx)
	}
}

func TestFirstSeenAccumulatesButNoRate(t *testing.T) {
	env := newEnv(t, 2)
	env.push(map[string][2]int64{"sbx_n1_i": {12345, 100}})
	env.tickOK()
	var valid int64
	if err := env.db.QueryRow("SELECT COUNT(*) FROM samples WHERE valid=1").Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid != 0 {
		t.Errorf("首见轮不应产生有效样本, got %d", valid)
	}
	var rx int64
	if err := env.db.QueryRow("SELECT rx FROM totals WHERE scope='node:1'").Scan(&rx); err != nil {
		t.Fatal(err)
	}
	if rx != 12345 {
		t.Errorf("首见应全量入账, got %d", rx)
	}
}

func TestDurationWindowBoundaries(t *testing.T) {
	// interval=2s -> 窗口 [500ms, 6500ms]
	cases := []struct {
		gapMS int64
		valid bool
	}{
		{499, false},
		{500, true},
		{2000, true},
		{6500, true},
		{6501, false},
	}
	for _, tc := range cases {
		env := newEnv(t, 2)
		env.push(map[string][2]int64{"sbx_n1_i": {100, 10}, "sbx_n1_o": {60, 6}})
		env.tickOK()
		env.advance(tc.gapMS)
		env.push(map[string][2]int64{"sbx_n1_i": {160, 16}, "sbx_n1_o": {120, 12}})
		env.tickOK()
		var cnt int64
		if err := env.db.QueryRow("SELECT COUNT(*) FROM samples WHERE valid=1 AND duration_ms=?",
			tc.gapMS).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if tc.valid {
			want = 1
		}
		if cnt != want {
			t.Errorf("gap=%dms: valid 样本数=%d, 期望 %d", tc.gapMS, cnt, want)
		}
	}
}

func TestRateUsesRealDuration(t *testing.T) {
	// 同样 1000 字节：耗时 2.0s 与 2.5s 的速率必须不同
	rates := map[int64]float64{}
	for _, gap := range []int64{2000, 2500} {
		env := newEnv(t, 2)
		env.push(map[string][2]int64{"sbx_n1_i": {0, 0}, "sbx_n1_o": {0, 0}})
		env.tickOK()
		env.advance(gap)
		env.push(map[string][2]int64{"sbx_n1_i": {0, 0}, "sbx_n1_o": {1000, 10}})
		env.tickOK()
		r, err := QRate(env.db.DB, 2, env.now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		rr, ok := r["node:1"]
		if !ok {
			t.Fatalf("gap=%d 无速率", gap)
		}
		rates[gap] = rr.Tx
	}
	if rates[2000] <= rates[2500] {
		t.Errorf("速率未随真实时长变化: 2.0s=%v 2.5s=%v", rates[2000], rates[2500])
	}
	want := 1000.0 / 2.5
	if math.Abs(rates[2500]-want) > 1e-9 {
		t.Errorf("2.5s 速率=%v, 期望 %v", rates[2500], want)
	}
}

func TestEpochSwitchDropsBaseline(t *testing.T) {
	env := newEnv(t, 2)
	env.push(map[string][2]int64{"sbx_epoch_1": {0, 0}, "sbx_n1_i": {100, 10}})
	env.tickOK()
	env.advance(2000)
	env.push(map[string][2]int64{"sbx_epoch_2": {0, 0}, "sbx_n1_i": {5, 1}})
	env.tickOK()

	var rx int64
	if err := env.db.QueryRow("SELECT rx FROM totals WHERE scope='node:1'").Scan(&rx); err != nil {
		t.Fatal(err)
	}
	if rx != 105 {
		t.Errorf("换代后应全量衔接 100+5, got %d", rx)
	}
	var epoch string
	if err := env.db.QueryRow("SELECT v FROM meta WHERE k='epoch'").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != "2" {
		t.Errorf("meta epoch=%q, 期望 2", epoch)
	}
}

func TestLegacySecondsUpdatedAt(t *testing.T) {
	env := newEnv(t, 2)
	if _, err := env.db.Exec(
		"INSERT INTO counter_state(name,last_bytes,last_pkts,updated_at) VALUES('sbx_n1_i',100,10,1700000000)"); err != nil {
		t.Fatal(err)
	}
	env.push(map[string][2]int64{"sbx_n1_i": {160, 16}})
	env.advance(2000) // 相对基线 1700000000s 的真实间隔由 ms 基线决定
	env.tickOK()

	var updated int64
	if err := env.db.QueryRow(
		"SELECT updated_at FROM counter_state WHERE name='sbx_n1_i'").Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated < 1_000_000_000_000 {
		t.Errorf("新写入应为毫秒级, got %d", updated)
	}
}

func TestSampleSecondMonotonicUnderClockJump(t *testing.T) {
	env := newEnv(t, 2)
	base := *env.clockMS
	env.push(map[string][2]int64{"sbx_n1_i": {100, 10}, "sbx_n1_o": {50, 5}})
	env.tickOK() // ts = base/1000
	*env.clockMS = base + 2000
	env.push(map[string][2]int64{"sbx_n1_i": {150, 15}, "sbx_n1_o": {75, 8}})
	env.tickOK()               // ts = base/1000 + 2
	*env.clockMS = base + 1500 // 回拨
	env.push(map[string][2]int64{"sbx_n1_i": {200, 20}, "sbx_n1_o": {100, 10}})
	env.tickOK() // 必须仍取上一条样本秒+1，不得覆盖

	var count int64
	if err := env.db.QueryRow("SELECT COUNT(*) FROM samples").Scan(&count); err != nil {
		t.Fatal(err)
	}
	// 与金标一致：回拨轮 duration<=0 不产生样本；有效样本仅第 2 轮一条。
	if count != 1 {
		t.Errorf("回拨后样本数=%d, 期望 1", count)
	}
	var ts int64
	if err := env.db.QueryRow("SELECT MAX(ts) FROM samples").Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != base/1000+2 {
		t.Errorf("样本秒=%d, 期望 %d（单调，不覆盖）", ts, base/1000+2)
	}
}

func TestLookupErrorTriggersRepairThrottled(t *testing.T) {
	env := newEnv(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { env.coll.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := env.coll.Snapshot(); st.Error != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := env.coll.Snapshot()
	if st.Error == "" {
		t.Fatal("Lookup 后 last_error 应非空")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未退出")
	}
	if env.backend.repairs < 1 {
		t.Errorf("应触发过 repair, got %d", env.backend.repairs)
	}
}

// ---- nftables-only（v3.0.9）：生产构造器只能实例化 nft 后端 ----------------
//
// 旧实现是 firewall.New(cfg.Backend, cfg.NftConf, cfg.IptScript)，会按
// backend=auto/iptables 动态选择。现在只有一条路径：firewall.NewNft。
func TestNewCollectorAlwaysUsesNft(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "traffic.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := &config.Config{
		DB: dbPath, NodesFile: filepath.Join(dir, "nodes.json"),
		NftConf: filepath.Join(dir, "nft.conf"),
		Listen:  "127.0.0.1", Port: 8080, Interval: 2, TZ: "UTC",
	}
	c := NewCollector(cfg, db)
	if _, ok := c.backend.(*firewall.Nft); !ok {
		t.Fatalf("生产采集器后端必须是 *firewall.Nft, got %T", c.backend)
	}
	if got := c.BackendName(); got != "nft" || firewall.BackendName != "nft" {
		t.Errorf("BackendName 必须恒为 nft（API schema 兼容）, got %q / 常量 %q",
			got, firewall.BackendName)
	}
}
