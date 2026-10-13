package notify

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []Message
	err  error
}

func (f *fakeSender) Name() string { return "fake" }

func (f *fakeSender) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func testConfig() *config.Config {
	return &config.Config{
		TZ: "UTC",
		Notify: &config.NotifyConfig{
			Enabled:        true,
			Type:           config.ChannelFeishu,
			Webhook:        "https://open.feishu.cn/open-apis/bot/v2/hook/test",
			DailyAt:        "00:05",
			TimeoutSeconds: 8,
		},
	}
}

func newTestManager(t *testing.T) (*Manager, *database.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(database.Schema); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	m, err := NewManager(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("管理器不应为 nil")
	}
	// 重试退避在测试里等不起，注入极短等待。
	m.backoffFn = func(int) time.Duration { return time.Millisecond }
	fake := &fakeSender{}
	m.SetSender(fake)
	return m, db
}

func senderOf(t *testing.T, m *Manager) *fakeSender {
	t.Helper()
	f, ok := m.sender.(*fakeSender)
	if !ok {
		t.Fatal("测试发送器类型不符")
	}
	return f
}

func insertDaily(t *testing.T, db *database.DB, day string, rx, tx int64) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,0,0) "+
			"ON CONFLICT(day,scope) DO UPDATE SET rx=excluded.rx, tx=excluded.tx",
		day, "node:test", rx, tx)
	if err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// ---- 快照 ----

// TestSnapshotDailyExcludesToday 确认日报不会把"刚开始的今天"混进历史。
func TestSnapshotDailyExcludesToday(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-12", 100, 50)
	insertDaily(t, db, "2026-10-13", 200, 100)
	insertDaily(t, db, "2026-10-14", 5, 5) // 今天刚开始

	slot := time.Date(2026, 10, 14, 0, 5, 0, 0, time.UTC)
	s, err := BuildSnapshot(m.cfg, db.DB, KindDaily, "2026-10-13", slot)
	if err != nil {
		t.Fatal(err)
	}
	if s.Focus.ByteTotal() != 300 {
		t.Errorf("主体应是 10-13 的 300 字节, 实得 %d", s.Focus.ByteTotal())
	}
	if len(s.Days) != 2 {
		t.Fatalf("历史应有 2 天（不含今天）, 实得 %d", len(s.Days))
	}
	if s.Days[0].Day != "2026-10-13" {
		t.Errorf("历史应倒序，首行是 10-13, 实得 %s", s.Days[0].Day)
	}
	if s.Grand.ByteTotal() != 450 {
		t.Errorf("累计应为 450, 实得 %d", s.Grand.ByteTotal())
	}
}

// TestSnapshotRecoverIncludesToday 确认补推能看到当天（进行中）的数据。
func TestSnapshotRecoverIncludesToday(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 100, 50)
	insertDaily(t, db, "2026-10-14", 700, 300)

	slot := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	s, err := BuildSnapshot(m.cfg, db.DB, KindRecover, "2026-10-14", slot)
	if err != nil {
		t.Fatal(err)
	}
	if s.Focus.ByteTotal() != 1000 {
		t.Errorf("主体应为当天 1000 字节, 实得 %d", s.Focus.ByteTotal())
	}
	if len(s.Days) != 2 || !s.Days[0].InProgress {
		t.Fatalf("当天应出现在历史首位且标记进行中: %+v", s.Days)
	}
	if s.Days[1].InProgress {
		t.Error("昨天不该标记进行中")
	}
}

// ---- 调度 ----

func TestTickBeforeDailyAtDoesNothing(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 1, 2)
	m.now = func() time.Time { return time.Date(2026, 10, 14, 0, 3, 0, 0, time.UTC) }
	m.tick(context.Background())
	if n := senderOf(t, m).count(); n != 0 {
		t.Errorf("未到时刻不应发送, 实得 %d 条", n)
	}
}

func TestTickSendsDailyAndIsIdempotent(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 111, 222)
	m.now = func() time.Time { return time.Date(2026, 10, 14, 0, 5, 0, 0, time.UTC) }

	m.tick(context.Background())
	sent := senderOf(t, m).sent
	if len(sent) != 1 {
		t.Fatalf("应发送 1 条, 实得 %d", len(sent))
	}
	if sent[0].Event != EventDaily {
		t.Errorf("事件应为 daily, 实得 %s", sent[0].Event)
	}
	if m.metaGet(metaKeyDailyDay) != "2026-10-13" {
		t.Errorf("应记录已推日期, 实得 %q", m.metaGet(metaKeyDailyDay))
	}

	// 同一分钟内重复 tick 不得重复发送（幂等）
	m.tick(context.Background())
	if n := senderOf(t, m).count(); n != 1 {
		t.Errorf("重复 tick 不应重复发送, 实得 %d 条", n)
	}
}

// TestTickFailureDoesNotRecordDay 确认发送失败时不写幂等键，
// 否则该天会永久缺失（次日不会重试，也永远不再补发）。
func TestTickFailureDoesNotRecordDay(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 111, 222)
	m.now = func() time.Time { return time.Date(2026, 10, 14, 0, 5, 0, 0, time.UTC) }
	m.SetSender(&fakeSender{err: fmt.Errorf("boom")})

	m.tick(context.Background())
	if got := m.metaGet(metaKeyDailyDay); got != "" {
		t.Errorf("失败时不应记录已推日期, 实得 %q", got)
	}
}

// TestRecoverPushOnResidualMarker 覆盖"上次异常退出"的补推路径。
func TestRecoverPushOnResidualMarker(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-14", 700, 300)
	m.now = func() time.Time { return time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC) }

	m.recoverPush(context.Background())
	sent := senderOf(t, m).sent
	if len(sent) != 1 {
		t.Fatalf("应补推 1 条, 实得 %d", len(sent))
	}
	if sent[0].Event != EventRecover {
		t.Errorf("事件应为 recover, 实得 %s", sent[0].Event)
	}

	// 同一天再次启动不再重复补推（防止反复重启刷屏）
	m.recoverPush(context.Background())
	if n := senderOf(t, m).count(); n != 1 {
		t.Errorf("同一天不应重复补推, 实得 %d", n)
	}
}

func TestRecoverPushSkippedWithoutTodayData(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 700, 300) // 只有昨天
	m.now = func() time.Time { return time.Date(2026, 10, 14, 0, 20, 0, 0, time.UTC) }

	m.recoverPush(context.Background())
	if n := senderOf(t, m).count(); n != 0 {
		t.Errorf("当天无流量时不应补推, 实得 %d", n)
	}
}

// TestRunClearsMarkerOnGracefulExit 是"异常退出检测"的关键前提：
// 正常收尾必须清除标记，否则每次重启都会误判为崩溃。
func TestRunClearsMarkerOnGracefulExit(t *testing.T) {
	m, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitFor(t, "运行标记写入", func() bool { return m.metaGet(metaKeyRunning) != "" })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未在取消后退出")
	}
	if got := m.metaGet(metaKeyRunning); got != "" {
		t.Errorf("正常退出必须清除运行标记, 实得 %q", got)
	}
}

// TestRunRecoversAfterCrashMarker 确认标记残留时启动即补推。
func TestRunRecoversAfterCrashMarker(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-14", 700, 300)
	m.now = func() time.Time { return time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC) }
	// 模拟上次崩溃：标记残留
	if err := m.metaSet(metaKeyRunning, "2026-10-14T09:00:00Z"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, "崩溃后补推", func() bool { return senderOf(t, m).count() == 1 })
	cancel()
	<-done

	if got := senderOf(t, m).sent[0].Event; got != EventRecover {
		t.Errorf("应为补推事件, 实得 %s", got)
	}
}

// TestNotifierWritesOnlyMeta 是核心不变量：
// 推送是旁路组件，绝不能改写计费数据。
func TestNotifierWritesOnlyMeta(t *testing.T) {
	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 111, 222)
	insertDaily(t, db, "2026-10-14", 333, 444)
	m.now = func() time.Time { return time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC) }

	before := tableDump(t, db, "daily")
	beforeTotals := tableDump(t, db, "totals")

	m.tick(context.Background())
	m.recoverPush(context.Background())
	m.now = func() time.Time { return time.Date(2026, 10, 14, 0, 5, 0, 0, time.UTC) }
	m.tick(context.Background())

	if after := tableDump(t, db, "daily"); after != before {
		t.Errorf("daily 表被改写:\n前=%s\n后=%s", before, after)
	}
	if after := tableDump(t, db, "totals"); after != beforeTotals {
		t.Errorf("totals 表被改写:\n前=%s\n后=%s", beforeTotals, after)
	}
}

// tableDump 返回一张表的全部内容（按稳定顺序拼成字符串）。
func tableDump(t *testing.T, db *database.DB, table string) string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
	if err != nil {
		// 表可能不存在，返回空串即可（测试关心的是"是否被改写"）
		return ""
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var b []byte
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		b = append(b, []byte(fmt.Sprint(vals))...)
		b = append(b, '\n')
	}
	var _ sql.NullString
	return string(b)
}
