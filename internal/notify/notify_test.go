package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// TestMain 静音日志：这些测试会刻意走失败/熔断/补偿路径，
// 日志噪音会把真正的失败信息淹没。
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// fakeChannel 是可编程的假渠道。
type fakeChannel struct {
	name   string
	events []string
	caps   Capabilities

	mu    sync.Mutex
	calls int
	err   error
	got   []Message
}

func (f *fakeChannel) Name() string       { return f.name }
func (f *fakeChannel) Caps() Capabilities { return f.caps }

func (f *fakeChannel) HasEvent(ev string) bool {
	// 与真实渠道语义一致：未显式限定 events 即订阅全部。
	if len(f.events) == 0 {
		return true
	}
	for _, e := range f.events {
		if e == ev {
			return true
		}
	}
	return false
}

func (f *fakeChannel) Send(ctx context.Context, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got = append(f.got, msg)
	return f.err
}

func (f *fakeChannel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeChannel) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// testNow 是所有 Manager 测试共用的固定时刻。
func testNow() time.Time { return time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC) }

func newTestManager(t *testing.T, chans ...Channel) (*Manager, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "traffic.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{
		DB: dbPath, NodesFile: filepath.Join(dir, "nodes.json"),
		TZ: "UTC", Interval: 2,
		Notify: &config.NotifyConfig{
			Enabled: true, IntervalMinutes: 30, DailyAt: "00:05", TimeoutSeconds: 5,
		},
	}
	m, err := NewManager(cfg, db, nil)
	if err != nil {
		t.Fatalf("构造 Manager 失败: %v", err)
	}
	if m == nil {
		t.Fatal("启用的配置不应返回 nil Manager")
	}
	m.channels = chans
	m.now = testNow
	// 生产退避是 1s/4s；测试里没必要真等，注入极短延迟。
	m.backoffFn = func(int) time.Duration { return time.Millisecond }
	return m, db
}

func mustExec(t *testing.T, db *database.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("执行 %s 失败: %v", q, err)
	}
}

// TestNewManagerDisabledIsNil 保证"不配置就完全关闭"：不启动 goroutine、不建连接。
func TestNewManagerDisabledIsNil(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, nc := range []*config.NotifyConfig{nil, {Enabled: false}} {
		m, err := NewManager(&config.Config{TZ: "UTC", Notify: nc}, db, nil)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if m != nil {
			t.Error("未启用时应返回 nil Manager")
		}
	}
}

func TestCurrentSlotAlignment(t *testing.T) {
	m, _ := newTestManager(t)
	cases := []struct{ hhmm, want string }{
		{"00:00", "2026-10-12T00:00"},
		{"00:05", "2026-10-12T00:00"},
		{"14:07", "2026-10-12T14:00"},
		{"14:29", "2026-10-12T14:00"},
		{"14:30", "2026-10-12T14:30"},
		{"14:59", "2026-10-12T14:30"},
		{"23:59", "2026-10-12T23:30"},
	}
	for _, c := range cases {
		h, mi := parseHHMM(c.hhmm)
		at := time.Date(2026, 10, 12, h, mi, 0, 0, m.loc)
		if got := m.currentSlot(at); got != c.want {
			t.Errorf("currentSlot(%s) = %s, 期望 %s", c.hhmm, got, c.want)
		}
	}
}

// TestHeartbeatIsIdempotentWithinSlot：进程重启后不得重复推送同一槽位。
func TestHeartbeatIsIdempotentWithinSlot(t *testing.T) {
	fc := &fakeChannel{name: "f", caps: Capabilities{MaxBytes: 4096}}
	m, _ := newTestManager(t, fc)
	// 预先标记昨天日结已发，隔离出纯心跳路径。
	if err := m.metaSet(metaKeyDailyDay, "2026-10-11"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	m.tick(ctx)
	after := fc.callCount()
	if after != 1 {
		t.Fatalf("首轮应只发 1 次心跳, 实得 %d", after)
	}
	m.tick(ctx)
	if got := fc.callCount(); got != after {
		t.Errorf("同一槽位内重复发送: %d -> %d", after, got)
	}

	// 跨到下一个槽位后应再次发送。
	m.now = func() time.Time { return testNow().Add(30 * time.Minute) }
	m.tick(ctx)
	if got := fc.callCount(); got != after+1 {
		t.Errorf("进入下一槽位应再发一次: %d -> %d", after, got)
	}
}

// TestNotifierTouchesOnlyMeta 是本包最重要的数据安全断言：
// 一个旁路的推送组件绝不该有机会污染计费数据。
func TestNotifierTouchesOnlyMeta(t *testing.T) {
	fc := &fakeChannel{name: "f", caps: Capabilities{MaxBytes: 4096}}
	m, db := newTestManager(t, fc)

	mustExec(t, db, "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)",
		"2026-10-11", "node:a", 111, 222, 3, 4)
	mustExec(t, db, "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?)",
		"2026-10-12", "node:a", 5, 6, 1, 1)
	mustExec(t, db, "INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?)",
		"node:a", 111, 222, 3, 4)
	mustExec(t, db, "INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(?,?,?,?,?,1)",
		1000, "node:a", 1, 2, 1000)
	mustExec(t, db, "INSERT INTO counter_state(name,last_bytes,last_pkts,updated_at) VALUES(?,?,?,?)",
		"sbx_a_rx", 10, 1, 1000)
	mustExec(t, db, "INSERT INTO node_policy(node_id) VALUES(?)", "a")

	before := dumpAccounting(t, db)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		m.tick(ctx)
	}
	after := dumpAccounting(t, db)

	if before != after {
		t.Errorf("推送过程修改了计费表:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func dumpAccounting(t *testing.T, db *database.DB) string {
	t.Helper()
	var sb strings.Builder
	for _, q := range []string{
		"SELECT day,scope,rx,tx,rx_pkts,tx_pkts FROM daily ORDER BY day,scope",
		"SELECT scope,rx,tx,rx_pkts,tx_pkts FROM totals ORDER BY scope",
		"SELECT ts,scope,rx,tx,duration_ms,valid FROM samples ORDER BY ts,scope",
		"SELECT name,last_bytes,last_pkts,updated_at FROM counter_state ORDER BY name",
		"SELECT node_id,paused FROM node_policy ORDER BY node_id",
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("查询失败 %s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			sb.WriteString(fmt.Sprint(vals...))
			sb.WriteByte('\n')
		}
		rows.Close()
	}
	return sb.String()
}

func TestDailyFailureEntersPendingQueue(t *testing.T) {
	fc := &fakeChannel{name: "f", caps: Capabilities{MaxBytes: 4096}, err: errors.New("boom")}
	m, _ := newTestManager(t, fc)
	if err := m.metaSet(metaKeyPending, ""); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	m.sendDaily(ctx, "2026-10-11")
	days := m.pendingDays()
	if len(days) != 1 || days[0] != "2026-10-11" {
		t.Fatalf("日结失败应进入补偿队列, 实得 %v", days)
	}

	fc.setErr(nil)
	m.sendDaily(ctx, "2026-10-11")
	if got := m.pendingDays(); len(got) != 0 {
		t.Errorf("补发成功后应出队, 实得 %v", got)
	}
	if got := m.lastDailyDay(); got != "2026-10-11" {
		t.Errorf("应记录已发日期, 实得 %q", got)
	}
}

// TestPartialChannelFailureStillCountsAsSuccess：多渠道互为冗余，
// 单个渠道失败不该让日结被反复补发。
func TestPartialChannelFailureStillCountsAsSuccess(t *testing.T) {
	ok := &fakeChannel{name: "ok", caps: Capabilities{MaxBytes: 4096}}
	bad := &fakeChannel{name: "bad", caps: Capabilities{MaxBytes: 4096}, err: errors.New("boom")}
	m, _ := newTestManager(t, ok, bad)
	if err := m.metaSet(metaKeyPending, ""); err != nil {
		t.Fatal(err)
	}

	m.sendDaily(context.Background(), "2026-10-11")
	if got := m.lastDailyDay(); got != "2026-10-11" {
		t.Error("只要有一个渠道成功就应记录已发")
	}
	if got := m.pendingDays(); len(got) != 0 {
		t.Errorf("不应进入补偿队列, 实得 %v", got)
	}
}

func TestBreakerOpensAfterRepeatedFailures(t *testing.T) {
	fc := &fakeChannel{name: "f", caps: Capabilities{MaxBytes: 4096}, err: errors.New("boom")}
	m, _ := newTestManager(t, fc)
	ctx := context.Background()
	snap := mkSnapshot(KindHeartbeat, 1)

	for i := 0; i < breakerThreshold; i++ {
		m.dispatch(ctx, config.EventHeartbeat, snap)
	}
	before := fc.callCount()
	m.dispatch(ctx, config.EventHeartbeat, snap)
	if got := fc.callCount(); got != before {
		t.Errorf("熔断后不应再发起请求: %d -> %d", before, got)
	}

	// 静默期过后自动恢复。
	m.now = func() time.Time { return testNow().Add(breakerCooldown + time.Minute) }
	m.dispatch(ctx, config.EventHeartbeat, snap)
	if got := fc.callCount(); got <= before {
		t.Errorf("静默期结束应恢复请求: %d -> %d", before, got)
	}
}

func TestChannelEventFilter(t *testing.T) {
	hbOnly := &fakeChannel{name: "hb", events: []string{config.EventHeartbeat}, caps: Capabilities{MaxBytes: 4096}}
	m, _ := newTestManager(t, hbOnly)
	if err := m.metaSet(metaKeyPending, ""); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	m.sendDaily(ctx, "2026-10-11")
	if hbOnly.callCount() != 0 {
		t.Fatal("未订阅 daily 的渠道不应被调用")
	}
	m.sendHeartbeat(ctx, "2026-10-12T14:30")
	if hbOnly.callCount() != 1 {
		t.Errorf("订阅 heartbeat 的渠道应被调用一次, 实得 %d", hbOnly.callCount())
	}
}

// TestSendTestIgnoresSubscription：自检的目的就是验证每一个渠道。
func TestSendTestIgnoresSubscription(t *testing.T) {
	dailyOnly := &fakeChannel{name: "d", events: []string{config.EventDaily}, caps: Capabilities{MaxBytes: 4096}}
	m, _ := newTestManager(t, dailyOnly)

	results := m.SendTest(context.Background(), config.EventHeartbeat, "2026-10-12")
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("自检应覆盖所有渠道: %+v", results)
	}
	if dailyOnly.callCount() != 1 {
		t.Error("SendTest 应无视 events 订阅")
	}
	for _, msg := range dailyOnly.got {
		if !strings.Contains(msg.Title, "[测试]") {
			t.Errorf("测试消息应带 [测试] 标记: %q", msg.Title)
		}
	}
}

func TestSendTestReportsChannelFailure(t *testing.T) {
	bad := &fakeChannel{name: "bad", caps: Capabilities{MaxBytes: 4096}, err: errors.New("nope")}
	m, _ := newTestManager(t, bad)
	results := m.SendTest(context.Background(), config.EventHeartbeat, "2026-10-12")
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("渠道失败应如实上报: %+v", results)
	}
}

func TestCatchUpEnqueuesMissedDays(t *testing.T) {
	fc := &fakeChannel{name: "f", caps: Capabilities{MaxBytes: 4096}, err: errors.New("down")}
	m, _ := newTestManager(t, fc)
	// 上次成功发的是 3 天前 → 应补 10-10 与 10-11。
	if err := m.metaSet(metaKeyDailyDay, "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if err := m.metaSet(metaKeyPending, ""); err != nil {
		t.Fatal(err)
	}
	m.catchUpOnce(testNow().In(m.loc))

	days := m.pendingDays()
	if len(days) != 2 || days[0] != "2026-10-10" || days[1] != "2026-10-11" {
		t.Fatalf("应补 2026-10-10 与 2026-10-11, 实得 %v", days)
	}
}

func TestCatchUpIsBounded(t *testing.T) {
	fc := &fakeChannel{name: "f", caps: Capabilities{MaxBytes: 4096}}
	m, _ := newTestManager(t, fc)
	// 上次成功是半年前：不应把历史全部推出去。
	if err := m.metaSet(metaKeyDailyDay, "2026-04-01"); err != nil {
		t.Fatal(err)
	}
	if err := m.metaSet(metaKeyPending, ""); err != nil {
		t.Fatal(err)
	}
	m.catchUpOnce(testNow().In(m.loc))

	if got := len(m.pendingDays()); got > maxCatchUpDays {
		t.Errorf("补发天数应被限制在 %d 天内, 实得 %d", maxCatchUpDays, got)
	}
}

func TestPendingQueueIsBoundedAndSorted(t *testing.T) {
	m, _ := newTestManager(t)
	for i := 0; i < maxPendingDays+3; i++ {
		m.addPending(fmt.Sprintf("2026-10-%02d", i+1))
	}
	days := m.pendingDays()
	if len(days) != maxPendingDays {
		t.Fatalf("补偿队列应被限制在 %d 天, 实得 %d", maxPendingDays, len(days))
	}
	for i := 1; i < len(days); i++ {
		if days[i-1] >= days[i] {
			t.Errorf("补偿队列应保持有序: %v", days)
			break
		}
	}
	// 重复加入不应产生重复项。
	m.addPending(days[0])
	if got := len(m.pendingDays()); got != maxPendingDays {
		t.Errorf("重复加入不应改变长度: %d", got)
	}
}

func TestDailyDueRespectsDailyAtAndAlreadySent(t *testing.T) {
	m, _ := newTestManager(t)

	// 未到 daily_at（now 00:00 < 00:05）→ 不该发
	early := time.Date(2026, 10, 12, 0, 0, 0, 0, m.loc)
	if _, ok := m.dailyDue(early); ok {
		t.Error("未到 daily_at 不应触发日结")
	}
	// 到点且未发过 → 目标是前一天
	day, ok := m.dailyDue(testNow().In(m.loc))
	if !ok || day != "2026-10-11" {
		t.Fatalf("应触发 2026-10-11 的日结, 实得 ok=%v day=%s", ok, day)
	}
	// 已发过 → 不再触发
	if err := m.metaSet(metaKeyDailyDay, "2026-10-11"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.dailyDue(testNow().In(m.loc)); ok {
		t.Error("已发送过的日期不应重复触发")
	}
}

// TestBuildSnapshotUsesConfiguredTimezone：day 按 cfg.TZ 计算，
// 时刻也必须按 cfg.TZ 呈现，否则跨时区部署下"日期"与"时刻"会互相矛盾。
func TestBuildSnapshotUsesConfiguredTimezone(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := &config.Config{
		DB: filepath.Join(dir, "t.db"), NodesFile: filepath.Join(dir, "nodes.json"),
		TZ: "Asia/Shanghai", Interval: 2,
	}
	// UTC 16:00 == 上海次日 00:00
	slot := time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC)
	snap, err := BuildSnapshot(cfg, db.DB, nil, KindHeartbeat, "2026-10-13", slot)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.HHMM(); got != "00:00" {
		t.Errorf("时刻应按配置时区呈现, 实得 %s", got)
	}
	if got := snap.Title(); got != "10-13 00:00" {
		t.Errorf("标题应按配置时区呈现, 实得 %s", got)
	}
}

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
	}{
		{"00:05", 0, 5},
		{"23:59", 23, 59},
		{"garbage", 0, 0},
		{"99:99", 0, 0},
	}
	for _, c := range cases {
		h, m := parseHHMM(c.in)
		if h != c.h || m != c.m {
			t.Errorf("parseHHMM(%q) = %d:%d, 期望 %d:%d", c.in, h, m, c.h, c.m)
		}
	}
}

func TestBackoffGrowsAndJitters(t *testing.T) {
	d1, d2 := backoff(1), backoff(2)
	if d1 <= 0 || d2 <= d1 {
		t.Errorf("退避应递增: %v -> %v", d1, d2)
	}
	// 抖动是 ±20%，多次采样不应完全相同。
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[backoff(1)] = true
	}
	if len(seen) < 2 {
		t.Error("退避应带抖动，避免多实例同时重试")
	}
}
