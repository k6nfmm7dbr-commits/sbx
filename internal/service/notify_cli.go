package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/notify"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// NotifyConfigCmd 处理 notify-config 子命令。
//
// 写入一律走 config.SetNotify（LoadStrict → 完整校验 → 原子写 0600），
// 非法配置绝不落盘——否则会把一个能正常启动的实例改成启动失败。
//
// 刻意提供 enable/set-time/disable 这类"按值写"的子命令，而不是让安装器在
// shell 里拼 JSON：shell 里做 JSON 拼装极易写出坏结构，而且报错极其难查。
func NotifyConfigCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: sbx-core notify-config <show|status|enable|set-time|set-enabled|set> [...]")
	}
	switch args[0] {
	case "show":
		return notifyShow()
	case "status":
		return notifyStatus()
	case "enable":
		// 参数按位置传：webhook、secret、daily_at。secret 传空串表示不使用签名。
		if len(args) < 4 {
			return fmt.Errorf("用法: sbx-core notify-config enable <webhook> <secret> <daily_at>\n" +
				"  例: sbx-core notify-config enable 'https://open.feishu.cn/open-apis/bot/v2/hook/xxx' '' 00:05")
		}
		nc := &config.NotifyConfig{
			Enabled: true,
			Type:    config.ChannelFeishu,
			Webhook: args[1],
			Secret:  args[2],
			DailyAt: args[3],
		}
		return writeNotify(nc)
	case "set-time":
		if len(args) < 2 {
			return fmt.Errorf("用法: sbx-core notify-config set-time <HH:MM>")
		}
		raw, err := loadNotifyRaw()
		if err != nil {
			return err
		}
		raw["daily_at"] = args[1]
		return saveNotifyRaw(raw)
	case "set-enabled":
		// 翻启停状态而不动其余字段：停用后再启用不需要重新粘贴 webhook。
		// 若配置本身不完整（例如只有 enabled 键），Validate 会拒绝，给出可读原因。
		if len(args) < 2 {
			return fmt.Errorf("用法: sbx-core notify-config set-enabled <true|false>")
		}
		v, err := strconv.ParseBool(args[1])
		if err != nil {
			return fmt.Errorf("取值只能是 true 或 false: %q", args[1])
		}
		raw, err := loadNotifyRaw()
		if err != nil {
			return err
		}
		raw["enabled"] = v
		raw["type"] = config.ChannelFeishu
		return saveNotifyRaw(raw)
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("用法: sbx-core notify-config set <配置文件.json>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", args[1], err)
		}
		if err := config.SetNotify(data); err != nil {
			return err
		}
		printRestartHint()
		return nil
	}
	return fmt.Errorf("未知子命令 %q（可选 show/status/enable/set-time/set-enabled/set）", args[0])
}

// writeNotify 把一份完整配置写入 panel.json。
func writeNotify(nc *config.NotifyConfig) error {
	out, err := fsx.MarshalIndent(nc)
	if err != nil {
		return err
	}
	if err := config.SetNotify(out); err != nil {
		return err
	}
	printRestartHint()
	return nil
}

// notifyShow 打印凭据已脱敏的当前配置。
//
// 没有 --unsafe：签名密钥与 hook token 都不该出现在终端历史或 CI 日志里，
// 而排查问题只需要看到"哪个 host、路径结构对不对、有没有配密钥"。
func notifyShow() error {
	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	if cfg.Notify == nil {
		fmt.Println("（未配置推送）")
		return nil
	}
	cp := *cfg.Notify
	cp.Webhook = notify.RedactURL(cp.Webhook)
	cp.Secret = notify.RedactSecret(cp.Secret)
	out, err := fsx.MarshalIndent(&cp)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	fmt.Println("（凭据已脱敏）")
	return nil
}

// notifyStatus 打印摘要（凭据不出现），供安装器菜单展示。
func notifyStatus() error {
	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	nc := cfg.Notify
	if nc == nil {
		fmt.Println("状态: 未配置")
		return nil
	}
	if !nc.Enabled {
		fmt.Println("状态: 未启用")
		return nil
	}
	fmt.Println("状态: 已启用")
	fmt.Printf("渠道: 飞书群机器人（%s）\n", notify.RedactURL(nc.Webhook))
	if nc.Secret == "" {
		fmt.Println("签名: 未启用（建议在飞书机器人设置里开启签名校验后再填入密钥）")
	} else {
		fmt.Println("签名: 已启用")
	}
	fmt.Printf("日报时刻: 每日 %s（时区 %s）\n", nc.DailyAt, cfg.TZ)
	if days, bytes, err := historyStats(cfg); err == nil && days > 0 {
		fmt.Printf("历史: %d 天，累计 %s\n", days, traffic.Human(bytes))
	}
	return nil
}

// historyStats 尽力给出历史规模，失败时静默跳过（摘要不该因为读库失败而报错）。
func historyStats(cfg *config.Config) (int, int64, error) {
	db, closer, err := openDB(cfg)
	if err != nil {
		return 0, 0, err
	}
	defer closer()
	return notify.HistoryStats(db.DB)
}

// loadNotifyRaw 读出 panel.json 的 notify 段（不存在时返回空对象，便于增量构造）。
func loadNotifyRaw() (map[string]any, error) {
	cfg, err := config.LoadStrict()
	if err != nil {
		return nil, err
	}
	raw := cfg.Raw()
	if m, ok := raw["notify"].(map[string]any); ok && m != nil {
		return m, nil
	}
	return map[string]any{}, nil
}

// saveNotifyRaw 走 config.SetNotify（完整校验 + 原子写 0600），非法配置绝不落盘。
func saveNotifyRaw(notifyRaw map[string]any) error {
	out, err := fsx.MarshalCompact(notifyRaw)
	if err != nil {
		return err
	}
	if err := config.SetNotify(out); err != nil {
		return err
	}
	printRestartHint()
	return nil
}

func printRestartHint() {
	fmt.Println("已写入: " + config.ConfPath())
	fmt.Println("需重启面板生效: systemctl restart sbx-panel")
	fmt.Println("建议随后执行: sbx-core notify-test")
}

// NotifyTestCmd 立即发送一条测试卡片。
func NotifyTestCmd(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("notify-test 不接受参数")
	}
	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	if cfg.Notify == nil || !cfg.Notify.Enabled {
		return fmt.Errorf("推送未启用（请在管理菜单「系统设置与运维 → 流量推送配置」中配置）")
	}
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()

	mgr, err := notify.NewManager(cfg, db)
	if err != nil {
		return err
	}
	if mgr == nil {
		return fmt.Errorf("推送未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := mgr.SendTest(ctx); err != nil {
		return fmt.Errorf("测试推送失败: %s", notify.SafeErr(err))
	}
	fmt.Println("测试推送已发出，请查看飞书群。")
	return nil
}
