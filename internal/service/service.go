// Package service 编排 sbx-core 的运行态：serve 守护进程与管理子命令，
// 对齐旧 `python3 panel.py <cmd>` 的全部行为。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/api"
	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// Serve 启动采集器与面板 HTTP 服务，阻塞直至 SIGINT/SIGTERM。
// 退出顺序：停止采集器（等待在途事务提交）→ 排空 HTTP → 关闭数据库。
//
// v3.0.3 fail-closed：panel.json 存在但损坏时拒绝启动（绝不带默认值上线）；
// 非 loopback 监听 + 空 token 直接拒绝启动（原为仅告警后继续，存在
// 无认证公网暴露风险）。
func Serve() int {
	cfg, err := config.LoadStrict()
	if err != nil {
		slog.Error(err.Error())
		return 1
	}

	if cfg.Token == "" && listenIsPublic(cfg.Listen) {
		slog.Error("拒绝启动: 面板监听在非本机地址(" + cfg.Listen + ")且未设置 token, " +
			"公网无认证暴露已被禁止。请先执行 sbx-core config-ensure-token 设置访问令牌, " +
			"或将 listen 改为 127.0.0.1")
		return 1
	}
	if cfg.Token == "" {
		slog.Warn("警告: 面板监听在本机回环地址且未设置 token, 仅本机可访问")
	}

	db, err := database.Open(cfg.DB)
	if err != nil {
		slog.Error("数据库打开失败", "err", err)
		return 1
	}
	defer db.Close()

	collector := traffic.NewCollector(cfg, db)

	// 策略服务（Quota / IP Limit）：与采集器并行运行，复用同一 SQLite。
	//
	// 脚本路径必须与计数规则 cfg.NftConf 分离：旧版把 cfg.NftConf 传进来，
	// 一旦任何节点启用策略，policy 脚本就覆盖 /etc/sbx/nft.conf 的
	// sbx_traffic 计数表定义；更糟的是 firewall.Nft.Repair 自愈时重放的
	// 也是这个文件，导致计数器永远建不回来（统计与配额一起停摆）。
	policySvc := policy.New(db.DB, config.AppDir(), policy.DefaultPolicyConf(config.AppDir()))
	// 策略 enforcement 只支持 nft；把「当前生效后端」告诉策略层，
	// 使 iptables 主机上状态照常发布并明确提示不支持，而不是每秒失败一次。
	policySvc.SetEnforceBackend(func() string { return firewall.EffectiveBackend(cfg.Backend) })

	addr := net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.Port))

	_, hs := api.New(cfg, db, collector, policySvc)

	ln, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		slog.Error("无法监听 " + addr + " — " + lerr.Error())
		slog.Error("端口可能已被占用，请在面板设置中更换端口，或先停止旧进程")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	collCtx, collCancel := context.WithCancel(context.Background())
	defer collCancel()
	go collector.Run(collCtx)

	polCtx, polCancel := context.WithCancel(context.Background())
	defer polCancel()
	go policySvc.Run(polCtx)

	slog.Info("面板已启动 http://" + addr + "  后端=" + collector.BackendName() +
		"  采集间隔=" + fmt.Sprint(cfg.Interval) + "s")

	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP 服务异常退出", "err", err)
			return 1
		}
	}

	// 收尾顺序（不可暴力中断）：先停采集并等它在途事务落库，
	// 再排空 HTTP，最后关闭数据库。
	polCancel()
	collCancel()
	select {
	case <-collector.Done():
	case <-time.After(15 * time.Second):
		slog.Warn("等待采集器退出超时，继续收尾")
	}
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hsCancel()
	if err := hs.Shutdown(hsCtx); err != nil {
		slog.Warn("HTTP 关闭超时", "err", err)
	}
	return 0
}

// listenIsPublic 判断监听地址是否面向非本机（fail-closed 口径）：
//   - 127.x.x.x / ::1 / localhost → 非公网（false）；
//   - 0.0.0.0 / :: / 公网 IP / 空串 / 其它不可解析主机名 → 按公网处理（true）。
//
// 空 token 时只有确认回环的监听才被允许；无法证明是回环的一律拒绝，
// 避免“看起来像主机名实际暴露公网”的边角案例。
func listenIsPublic(listen string) bool {
	l := strings.ToLower(strings.TrimSpace(listen))
	switch l {
	case "":
		return true // 未配置等价于全接口监听
	case "localhost", "::1":
		return false
	}
	if ip := net.ParseIP(l); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}
