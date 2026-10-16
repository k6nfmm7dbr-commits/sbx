package traffic

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/clash"
)

const (
	// maxDomainsPerScope 是单个节点每天保留的域名条目上限。
	// 超出后新域名统一归入 otherDomain——异常流量（端口扫描、随机子域）能让
	// 域名数无限膨胀，必须封顶。
	maxDomainsPerScope = 5000
	// otherDomain 是超出上限后的归并处。
	otherDomain = "(其它域名)"
	// unknownDomain 表示客户端没有把域名交给服务端（只有 IP 可看）。
	unknownDomain = "(未知域名)"
	// maxDomainLen 限制单个域名的存储长度，防御异常超长 host。
	maxDomainLen = 253
)

// DomainRow 是某个节点某天的单个域名流量。
type DomainRow struct {
	Domain string `json:"domain"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
	Total  int64  `json:"total"`
	Hits   int64  `json:"hits"`
}

// ---- 差分累加 ----

type domainKey struct{ scope, domain string }

type domainDelta struct {
	scope  string
	domain string
	rx, tx int64
	hits   int64
}

type connCursor struct{ up, down int64 }

// domainTracker 把「活动连接的字节增量」归集到 (节点, 域名)。
//
// 为什么必须自己记状态：Clash API 的 /connections 只返回**当前存活**的连接，
// 且每条连接的 upload/download 是它自己的累计值。连接关闭即从列表消失，
// 所以只能高频轮询、对每个连接 ID 做差分累加，关闭的连接连最后一段增量
// 一起丢掉（这是该数据源的固有精度损失，接口层会给出「未归类」让偏差可见）。
type domainTracker struct {
	last  map[string]connCursor
	known map[string]map[string]struct{} // scope → 当天已出现过的域名
	day   string
}

func newDomainTracker() *domainTracker {
	return &domainTracker{
		last:  map[string]connCursor{},
		known: map[string]map[string]struct{}{},
	}
}

// snapshot 在 observe 之前留存游标基线。
//
// 为什么需要：observe 会就地推进 t.last，而增量要等 commitTick 事务提交才落库。
// 若提交失败（磁盘满、DB busy），计数器基线在 DB 里随事务一起回滚、下一轮
// 差分会重算，没有损失；但 t.last 在内存里，不回滚的话这一轮已差分出的域名
// 增量就永久丢失了——且是无声的少计。提交失败时调用 rollback 恢复基线即可。
func (t *domainTracker) snapshot() func() {
	saved := make(map[string]connCursor, len(t.last))
	for id, cur := range t.last {
		saved[id] = cur
	}
	savedDay := t.day
	return func() {
		t.last = saved
		if t.day != savedDay {
			// 跨天被回滚：恢复 day 让下一轮差分对到正确的日期。
			// known（当天已知域名集合）不回滚——它只影响 5000 上限计数，
			// 深拷贝成本不值当；跨天与提交失败同 tick 撞上的概率极低，
			// 最坏后果也仅是上限重新放行一批已记录过的域名。
			t.day = savedDay
		}
	}
}

// observe 用一次连接表快照算出本轮增量。返回的切片元素即待入账的数据。
func (t *domainTracker) observe(conns []clash.Connection, day string) []domainDelta {
	if t.day != day { // 跨天：域名计数重置
		t.day = day
		t.known = map[string]map[string]struct{}{}
	}
	agg := map[domainKey]*domainDelta{}
	seen := make(map[string]struct{}, len(conns))

	for _, c := range conns {
		seen[c.ID] = struct{}{}
		prev, existed := t.last[c.ID]
		du, dd := c.Upload-prev.up, c.Download-prev.down
		t.last[c.ID] = connCursor{up: c.Upload, down: c.Download}
		// 计数器不应回退；真回退（连接被复用/异常）时按 0 处理而不是负数入账。
		if du < 0 {
			du = 0
		}
		if dd < 0 {
			dd = 0
		}
		if du == 0 && dd == 0 {
			continue
		}
		id, ok := nodeIDFromTag(c.Metadata.InboundTag())
		if !ok {
			continue // 不是 sbx 管理的节点（用户自建 inbound）
		}
		scope := "node:" + id
		dom := t.classify(scope, normalizeHost(c.Metadata))
		k := domainKey{scope: scope, domain: dom}
		d := agg[k]
		if d == nil {
			d = &domainDelta{scope: scope, domain: dom}
			agg[k] = d
		}
		// 服务器视角：从客户端收到的字节 = clash 的 upload；发给客户端 = download。
		d.rx += du
		d.tx += dd
		if !existed {
			d.hits++ // 连接数只在首次见到该连接时计一次
		}
	}

	for id := range t.last { // 清掉已关闭的连接，否则内存只增不减
		if _, ok := seen[id]; !ok {
			delete(t.last, id)
		}
	}

	out := make([]domainDelta, 0, len(agg))
	for _, d := range agg {
		out = append(out, *d)
	}
	// 固定顺序，便于测试与日志稳定。
	sort.Slice(out, func(i, j int) bool {
		if out[i].scope != out[j].scope {
			return out[i].scope < out[j].scope
		}
		return out[i].domain < out[j].domain
	})
	return out
}

// classify 做域名上限保护。
func (t *domainTracker) classify(scope, dom string) string {
	set := t.known[scope]
	if set == nil {
		set = map[string]struct{}{}
		t.known[scope] = set
	}
	if _, ok := set[dom]; ok {
		return dom
	}
	if len(set) >= maxDomainsPerScope {
		return otherDomain
	}
	set[dom] = struct{}{}
	return dom
}

// nodeIDFromTag 从入站标签里取出节点 id（"sbx-n12" → "12"）。
// 前缀与 nodes.TagPrefix 一致；这里不复用常量是为了避免 traffic 反向依赖 nodes。
func nodeIDFromTag(tag string) (string, bool) {
	const prefix = "sbx-n"
	if !strings.HasPrefix(tag, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(tag, prefix)
	if id == "" {
		return "", false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return id, true
}

// normalizeHost 规范化域名。客户端没把域名交给服务端时退化为目标 IP。
func normalizeHost(m clash.Metadata) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(m.Host), "."))
	if h == "" {
		if ip := strings.TrimSpace(m.DestinationIP); ip != "" {
			return ip
		}
		return unknownDomain
	}
	if len(h) > maxDomainLen {
		// 按字节截断可能切断多字节 UTF-8 序列；丢弃尾部不完整的残片，
		// 避免无效 UTF-8 进入 SQLite（TEXT 列要求合法 UTF-8）。
		h = strings.ToValidUTF8(h[:maxDomainLen], "")
	}
	return h
}

// ---- 落库与查询 ----

// applyDomainDeltas 在当前事务里累加域名增量。与计数入账同事务，保证一致。
func applyDomainDeltas(ctx context.Context, tx *sql.Tx, day string, deltas []domainDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO domain_daily(day, scope, domain, rx, tx, hits) VALUES(?,?,?,?,?,?)
ON CONFLICT(day, scope, domain) DO UPDATE SET
    rx   = rx   + excluded.rx,
    tx   = tx   + excluded.tx,
    hits = hits + excluded.hits`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, d := range deltas {
		if _, err := stmt.ExecContext(ctx, day, d.scope, d.domain, d.rx, d.tx, d.hits); err != nil {
			return err
		}
	}
	return nil
}

// purgeDomainsBefore 删除早于给定日期的域名记录，控制表体积。
func purgeDomainsBefore(ctx context.Context, tx *sql.Tx, beforeDay string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM domain_daily WHERE day < ?`, beforeDay)
	return err
}

// QTopDomains 返回某节点在 [fromDay, toDay] 区间内按总流量倒序的域名列表。
// fromDay == toDay 即「某一天」，两个都传今天即「今日」。
func QTopDomains(db *sql.DB, fromDay, toDay, scope string, limit int) ([]DomainRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := db.Query(`
SELECT domain, SUM(rx), SUM(tx), SUM(rx+tx) AS total, SUM(hits)
FROM domain_daily WHERE day >= ? AND day <= ? AND scope=?
GROUP BY domain
ORDER BY total DESC, domain ASC LIMIT ?`, fromDay, toDay, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DomainRow, 0, limit)
	for rows.Next() {
		var r DomainRow
		if err := rows.Scan(&r.Domain, &r.Rx, &r.Tx, &r.Total, &r.Hits); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QDomainClassified 返回某节点在 [fromDay, toDay] 区间内已归类到域名的总流量。
//
// 与 daily 表里该节点的总量相减，就得到「未被采样到」的部分——短连接会在
// 两次轮询之间结束，那部分流量无法归属到任何域名。把这个差值展示出来，
// 用户才能判断排序结果可不可信，而不是误以为它就是全部。
func QDomainClassified(db *sql.DB, fromDay, toDay, scope string) (int64, error) {
	var total sql.NullInt64
	err := db.QueryRow(
		`SELECT SUM(rx+tx) FROM domain_daily WHERE day >= ? AND day <= ? AND scope=?`,
		fromDay, toDay, scope).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// QScopeTrafficRange 返回某节点在 [fromDay, toDay] 区间内的**权威**总流量。
//
// 口径是 nftables 计数器（与面板其它地方一致），用来和上面「已归类到域名」
// 的量相减，得出没被连接表采样到的部分——把它显式展示出来，用户才能判断
// 域名排序可不可信，而不是误以为那就是全部流量。
func QScopeTrafficRange(db *sql.DB, fromDay, toDay, scope string) (int64, error) {
	var total sql.NullInt64
	err := db.QueryRow(
		`SELECT SUM(rx+tx) FROM daily WHERE day >= ? AND day <= ? AND scope=?`,
		fromDay, toDay, scope).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}
