package notify

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

const (
	// maxSendAttempts 是单次发送的最大尝试次数（首次 + 2 次重试）。
	maxSendAttempts = 3
	// meta 表中的状态键。这是本包唯一写入的位置（其余全部只读）。
	//
	// metaKeyRunning 兼作"上次是否正常退出"的判据：进程正常收尾时清除，
	// 崩溃/断电/SIGKILL 时残留下来，下次启动据此触发一次补推。
	metaKeyRunning    = "notify_running"
	metaKeyDailyDay   = "notify_daily_day"
	metaKeyRecoverDay = "notify_recover_day"
)

// Manager 负责调度、渲染与发送。
//
// 并发模型：单个 goroutine 执行调度循环，与采集器严格单向——只读数据库与采集器状态，
// 从不反向调用。
type Manager struct {
	cfg    *config.Config
	ncfg   *config.NotifyConfig
	db     *database.DB
	sender Sender
	loc    *time.Location
	now    func() time.Time
	// backoffFn 是重试退避函数。生产恒为 backoff；测试注入极短退避以加速用例。
	backoffFn func(int) time.Duration

	done     chan struct{}
	doneOnce sync.Once
}

// NewManager 构造推送管理器。未配置或未启用时返回 (nil, nil)——
// 调用方据此不启动 goroutine，保证"不配置就完全关闭"。
func NewManager(cfg *config.Config, db *database.DB) (*Manager, error) {
	ncfg := cfg.Notify
	if ncfg == nil || !ncfg.Enabled {
		return nil, nil
	}
	client := &http.Client{
		// 超时同时由 http.Client 与每次调用的 context 兜底。
		Timeout: time.Duration(ncfg.TimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			// 不启用压缩：响应体本就很小，省去一层不确定性。
			DisableCompression: true,
		},
	}
	sender, err := NewSender(ncfg, client)
	if err != nil {
		return nil, err
	}
	return &Manager{
		cfg:       cfg,
		ncfg:      ncfg,
		db:        db,
		sender:    sender,
		loc:       traffic.Location(cfg.TZ),
		now:       time.Now,
		backoffFn: backoff,
		done:      make(chan struct{}),
	}, nil
}

// Done 在 Run 返回后被关闭。
func (m *Manager) Done() <-chan struct{} { return m.done }

// SetClock 注入时钟（仅测试使用）。
func (m *Manager) SetClock(fn func() time.Time) { m.now = fn }

// SetSender 替换推送目标（仅测试使用）。
func (m *Manager) SetSender(s Sender) { m.sender = s }

// Run 常驻调度循环：每分钟对齐检查一次是否到了日报时刻。
//
// 用"每分钟对齐检查"而不是一次性 Timer：跨周期挂起（休眠、时钟跳变）后能自动
// 从当前时间重新对齐，不会因为算错一次而永久错位。
func (m *Manager) Run(ctx context.Context) {
	defer m.doneOnce.Do(func() { close(m.done) })

	// 异常退出检测必须在覆盖标记之前做。
	abnormal := m.metaGet(metaKeyRunning) != ""
	if err := m.metaSet(metaKeyRunning, m.now().In(m.loc).Format(time.RFC3339)); err != nil {
		slog.Warn("写入推送运行标记失败, 异常补推检测将失效", "err", err)
	}
	// 正常收尾（SIGTERM 触发的 ctx 取消）会走到这里清除标记。
	// 注册顺序在 done 之后，因此先清标记、再通知退出完成。
	defer func() {
		if err := m.metaClear(metaKeyRunning); err != nil {
			slog.Warn("清除推送运行标记失败, 下次启动可能误判为异常退出", "err", err)
		}
	}()

	slog.Info("流量日报已启用", "daily_at", m.ncfg.DailyAt, "channel", m.sender.Name())

	if abnormal {
		m.recoverPush(ctx)
	}

	for {
		now := m.now()
		next := now.In(m.loc).Truncate(time.Minute).Add(time.Minute)
		wait := next.Sub(now.In(m.loc))
		if wait <= 0 {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		m.tick(ctx)
	}
}

// recoverPush 在上次异常退出后补推一条"今日截至当前"的卡片。
//
// 它解决的是：服务器在一天中途崩溃/断电，当天数据虽然还在库里，但要等到次日
// 才会被日报带出来。补推让用户立刻知道"服务器重启过"以及"今天已经用了多少"。
func (m *Manager) recoverPush(ctx context.Context) {
	now := m.now().In(m.loc)
	today := traffic.TodayAt(m.cfg.TZ, now)
	if m.metaGet(metaKeyRecoverDay) == today {
		// 同一天只补推一次，避免反复重启造成刷屏。
		return
	}
	snap, err := BuildSnapshot(m.cfg, m.db.DB, KindRecover, today, now)
	if err != nil {
		slog.Warn("补推快照构建失败", "err", SafeErr(err))
		return
	}
	if snap.Focus.ByteTotal() == 0 {
		// 刚过零点的重启没有可汇报的内容，静默跳过。
		slog.Info("检测到上次异常退出, 但当天尚无流量, 跳过补推", "day", today)
		return
	}
	if err := m.dispatch(ctx, EventRecover, snap); err != nil {
		slog.Warn("异常补推失败", "day", today, "err", SafeErr(err))
		return
	}
	if err := m.metaSet(metaKeyRecoverDay, today); err != nil {
		slog.Warn("记录补推日期失败", "err", err)
	}
	slog.Info("已补推当天流量", "day", today, "bytes", snap.Focus.ByteTotal())
}

// tick 执行一次检查：到点且未推过则发送日报。
func (m *Manager) tick(ctx context.Context) {
	now := m.now().In(m.loc)
	day, ok := m.dailyDue(now)
	if !ok {
		return
	}
	snap, err := BuildSnapshot(m.cfg, m.db.DB, KindDaily, day, now)
	if err != nil {
		slog.Warn("日报快照构建失败", "day", day, "err", SafeErr(err))
		return
	}
	if err := m.dispatch(ctx, EventDaily, snap); err != nil {
		// 刻意不做补偿队列：卡片本身携带全部历史，本次失败的那一天
		// 会出现在明天的卡片里。为一次的失败引入重试队列，
		// 换来的是"渠道恢复后连发多条"的消息风暴，不划算。
		slog.Error("日报推送失败, 该日数据仍会出现在明天的卡片中",
			"day", day, "err", SafeErr(err))
		return
	}
	if err := m.metaSet(metaKeyDailyDay, day); err != nil {
		slog.Warn("记录日报日期失败, 重启后可能重复推送", "err", err)
	}
	slog.Info("日报已送达", "day", day, "days", len(snap.Days))
}

// dailyTimeReached 判断当天是否已到配置的发送时刻。
func (m *Manager) dailyTimeReached(now time.Time) bool {
	h, mi := parseHHMM(m.ncfg.DailyAt)
	at := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, m.loc)
	return !now.Before(at)
}

// dailyDue 判断此刻是否该发日报，返回目标日（前一日）。
func (m *Manager) dailyDue(now time.Time) (string, bool) {
	if !m.dailyTimeReached(now) {
		return "", false
	}
	day := now.AddDate(0, 0, -1).Format("2006-01-02")
	// 前置检查：目标日必须已经冻结。已经过去的一天不会再被采集器改写
	//（除非用户改了 tz），这是"日报即终值"的前提。
	if traffic.TodayAt(m.cfg.TZ, now) == day {
		return "", false
	}
	if m.metaGet(metaKeyDailyDay) == day {
		return "", false
	}
	return day, true
}

func parseHHMM(s string) (int, int) {
	var h, mi int
	// 格式已由 config.Validate 保证为 HH:MM，这里解析失败只会得到 0:00。
	_, _ = fmt.Sscanf(s, "%d:%d", &h, &mi)
	if h < 0 || h > 23 {
		h = 0
	}
	if mi < 0 || mi > 59 {
		mi = 0
	}
	return h, mi
}

// dispatch 渲染并发送一次，带退避重试。
func (m *Manager) dispatch(ctx context.Context, event string, snap *Snapshot) error {
	msg := Message{
		Event: event,
		Title: cardTitle(snap),
		Card:  BuildCard(snap),
	}
	var lastErr error
	for attempt := 0; attempt < maxSendAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.retryDelay(attempt)):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, time.Duration(m.ncfg.TimeoutSeconds)*time.Second)
		err := m.sender.Send(cctx, msg)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

// retryDelay 返回第 attempt 次重试前的等待时长。
func (m *Manager) retryDelay(attempt int) time.Duration {
	if m.backoffFn == nil {
		return backoff(attempt)
	}
	return m.backoffFn(attempt)
}

// backoff 返回第 attempt 次重试前的等待时长（1s、4s……）并加 ±20% 抖动，
// 避免多实例同时重试造成脉冲。
func backoff(attempt int) time.Duration {
	d := time.Second
	for i := 1; i < attempt; i++ {
		d *= 4
	}
	jitter := time.Duration(rand.Int63n(int64(d)/5 + 1))
	return d - d/5 + jitter
}

// ---- meta 状态（本包唯一的写入点） ----

func (m *Manager) metaGet(key string) string {
	var v sql.NullString
	if err := m.db.QueryRow("SELECT v FROM meta WHERE k=?", key).Scan(&v); err != nil {
		return ""
	}
	if !v.Valid {
		return ""
	}
	return v.String
}

func (m *Manager) metaSet(key, value string) error {
	_, err := m.db.Exec(
		"INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", key, value)
	return err
}

func (m *Manager) metaClear(key string) error {
	_, err := m.db.Exec("DELETE FROM meta WHERE k=?", key)
	return err
}

// ---- 自检 ----

// SendTest 立即发送一条测试卡片（标注 [测试]，避免与真实日报混淆）。
//
// 与日常推送的差异（有意为之）：不受幂等状态影响、不写 meta、失败不重试队列化——
// 自检的目的就是当场验证渠道当前是否可用。
func (m *Manager) SendTest(ctx context.Context) error {
	now := m.now().In(m.loc)
	day := traffic.TodayAt(m.cfg.TZ, now)
	snap, err := BuildSnapshot(m.cfg, m.db.DB, KindRecover, day, now)
	if err != nil {
		return err
	}
	snap.Test = true
	return m.dispatch(ctx, EventTest, snap)
}
