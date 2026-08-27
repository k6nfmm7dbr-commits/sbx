// Package database 封装 SQLite 访问：单个共享连接池、每连接 PRAGMA、
// 与旧 Python 完全一致的建表/迁移逻辑。
package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// Schema 与旧 panel.py 的 SCHEMA 完全一致（逐条执行，等价 executescript）。
const Schema = `
CREATE TABLE IF NOT EXISTS meta (
    k TEXT PRIMARY KEY,
    v TEXT
);
CREATE TABLE IF NOT EXISTS counter_state (
    name       TEXT PRIMARY KEY,
    last_bytes INTEGER NOT NULL DEFAULT 0,
    last_pkts  INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS daily (
    day     TEXT NOT NULL,
    scope   TEXT NOT NULL,
    rx      INTEGER NOT NULL DEFAULT 0,
    tx      INTEGER NOT NULL DEFAULT 0,
    rx_pkts INTEGER NOT NULL DEFAULT 0,
    tx_pkts INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, scope)
);
CREATE TABLE IF NOT EXISTS totals (
    scope   TEXT PRIMARY KEY,
    rx      INTEGER NOT NULL DEFAULT 0,
    tx      INTEGER NOT NULL DEFAULT 0,
    rx_pkts INTEGER NOT NULL DEFAULT 0,
    tx_pkts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS samples (
    ts          INTEGER NOT NULL,
    scope       TEXT NOT NULL,
    rx          INTEGER NOT NULL DEFAULT 0,
    tx          INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    valid       INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (ts, scope)
);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);
CREATE TABLE IF NOT EXISTS node_policy (
    node_id                TEXT PRIMARY KEY,
    quota_enabled          INTEGER NOT NULL DEFAULT 0,
    quota_limit_bytes      INTEGER NOT NULL DEFAULT 0,
    quota_reset_baseline   INTEGER NOT NULL DEFAULT 0,
    ip_limit_enabled       INTEGER NOT NULL DEFAULT 0,
    ip_limit_max           INTEGER NOT NULL DEFAULT 0,
    reset_enabled          INTEGER NOT NULL DEFAULT 0,
    reset_period           TEXT    NOT NULL DEFAULT '',
    reset_time             TEXT    NOT NULL DEFAULT '',
    reset_next_at          INTEGER NOT NULL DEFAULT 0
);
`

// DB 包装 *sql.DB。SQLite 是单写者数据库，这里把连接数限制为 1：
// 采集写入与 API 查询天然串行（表都很小），彻底规避 SQLITE_BUSY，
// busy_timeout 仅在与其他进程（如迁移期共存的旧面板）竞争时兜底。
type DB struct {
	*sql.DB
	path string
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	db := sql.OpenDB(&pragmaConnector{dsn: "file:" + path})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // 进程生命周期内复用同一连接
	db.SetConnMaxIdleTime(0)

	d := &DB{DB: db, path: path}
	// WAL 让读写并发互不阻塞；个别文件系统不支持时静默退回（对齐旧行为）。
	if _, err := d.Exec("PRAGMA journal_mode=WAL"); err != nil {
		slog.Debug("WAL 不可用, 使用默认日志模式", "err", err)
	}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

func (d *DB) migrate() error {
	// 迁移全部放入单个事务：CREATE / ALTER / UPDATE 任一失败即整体回滚，
	// 绝不留半迁移 schema。MaxOpenConns=1 下事务独占连接，因此 PRAGMA
	// 查询也必须走 tx（不能走 d.Query，否则会与事务争用同一连接而死锁）。
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range splitStatements(Schema) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	// 无损迁移旧库：旧 samples 没有 duration_ms / valid 列。
	cols, err := sampleColumns(tx)
	if err != nil {
		return err
	}
	if _, ok := cols["duration_ms"]; !ok {
		if _, err := tx.Exec("ALTER TABLE samples ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if _, ok := cols["valid"]; !ok {
		if _, err := tx.Exec("ALTER TABLE samples ADD COLUMN valid INTEGER NOT NULL DEFAULT 1"); err != nil {
			return err
		}
	}
	// 升级前的旧样本没有真实耗时，不能用于实时速率计算；daily/totals 不受影响。
	if _, err := tx.Exec("UPDATE samples SET valid=0 WHERE duration_ms<=0"); err != nil {
		return err
	}
	// node_policy 定时重置列（v3.0.6+）：旧库补齐四列。
	npCols, err := tableColumns(tx, "node_policy")
	if err != nil {
		return err
	}
	if _, ok := npCols["reset_enabled"]; !ok {
		if _, err := tx.Exec("ALTER TABLE node_policy ADD COLUMN reset_enabled INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if _, ok := npCols["reset_period"]; !ok {
		if _, err := tx.Exec("ALTER TABLE node_policy ADD COLUMN reset_period TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, ok := npCols["reset_time"]; !ok {
		if _, err := tx.Exec("ALTER TABLE node_policy ADD COLUMN reset_time TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, ok := npCols["reset_next_at"]; !ok {
		if _, err := tx.Exec("ALTER TABLE node_policy ADD COLUMN reset_next_at INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// querier 抽象 *sql.DB 与 *sql.Tx 共有的查询能力，供 sampleColumns 复用。
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func sampleColumns(q querier) (map[string]bool, error) {
	return tableColumns(q, "samples")
}

// tableColumns 返回指定表的列名集合（迁移用：判断是否已有某列）。
func tableColumns(q querier, table string) (map[string]bool, error) {
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// pragmaConnector 包装 modernc 驱动：每个新建连接自动执行 busy_timeout，
// 保证连接池重建后设置不丢。
type pragmaConnector struct {
	dsn string
}

func (c *pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	if ec, ok := conn.(driver.ExecerContext); ok {
		if _, err := ec.ExecContext(ctx, "PRAGMA busy_timeout=30000", nil); err != nil {
			slog.Debug("busy_timeout 设置失败", "err", err)
		}
	}
	return conn, nil
}

func (c *pragmaConnector) Driver() driver.Driver { return &sqlite.Driver{} }

// splitStatements 把 SQL 脚本按分号切成独立语句。
func splitStatements(script string) []string {
	var out []string
	for _, part := range strings.Split(script, ";") {
		stmt := strings.TrimSpace(part)
		if stmt == "" || isCommentOnly(stmt) {
			continue
		}
		out = append(out, stmt)
	}
	return out
}

func isCommentOnly(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "--") {
			return false
		}
	}
	return true
}
