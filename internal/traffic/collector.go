package traffic

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// sampleKeepSeconds 实时速率窗口（旧 SAMPLE_KEEP_SECONDS）。
const sampleKeepSeconds = int64(120)

// Clock 可注入时钟，测试用固定时间回放。
type Clock func() time.Time

// Collector 单调差分累加采集器。并发模型：单个 goroutine 执行 Tick，
// 状态经 RWMutex 暴露给 HTTP 层快照读取。
type Collector struct {
	cfg     *config.Config
	db      *database.DB
	backend firewall.Backend
	now     Clock

	mu           sync.RWMutex
	lastError    string
	lastOKTs     int64
	lastSampleTs int64
	lastConns    map[string]connection.Conns
	hasConns     bool
	repairAt     int64

	doneOnce sync.Once
	done     chan struct{}
}

// NewCollector 构造采集器（按配置自动选择 nftables / iptables 后端）。
func NewCollector(cfg *config.Config, db *database.DB) *Collector {
	return &Collector{
		cfg:     cfg,
		db:      db,
		backend: firewall.New(cfg.Backend, cfg.NftConf, cfg.IptScript),
		now:     time.Now,
		done:    make(chan struct{}),
	}
}

// SetBackend 替换后端与时钟（仅测试使用）。
func (c *Collector) SetBackend(b firewall.Backend) { c.backend = b }

// SetClock 注入时钟（仅测试使用）。
func (c *Collector) SetClock(fn Clock) { c.now = fn }

// BackendName 返回当前后端名（nft / iptables）。
func (c *Collector) BackendName() string { return c.backend.Name() }

// Done 在 Run 返回后被关闭。
func (c *Collector) Done() <-chan struct{} { return c.done }

// Status 是给 HTTP 层的只读快照。
type Status struct {
	Error    string
	LastOK   int64
	Conns    map[string]connection.Conns
	HasConns bool
}

// Snapshot 返回当前状态快照（无锁竞争开销极小）。
func (c *Collector) Snapshot() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Status{
		Error:    c.lastError,
		LastOK:   c.lastOKTs,
		Conns:    c.lastConns,
		HasConns: c.hasConns,
	}
}

// ---- 元数据 / 基线 -------------------------------------------------------

func (c *Collector) metaGet(ctx context.Context, key string) (string, bool) {
	var val sql.NullString
	if err := c.db.QueryRowContext(ctx, "SELECT v FROM meta WHERE k=?", key).Scan(&val); err != nil {
		return "", false
	}
	if !val.Valid {
		return "", false
	}
	return val.String, true
}

// counterBaseline: [bytes, packets, updatedAtMs]
type counterBaseline [3]int64

func (c *Collector) loadState(ctx context.Context) (map[string]counterBaseline, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT name,last_bytes,last_pkts,updated_at FROM counter_state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]counterBaseline{}
	for rows.Next() {
		var name string
		var b, p, upd int64
		if err := rows.Scan(&name, &b, &p, &upd); err != nil {
			return nil, err
		}
		// 旧版存秒，新版存毫秒；升级时自动兼容。
		if upd > 0 && upd < 1_000_000_000_000 {
			upd *= 1000
		}
		out[name] = counterBaseline{b, p, upd}
	}
	return out, rows.Err()
}

// ---- 提交 ---------------------------------------------------------------

const (
	sqlUpsertDaily = "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?) " +
		"ON CONFLICT(day,scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx, " +
		"rx_pkts=rx_pkts+excluded.rx_pkts, tx_pkts=tx_pkts+excluded.tx_pkts"
	sqlUpsertTotals = "INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?) " +
		"ON CONFLICT(scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx, " +
		"rx_pkts=rx_pkts+excluded.rx_pkts, tx_pkts=tx_pkts+excluded.tx_pkts"
	sqlUpsertSample = "INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(?,?,?,?,?,1) " +
		"ON CONFLICT(ts,scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx, " +
		"duration_ms=MAX(duration_ms,excluded.duration_ms), valid=1"
	sqlUpsertMeta = "INSERT INTO meta(k,v) VALUES('epoch',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v"
)

type deltaSlot struct {
	rx, tx, rxPkts, txPkts int64
	durationMS             int64
	valid                  bool
	seenRX, seenTX         bool
}

func (d *deltaSlot) anyTraffic() bool {
	return d.rx != 0 || d.tx != 0 || d.rxPkts != 0 || d.txPkts != 0
}

// commitTick 把一轮差分结果原子入账：增量、样本、基线、世代、清理同事务。
func (c *Collector) commitTick(ctx context.Context, deltas map[string]*deltaSlot,
	snap firewall.Snapshot, ts, tsMS int64, epoch uint64, hasEpoch bool) error {

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stDaily, err := tx.PrepareContext(ctx, sqlUpsertDaily)
	if err != nil {
		return err
	}
	defer stDaily.Close()
	stTotals, err := tx.PrepareContext(ctx, sqlUpsertTotals)
	if err != nil {
		return err
	}
	defer stTotals.Close()
	stSample, err := tx.PrepareContext(ctx, sqlUpsertSample)
	if err != nil {
		return err
	}
	defer stSample.Close()

	scopes := make([]string, 0, len(deltas))
	for k := range deltas {
		scopes = append(scopes, k)
	}
	sort.Strings(scopes)

	day := TodayAt(c.cfg.TZ, c.now())
	for _, scope := range scopes {
		d := deltas[scope]
		if d.anyTraffic() {
			if _, err := stDaily.ExecContext(ctx, day, scope, d.rx, d.tx, d.rxPkts, d.txPkts); err != nil {
				return err
			}
			if _, err := stTotals.ExecContext(ctx, scope, d.rx, d.tx, d.rxPkts, d.txPkts); err != nil {
				return err
			}
		}
		// valid=1 才进入实时速率窗口；零字节也写样本，让速率回落到零。
		if d.valid && d.durationMS > 0 {
			if _, err := stSample.ExecContext(ctx, ts, scope, d.rx, d.tx, d.durationMS); err != nil {
				return err
			}
		}
	}

	// 基线全量重写（DELETE + INSERT，事务内一致）
	names := make([]string, 0, len(snap))
	for k := range snap {
		names = append(names, k)
	}
	sort.Strings(names)
	if _, err := tx.ExecContext(ctx, "DELETE FROM counter_state"); err != nil {
		return err
	}
	stBase, err := tx.PrepareContext(ctx,
		"INSERT INTO counter_state(name,last_bytes,last_pkts,updated_at) VALUES(?,?,?,?)")
	if err != nil {
		return err
	}
	defer stBase.Close()
	for _, name := range names {
		v := snap[name]
		if _, err := stBase.ExecContext(ctx, name, v[0], v[1], tsMS); err != nil {
			return err
		}
	}

	if hasEpoch {
		if _, err := tx.ExecContext(ctx, sqlUpsertMeta, strconv.FormatUint(epoch, 10)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM samples WHERE ts < ?", ts-sampleKeepSeconds); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- 单轮采集 -----------------------------------------------------------

// Tick 执行一轮采集：读计数器 → 差分 → 原子入账 → 刷新连接数缓存。
func (c *Collector) Tick(ctx context.Context) error {
	tsMS := c.now().UnixMilli()
	wallTs := tsMS / 1000

	snapshot, err := c.backend.Read(ctx)
	if err != nil {
		return err
	}

	c.mu.RLock()
	prevSampleTs := c.lastSampleTs
	c.mu.RUnlock()

	// samples 主键是秒；保证极端调度/时钟回拨时仍单调，不覆盖上一条样本。
	ts := wallTs
	if prevSampleTs != 0 && wallTs < prevSampleTs+1 {
		ts = prevSampleTs + 1
	}

	epoch, hasEpoch := firewall.SnapshotEpoch(snapshot)
	prevEpoch, hasPrev := c.metaGet(ctx, "epoch")
	epochStr := ""
	if hasEpoch {
		epochStr = strconv.FormatUint(epoch, 10)
	}
	freshRuleset := hasEpoch && hasPrev && epochStr != prevEpoch

	state := map[string]counterBaseline{}
	if !freshRuleset {
		if state, err = c.loadState(ctx); err != nil {
			return err
		}
	}

	interval := c.cfg.Interval
	if interval < 1 {
		interval = 1
	}
	expectedMs := int64(interval) * 1000
	minMs := expectedMs / 4
	if minMs < 200 {
		minMs = 200
	}
	maxMs := expectedMs*3 + 500

	deltas := map[string]*deltaSlot{}
	resetHits := 0

	for name, val := range snapshot {
		byts, pkts := val[0], val[1]
		scope, direction, ok := firewall.ParseCounterName(name)
		if !ok {
			continue
		}
		slot := deltas[scope]
		if slot == nil {
			slot = &deltaSlot{valid: true}
			deltas[scope] = slot
		}

		var dBytes, dPkts int64
		var durationMs int64
		valid := false

		prev, had := state[name]
		if !had {
			// 第一次看到这个计数器：字节可累计，但起点未知，不能拿来算速率。
			dBytes, dPkts = byts, pkts
		} else {
			durationMs = tsMS - prev[2]
			if durationMs < 0 {
				durationMs = 0
			}
			if byts < prev[0] {
				// 计数器归零：补进累计，但发生时刻未知，不制造速率尖峰。
				dBytes, dPkts = byts, pkts
				resetHits++
			} else {
				dBytes = byts - prev[0]
				dPkts = pkts - prev[1]
				if dPkts < 0 {
					dPkts = 0
				}
				valid = durationMs >= minMs && durationMs <= maxMs
			}
		}

		if direction == "rx" {
			slot.rx += dBytes
			slot.rxPkts += dPkts
			slot.seenRX = true
		} else {
			slot.tx += dBytes
			slot.txPkts += dPkts
			slot.seenTX = true
		}
		if durationMs > slot.durationMS {
			slot.durationMS = durationMs
		}
		slot.valid = slot.valid && valid
	}

	// 一个节点必须同时拿到入/出两个方向的可信基线，才写速率样本。
	for _, d := range deltas {
		d.valid = d.valid && d.seenRX && d.seenTX && d.durationMS > 0
	}

	if err := c.commitTick(ctx, deltas, snapshot, ts, tsMS, epoch, hasEpoch); err != nil {
		return err
	}

	// 连接数缓存：由采集线程每轮算一次，HTTP 请求不再逐次读 /proc
	connsMap := map[string]connection.Conns{}
	nodeList := nodes.LoadPanelNodes(c.cfg.NodesFile)
	if cs, cerr := connection.CountForNodes(nodeList); cerr == nil {
		connsMap = cs
	} else {
		slog.Debug("连接数统计失败", "err", cerr)
	}

	c.mu.Lock()
	c.lastSampleTs = ts
	c.lastOKTs = wallTs
	c.lastError = ""
	c.lastConns = connsMap
	c.hasConns = true
	c.mu.Unlock()

	if freshRuleset {
		slog.Info("检测到规则集世代变化, 累计流量已衔接，速率从新基线开始",
			"from", prevEpoch, "to", epochStr)
	}
	if resetHits > 0 {
		slog.Warn("检测到计数器归零, 已补入累计；为避免假峰值，本轮不写速率",
			"count", resetHits)
	}
	return nil
}

// ---- 常驻循环 -----------------------------------------------------------

func (c *Collector) setError(msg string) {
	c.mu.Lock()
	c.lastError = msg
	c.mu.Unlock()
}

// Run 常驻采集循环：绝对周期调度，执行耗时不会叠加进下一轮；
// 跨周期挂起后从当前重新对齐，不补跑假样本。
func (c *Collector) Run(ctx context.Context) {
	defer c.doneOnce.Do(func() { close(c.done) })

	interval := time.Duration(c.cfg.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	deadline := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.Tick(ctx)
		if err != nil {
			if firewall.IsLookup(err) {
				c.setError("计数器不存在: " + unwrapMsg(err))
				nowSec := time.Now().Unix()
				c.mu.Lock()
				due := nowSec-c.repairAt > 30
				if due {
					c.repairAt = nowSec
				}
				c.mu.Unlock()
				if due {
					_ = c.backend.Repair(ctx)
				}
			} else {
				c.setError(err.Error())
				slog.Warn("采集异常", "err", err)
			}
		}
		deadline = deadline.Add(interval)
		nowT := time.Now()
		if !deadline.After(nowT) {
			// 若系统挂起/卡顿跨过多个周期，从当前重新对齐
			deadline = nowT.Add(interval)
		}
		timer := time.NewTimer(deadline.Sub(nowT))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func unwrapMsg(err error) string { return err.Error() }
