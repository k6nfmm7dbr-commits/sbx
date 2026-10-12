package notify

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

const (
	// maxSendAttempts 是单渠道单次发送的最大尝试次数（首次 + 2 次重试）。
	maxSendAttempts = 3
	// breakerThreshold / breakerCooldown：同一渠道连续失败达到阈值后静默一段时间。
	// 目的是避免"凭据失效"这类永久错误每 30 分钟刷一次日志与请求。
	breakerThreshold = 5
	breakerCooldown  = 30 * time.Minute
	// maxPendingDays 是日结补偿队列的长度上限。
	maxPendingDays = 7
	// maxCatchUpDays 是启动时最多补发的天数（避免长期停机后一次刷屏）。
	maxCatchUpDays = 3
	// meta 表中的三个状态键。这是本包唯一写入的位置（其余全部只读）。
	metaKeyHBSlot   = "notify_hb_slot"
	metaKeyDailyDay = "notify_daily_day"
	metaKeyPending  = "notify_pending_daily"
)

// Manager 负责调度、渲染、扇出与补偿。
//
// 并发模型：单个 goroutine 执行 tick；熔断状态经 mutex 暴露。
// 与采集器的关系是严格单向的——它只读数据库与采集器快照，从不反向调用。
type Manager struct {
	cfg      *config.Config
	ncfg     *config.NotifyConfig
	db       *database.DB
	live     traffic.LiveSource
	loc      *time.Location
	channels []Channel
	client   *http.Client
	now      func() time.Time
	// backoffFn 是重试退避函数。生产恒为 backoff；测试注入极短退避以加速用例。
	backoffFn func(int) time.Duration

	mu        sync.Mutex
	breaker   map[string]int64 // 渠道名 → 静默截止 Unix 秒
	failCount map[string]int
	caughtUp  bool

	done     chan struct{}
	doneOnce sync.Once
}

// NewManager 构造推送管理器。未配置或未启用时返回 (nil, nil)——
// 调用方据此不启动 goroutine，保证"不配置就完全关闭"。
func NewManager(cfg *config.Config, db *database.DB, live traffic.LiveSource) (*Manager, error) {
	ncfg := cfg.Notify
	if ncfg == nil || !ncfg.Enabled {
		return nil, nil
	}
	client := &http.Client{
		// 超时同时由 http.Client 与每次调用的 context 兜底。
		Timeout: time.Duration(ncfg.TimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			// 不启用压缩：响应体本就很小，省去一层不确定性。
			DisableCompression: true,
		},
	}
	channels := make([]Channel, 0, len(ncfg.Channels))
	for _, cc := range ncfg.Channels {
		ch, err := NewChannel(cc, client)
		if err != nil {
			return nil, fmt.Errorf("渠道 %s 构造失败: %w", cc.Name, err)
		}
		channels = append(channels, ch)
	}
	return &Manager{
		cfg:       cfg,
		ncfg:      ncfg,
		db:        db,
		live:      live,
		loc:       traffic.Location(cfg.TZ),
		channels:  channels,
		client:    client,
		now:       time.Now,
		backoffFn: backoff,
		breaker:   map[string]int64{},
		failCount: map[string]int{},
		done:      make(chan struct{}),
	}, nil
}

// Done 在 Run 返回后被关闭。
func (m *Manager) Done() <-chan struct{} { return m.done }

// SetClock 注入时钟（仅测试使用）。
func (m *Manager) SetClock(fn func() time.Time) { m.now = fn }

// Run 常驻调度循环：对齐到分钟边界，每分钟检查一次是否有该发的东西。
//
// 用"每分钟对齐检查"而不是为心跳/日结各起一个 Ticker：
//   - 天然对齐整分（用户看到的消息时间规律，便于发现"少了什么"）；
//   - 跨周期挂起后自动从当前重新对齐，不补跑（与采集器同一策略）。
func (m *Manager) Run(ctx context.Context) {
	defer m.doneOnce.Do(func() { close(m.done) })

	slog.Info("流量推送已启用",
		"interval_minutes", m.ncfg.IntervalMinutes,
		"daily_at", m.ncfg.DailyAt,
		"channels", len(m.channels))

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

// tick 执行一次检查：补发 → 日结 → 心跳。三者的顺序有意如此——
// 先把欠的账还上，再处理当期。
func (m *Manager) tick(ctx context.Context) {
	now := m.now().In(m.loc)
	m.catchUpOnce(now)
	m.flushPending(ctx, now)

	if day, ok := m.dailyDue(now); ok {
		m.sendDaily(ctx, day)
	}

	slot := m.currentSlot(now)
	if m.hbSlotDone(slot) {
		return
	}
	m.sendHeartbeat(ctx, slot)
}

// ---- 槽位与日界 ----

// currentSlot 返回当前时间所属的心跳槽位标识，如 "2026-10-12T14:30"。
// 同一槽位内的任意时刻得到相同标识，因此天然幂等。
func (m *Manager) currentSlot(now time.Time) string {
	iv := m.ncfg.IntervalMinutes
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, m.loc)
	at := dayStart.Add(time.Duration(int(now.Sub(dayStart).Minutes())/iv*iv) * time.Minute)
	return at.Format("2006-01-02T15:04")
}

func (m *Manager) dailyTimeReached(now time.Time) bool {
	h, mi := parseHHMM(m.ncfg.DailyAt)
	at := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, m.loc)
	return !now.Before(at)
}

// dailyDue 判断此刻是否该发日结，返回目标日（前一日）。
func (m *Manager) dailyDue(now time.Time) (string, bool) {
	if !m.dailyTimeReached(now) {
		return "", false
	}
	day := now.AddDate(0, 0, -1).Format("2006-01-02")
	// 前置检查：目标日必须已经冻结。已经过去的一天不会再被 commitTick 改写
	// （除非用户改了 tz），这是"日结即终值"的前提。
	if traffic.TodayAt(m.cfg.TZ, now) == day {
		return "", false
	}
	if m.lastDailyDay() == day {
		return "", false
	}
	return day, true
}

func parseHHMM(s string) (int, int) {
	var h, mi int
	// 格式已由 config.Validate 保证为 HH:MM，这里解析失败只会得到 0:00。
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &mi)
	if h < 0 || h > 23 {
		h = 0
	}
	if mi < 0 || mi > 59 {
		mi = 0
	}
	return h, mi
}

// catchUpOnce 在首次 tick 时把停机期间欠下的日结补入补偿队列。
// 只做一次，避免每轮重复扫描。
func (m *Manager) catchUpOnce(now time.Time) {
	if m.caughtUp {
		return
	}
	m.caughtUp = true

	last := m.lastDailyDay()
	yesterday := now.AddDate(0, 0, -1)
	if !m.dailyTimeReached(now) {
		// 今天还没到日结点，则"最近一个应该发出去的日期"是前天。
		yesterday = now.AddDate(0, 0, -2)
	}
	if last == "" {
		// 首次启用：只补最近一天，不把历史全部推出去。
		m.addPending(yesterday.Format("2006-01-02"))
		return
	}
	start, err := time.ParseInLocation("2006-01-02", last, m.loc)
	if err != nil {
		return
	}
	start = start.AddDate(0, 0, 1)
	added := 0
	for d := start; !d.After(yesterday) && added < maxCatchUpDays; d = d.AddDate(0, 0, 1) {
		m.addPending(d.Format("2006-01-02"))
		added++
	}
	if added > 0 {
		slog.Info("检测到停机期间缺失的日结, 已入补偿队列", "days", added)
	}
}

// ---- 发送 ----

func (m *Manager) sendHeartbeat(ctx context.Context, slot string) {
	day := traffic.TodayAt(m.cfg.TZ, m.now())
	snap, err := BuildSnapshot(m.cfg, m.db.DB, m.live, KindHeartbeat, day, m.now())
	if err != nil {
		slog.Warn("心跳快照构建失败", "err", SafeErr(err))
		return
	}
	snap.Pending = m.pendingDays()

	ok := m.dispatch(ctx, config.EventHeartbeat, snap)
	if ok {
		if err := m.metaSet(metaKeyHBSlot, slot); err != nil {
			slog.Warn("记录心跳槽位失败", "err", err)
		}
	} else {
		// 心跳失败刻意不补发：IM 是"快照覆盖"语义，下个槽位会带上最新数据，
		// 排队重试只会在渠道恢复时造成消息风暴。
		slog.Warn("心跳全部渠道失败, 下个槽位将自然覆盖", "slot", slot)
	}
	m.pingHeartbeat(ctx, ok)
}

func (m *Manager) sendDaily(ctx context.Context, day string) {
	snap, err := BuildSnapshot(m.cfg, m.db.DB, m.live, KindDaily, day, m.now())
	if err != nil {
		slog.Warn("日结快照构建失败", "day", day, "err", SafeErr(err))
		return
	}
	// 日结不携带 pending 提示（它本身就是补偿动作）。
	if m.dispatch(ctx, config.EventDaily, snap) {
		if err := m.metaSet(metaKeyDailyDay, day); err != nil {
			slog.Warn("记录日结日期失败", "err", err)
		}
		m.removePending(day)
		slog.Info("日结已送达", "day", day)
		return
	}
	// 日结是不可再生的终值，必须进补偿队列由后续心跳/调度补发。
	m.addPending(day)
	slog.Error("日结全部渠道失败, 已入补偿队列", "day", day)
}

// flushPending 补发补偿队列中的日结，每次 tick 最多一条。
// 一次只发一条是为了不让"重启后补 3 天"变成瞬间 3 条消息。
func (m *Manager) flushPending(ctx context.Context, now time.Time) {
	today := traffic.TodayAt(m.cfg.TZ, now)
	for _, day := range m.pendingDays() {
		if day == today {
			continue // 当天数据尚未冻结，等跨天后再补
		}
		if m.lastDailyDay() == day {
			m.removePending(day)
			continue
		}
		m.sendDaily(ctx, day)
		return
	}
}

// dispatch 向所有订阅了该事件的渠道发送。返回是否至少有一个渠道成功。
//
// "至少一个成功即算成功"是刻意的口径：多渠版本就互为冗余，
// 单渠道失败不重要（下一条消息会带最新数据）；只有日结因为承载终值才需要补偿。
func (m *Manager) dispatch(ctx context.Context, event string, snap *Snapshot) bool {
	anyOK := false
	for _, ch := range m.channels {
		if !ch.HasEvent(event) {
			continue
		}
		if m.breakerOpen(ch.Name()) {
			slog.Debug("渠道处于静默期, 本次跳过", "channel", ch.Name())
			continue
		}
		blocks, dropped := Render(snap, ch.Caps())
		if len(dropped) > 0 {
			// 单行本身超长只能丢弃，但必须上报，绝不静默。
			slog.Warn("部分数据行因标识过长未能纳入备份",
				"channel", ch.Name(), "count", len(dropped))
		}
		msg := Message{
			Event:  event,
			Kind:   snap.Kind,
			Day:    snap.Day,
			HHMM:   snap.HHMM(),
			Title:  messageTitle(event, snap),
			Blocks: blocks,
			Snap:   snap,
		}
		if err := m.sendWithRetry(ctx, ch, msg); err != nil {
			slog.Warn("推送失败", "channel", ch.Name(), "event", event, "err", SafeErr(err))
			m.recordFailure(ch.Name())
			continue
		}
		m.recordSuccess(ch.Name())
		anyOK = true
	}
	return anyOK
}

func messageTitle(event string, snap *Snapshot) string {
	if event == config.EventDaily {
		return "SBX 日报 " + snap.Day
	}
	return "SBX 流量 " + snap.Title()
}

// sendWithRetry 带退避重试的单渠道发送。每次尝试都有独立的超时，
// 避免一次卡死拖垮整个调度循环。
func (m *Manager) sendWithRetry(ctx context.Context, ch Channel, msg Message) error {
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
		err := ch.Send(cctx, msg)
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

// ---- 熔断 ----

func (m *Manager) breakerOpen(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.breaker[name]
	if !ok {
		return false
	}
	if m.now().Unix() >= until {
		delete(m.breaker, name)
		return false
	}
	return true
}

func (m *Manager) recordFailure(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCount[name]++
	if m.failCount[name] >= breakerThreshold {
		m.breaker[name] = m.now().Unix() + int64(breakerCooldown/time.Second)
		m.failCount[name] = 0
		slog.Warn("渠道连续失败, 进入静默期",
			"channel", name, "cooldown_seconds", int(breakerCooldown/time.Second))
	}
}

func (m *Manager) recordSuccess(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, was := m.breaker[name]; was {
		slog.Info("渠道已恢复", "channel", name)
	}
	delete(m.breaker, name)
	m.failCount[name] = 0
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

func (m *Manager) lastDailyDay() string { return m.metaGet(metaKeyDailyDay) }

func (m *Manager) hbSlotDone(slot string) bool { return m.metaGet(metaKeyHBSlot) == slot }

func (m *Manager) pendingDays() []string {
	v := m.metaGet(metaKeyPending)
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, d := range strings.Split(v, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func (m *Manager) addPending(day string) {
	days := m.pendingDays()
	for _, d := range days {
		if d == day {
			return
		}
	}
	days = append(days, day)
	sort.Strings(days)
	if len(days) > maxPendingDays {
		days = days[len(days)-maxPendingDays:]
	}
	if err := m.metaSet(metaKeyPending, strings.Join(days, ",")); err != nil {
		slog.Warn("写入补偿队列失败", "err", err)
	}
}

func (m *Manager) removePending(day string) {
	days := m.pendingDays()
	kept := make([]string, 0, len(days))
	for _, d := range days {
		if d != day {
			kept = append(kept, d)
		}
	}
	if err := m.metaSet(metaKeyPending, strings.Join(kept, ",")); err != nil {
		slog.Warn("更新补偿队列失败", "err", err)
	}
}

// ---- dead man's switch ----
// pingHeartbeat 在配置了 heartbeat_url 时上报一次（可选功能）。
// 这是唯一能在整机损毁后仍能告警的机制——服务器自己炸了没法自己报警。
// ping 失败只记 Debug，绝不影响主流程。
func (m *Manager) pingHeartbeat(ctx context.Context, ok bool) {
	u := strings.TrimSpace(m.ncfg.HeartbeatURL)
	if u == "" {
		return
	}
	if !ok {
		u = strings.TrimSuffix(u, "/") + "/fail"
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		slog.Debug("heartbeat ping 失败", "err", SafeErr(err))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
}

// ---- 自检 ----

// ChannelResult 是单渠道的发送结果。
type ChannelResult struct {
	Name string
	Err  error
}

// SendTest 向所有渠道各发一条测试消息，返回逐渠道结果。
//
// 与 dispatch 的差异（有意为之）：不受熔断影响、不记录 meta、不看 events 订阅——
// 测试的目的就是验证**每一个**渠道当前是否可用。
// 消息正文标注 [测试]，避免与真实推送混淆。
func (m *Manager) SendTest(ctx context.Context, event, day string) []ChannelResult {
	kind := KindHeartbeat
	if event == config.EventDaily {
		kind = KindDaily
	}
	if day == "" {
		day = traffic.TodayAt(m.cfg.TZ, m.now())
	}
	snap, err := BuildSnapshot(m.cfg, m.db.DB, m.live, kind, day, m.now())
	if err != nil {
		return []ChannelResult{{Name: "(构建快照)", Err: err}}
	}
	// 测试消息不反映采集状态：live 可能为 nil，若照原样渲染会被误读成"采集异常"。
	snap.Healthy = true
	snap.LastOK = 0
	snap.LagSec = -1

	out := make([]ChannelResult, 0, len(m.channels))
	for _, ch := range m.channels {
		blocks, dropped := Render(snap, ch.Caps())
		if len(dropped) > 0 {
			slog.Warn("测试消息有数据行未纳入备份", "channel", ch.Name(), "count", len(dropped))
		}
		msg := Message{
			Event:  event,
			Kind:   snap.Kind,
			Day:    snap.Day,
			HHMM:   snap.HHMM(),
			Title:  "[测试] " + messageTitle(event, snap),
			Blocks: blocks,
			Snap:   snap,
		}
		out = append(out, ChannelResult{Name: ch.Name(), Err: m.sendWithRetry(ctx, ch, msg)})
	}
	return out
}
