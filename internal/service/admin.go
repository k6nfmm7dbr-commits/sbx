package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

func openDB(cfg *config.Config) (*database.DB, func(), error) {
	db, err := database.Open(cfg.DB)
	if err != nil {
		return nil, nil, err
	}
	return db, func() { db.Close() }, nil
}

// Once 执行单轮采集（旧 cmd_once）。
// 会写 SQLite（daily/totals/samples/counter_state），属 mutation：
// panel.json 存在但损坏时必须 fail-closed，绝不基于 defaults 误写数据库。
func Once() error {
	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()
	c := traffic.NewCollector(cfg, db)
	if err := c.Tick(context.Background()); err != nil {
		return err
	}
	fmt.Println("采集完成")
	return nil
}

// Show 输出今日/累计表格（旧 cmd_show）。
func Show() error {
	cfg := config.Load()
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()
	s, err := traffic.BuildSummary(cfg, db.DB, nil)
	if err != nil {
		return err
	}
	fmt.Printf("后端: %s   日期: %s   时区: %s\n", s.Backend, s.Day, s.TZ)
	fmt.Println("-" + repeatStr("-", 67))
	fmt.Printf("%-18s %12s %12s %12s %12s\n", "节点", "今日↑", "今日↓", "累计↑", "累计↓")
	for _, n := range s.Nodes {
		fmt.Printf("%-18s %12s %12s %12s %12s\n",
			truncDisplay(n.Name), traffic.Human(n.Today.Rx), traffic.Human(n.Today.Tx),
			traffic.Human(n.Total.Rx), traffic.Human(n.Total.Tx))
	}
	fmt.Println("-" + repeatStr("-", 67))
	fmt.Printf("%-18s %12s %12s %12s %12s\n",
		"合计", traffic.Human(s.Today.Rx), traffic.Human(s.Today.Tx),
		traffic.Human(s.Total.Rx), traffic.Human(s.Total.Tx))
	return nil
}

func repeatStr(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// truncDisplay 对齐 Python name[:16]（按字符截断）。
func truncDisplay(name string) string {
	r := []rune(name)
	if len(r) > 16 {
		return string(r[:16])
	}
	return name
}

// Daily 输出最近 N 天每日流量表（旧 cmd_daily）。
func Daily(days int) error {
	cfg := config.Load()
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()
	rows, err := traffic.QDaily(db.DB, days, "")
	if err != nil {
		return err
	}
	fmt.Printf("%-12s %14s %14s %14s\n", "日期", "上传", "下载", "合计")
	for _, row := range rows {
		fmt.Printf("%-12s %14s %14s %14s\n",
			row.Day, traffic.Human(row.Rx), traffic.Human(row.Tx), traffic.Human(row.Rx+row.Tx))
	}
	return nil
}

// Reset 清空统计数据（可选 scope，旧 cmd_reset）。
// destructive 操作：panel.json 存在但损坏时必须 fail-closed——
// 绝不允许在 defaults 猜测下打开错误路径的数据库执行删除。
func Reset(scope string) error {
	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()
	if scope != "" {
		for _, q := range []string{
			"DELETE FROM daily WHERE scope=?", "DELETE FROM totals WHERE scope=?",
			"DELETE FROM samples WHERE scope=?"} {
			if _, err := db.Exec(q, scope); err != nil {
				return err
			}
		}
	} else {
		for _, t := range []string{"daily", "totals", "samples"} {
			if _, err := db.Exec("DELETE FROM " + t); err != nil {
				return err
			}
		}
	}
	suffix := ""
	if scope != "" {
		suffix = fmt.Sprintf(" (%s)", scope)
	}
	fmt.Println("统计数据已清空" + suffix)
	return nil
}

// Rules 生成计数规则文件（旧 cmd_rules），返回世代号。
// 配置与 nodes.json 均为严格读取：任一文件存在但损坏时拒绝生成，
// 防止以空节点列表重建规则、清空 per-node counter 基线。
// epoch 使用 UnixNano（v3.0.3 起弃用秒级）：同一秒内连续两次 Apply
// 不再产生相同世代号，Collector 能可靠识别规则集换代。
func Rules() (uint64, error) {
	cfg, err := config.LoadStrict()
	if err != nil {
		return 0, err
	}
	list, err := nodes.LoadPanelNodesStrict(cfg.NodesFile)
	if err != nil {
		return 0, err
	}
	epoch := uint64(time.Now().UnixNano())
	if err := os.MkdirAll(config.AppDir(), 0o755); err != nil {
		return 0, err
	}
	if err := fsx.WriteFileAtomic(cfg.NftConf,
		[]byte(firewall.GenNFT(list, epoch)), 0o644); err != nil {
		return 0, err
	}
	if err := fsx.WriteFileAtomic(cfg.IptScript,
		[]byte(firewall.GenIPTables(list, epoch)), 0o755); err != nil {
		return 0, err
	}
	fmt.Printf("已生成计数规则: %s / %s (%d 个节点, 世代 %d)\n",
		cfg.NftConf, cfg.IptScript, len(list), epoch)
	return epoch, nil
}

// Apply 先把旧规则的最后一轮计数强制落库，再重建并应用规则（旧 cmd_apply）。
//
// v3.0.3 安全闸门：
//  1. 配置 / nodes.json 损坏 → 立即中止，不碰防火墙与 counter_state；
//  2. 最终采样失败（SQLite 写入失败、provider 失败、部分快照等）→ 中止，
//     内核计数器原样保留，下一轮还有机会重新采集；仅明确的 LookupError
//     （旧规则不存在/首次安装）放行。
func Apply() int {
	cfg, err := config.LoadStrict()
	if err != nil {
		slog.Error(err.Error())
		return 1
	}
	// 损坏的 nodes.json 绝不能被宽松读取当成空列表：那会以 0 节点重建
	// 防火墙规则并清空全部 per-node 计数基线。先严格校验再动手。
	if _, err := nodes.LoadPanelNodesStrict(cfg.NodesFile); err != nil {
		slog.Error("nodes.json 校验失败, 已中止 apply(规则/计数器/epoch 均未改动)", "err", err)
		return 1
	}

	if err := finalSampleForRebuild(context.Background(), cfg); err != nil {
		slog.Error("重建前最终采样失败, 已中止 apply(内核计数器保留, 排除故障后重试)", "err", err)
		return 1
	}

	if _, err := Rules(); err != nil {
		slog.Error("规则生成失败", "err", err)
		return 1
	}
	backend := firewall.DetectBackend(cfg.Backend)
	if backend == "nft" {
		rc, _, errMsg := firewall.RunCmd(context.Background(), "nft", "-f", cfg.NftConf)
		if rc != 0 {
			slog.Warn("nft 应用失败, 尝试 iptables:", "err", errMsg)
			backend = "iptables"
		} else {
			fmt.Println("nftables 计数规则已生效")
		}
	}
	if backend == "iptables" {
		rc, _, errMsg := firewall.RunCmd(context.Background(), "sh", cfg.IptScript, "apply")
		if rc != 0 {
			slog.Warn("iptables 应用失败:", "err", errMsg)
			return 1
		}
		fmt.Println("iptables 计数规则已生效")
	}
	return 0
}

// finalSampleForRebuild 在重建规则前做最后一次采样落库。
// 明确的 LookupError（计数器/规则不存在，即首次安装或规则被外部清除）允许继续；
// 其余任何错误（数据库打开/写入失败、provider 执行失败、部分快照等）必须上抛，
// 由调用方中止重建——宁可推迟一轮重建，也不能丢掉旧 counter 的最后一段流量。
func finalSampleForRebuild(ctx context.Context, cfg *config.Config) error {
	db, closer, err := openDB(cfg)
	if err != nil {
		return fmt.Errorf("数据库打开失败: %w", err)
	}
	defer closer()
	c := traffic.NewCollector(cfg, db)
	tickErr := c.Tick(ctx)
	switch {
	case tickErr == nil:
		return nil
	case firewall.IsLookup(tickErr):
		slog.Info("计数规则不存在(首次安装?), 跳过重建前采样", "err", tickErr)
		return nil
	default:
		return fmt.Errorf("最终采样落库失败: %w", tickErr)
	}
}

// Clear 移除内核计数规则，保留历史统计（旧 cmd_clear）。
//
// v3.0.x 数据保护：与 Apply 同一原则——最终采样失败时禁止清除计数器。
// 若 Tick 失败仍删除 nftables/iptables 计数器，最后一次采样到删除之间
// 的流量将永久丢失。仅明确的 LookupError（规则本就不存在/首次运行）放行。
// systemd ExecStop 走同一路径，同样受保护。
func Clear() int {
	cfg, err := config.LoadStrict()
	if err != nil {
		slog.Error(err.Error())
		return 1
	}
	// 先落库最后一段流量；失败则保留现场，下一轮还有机会重新采集。
	db, closer, err := openDB(cfg)
	if err != nil {
		slog.Error("最终采样失败, 已取消清除(内核计数器保留)", "err", err)
		return 1
	}
	c := traffic.NewCollector(cfg, db)
	tickErr := c.Tick(context.Background())
	closer()
	switch {
	case tickErr == nil:
	case firewall.IsLookup(tickErr):
		slog.Info("计数规则不存在(首次运行?), 直接清除", "err", tickErr)
	default:
		slog.Error("最终采样失败, 已取消清除(内核计数器保留, 排除故障后重试)", "err", tickErr)
		return 1
	}
	if firewall.Which("nft") {
		firewall.RunCmd(context.Background(), "nft", "delete", "table", "inet", firewall.NFTTable)
	}
	if _, err := os.Stat(cfg.IptScript); err == nil {
		firewall.RunCmd(context.Background(), "sh", cfg.IptScript, "clear")
	}
	fmt.Println("计数规则已移除（历史统计数据保留）")
	return 0
}

// SelfTest 验证计数器存在且在增长（旧 cmd_selftest）。
func SelfTest() int {
	cfg := config.Load()
	b := firewall.New(cfg.Backend, cfg.NftConf, cfg.IptScript)
	fmt.Println("后端:", b.Name())

	ctx := context.Background()
	first, err := b.Read(ctx)
	if err != nil {
		fmt.Printf("失败: 计数器不存在 (%s)，请先执行 sbx apply\n", err.Error())
		return 1
	}
	var names []string
	var total0 int64
	for k, v := range first {
		total0 += v[0]
		if _, _, ok := firewall.ParseCounterName(k); ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	fmt.Printf("识别到 %d 个计数器: %s\n", len(names), joinOrNone(names))
	if len(names) == 0 {
		return 1
	}
	time.Sleep(3 * time.Second)
	second, err := b.Read(ctx)
	if err != nil {
		fmt.Printf("失败: 第二次读取异常 (%s)\n", err.Error())
		return 1
	}
	var total1 int64
	for _, v := range second {
		total1 += v[0]
	}
	fmt.Printf("3 秒内计数变化: %s\n", traffic.Human(total1-total0))
	if total1 > total0 {
		fmt.Println("自检通过: 计数器可读且有流量")
	} else {
		fmt.Println("自检通过: 计数器可读（当前无流量，属正常）")
	}
	return 0
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "无"
	}
	out := s[0]
	for _, v := range s[1:] {
		out += ", " + v
	}
	return out
}

var _ = config.AppDir // 引用避免误删 import
