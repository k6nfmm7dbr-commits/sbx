package traffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/clash"
)

// domainRetentionDays 是域名明细的保留天数。
// 表按 (天, 节点, 域名) 累积，不清理会随访问面扩大而无上限膨胀。
const domainRetentionDays = 30

// clashClient 返回连接表客户端；sing-box 配置未变时复用同一个实例。
//
// 单一事实来源就是 sing-box 配置本身——不维护第二份地址副本，也就不会出现
// 两者不同步。每 tick 都解析一遍配置 JSON 太浪费（几十个节点时配置会到几百 KB），
// 所以按 mtime+size 判断是否需要重读。
func (c *Collector) clashClient() *clash.Client {
	if c.sbConf == "" {
		return nil
	}
	st, err := os.Stat(c.sbConf)
	if err != nil {
		return nil
	}
	if c.clashCli != nil && st.ModTime().Equal(c.clashMod) && st.Size() == c.clashSize {
		return c.clashCli
	}
	data, err := os.ReadFile(c.sbConf)
	if err != nil {
		return nil
	}
	var parsed struct {
		Experimental struct {
			ClashAPI struct {
				ExternalController string `json:"external_controller"`
				Secret             string `json:"secret"`
			} `json:"clash_api"`
		} `json:"experimental"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	c.clashMod, c.clashSize = st.ModTime(), st.Size()
	addr := parsed.Experimental.ClashAPI.ExternalController
	if addr == "" {
		// 配置里没有管理接口（例如尚未同步过节点配置）——本功能不可用，
		// 但必须在下次配置变化时重新判断，所以把缓存置空并记住 mtime。
		c.clashCli = nil
		return nil
	}
	c.clashCli = clash.New(addr, parsed.Experimental.ClashAPI.Secret)
	return c.clashCli
}

// observeDomains 拉一次连接表并结算本轮的域名流量增量。
//
// 任何失败都只记日志并返回 nil：域名统计是附加能力，绝不能影响主采集循环——
// 那条链路负责计费口径的流量统计。
func (c *Collector) observeDomains(ctx context.Context, day string) []domainDelta {
	cli := c.clashClient()
	if cli == nil {
		return nil
	}
	conns, err := cli.Connections(ctx)
	if err != nil {
		slog.Debug("读取 sing-box 连接表失败，本轮跳过域名统计", "err", err)
		return nil
	}
	return c.domTracker.observe(conns, day)
}

// purgeDomainsIfDue 清理过期的域名明细。每小时最多执行一次——
// 虽然走的是主键前缀，也没必要每个 tick 都删一遍。
func (c *Collector) purgeDomainsIfDue(ctx context.Context, tx *sql.Tx, day string) error {
	now := c.now()
	if !c.domPurgeAt.IsZero() && now.Sub(c.domPurgeAt) < time.Hour {
		return nil
	}
	c.domPurgeAt = now
	return purgeDomainsBefore(ctx, tx, dayBefore(day, domainRetentionDays))
}

// dayBefore 返回 day 往前 n 天的日期；day 形如 2006-01-02。
func dayBefore(day string, n int) string {
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, -n).Format("2006-01-02")
}
