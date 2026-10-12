package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/notify"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// NotifyConfigCmd 处理 notify-config 子命令。
//
// 写入走 config.SetNotify（LoadStrict → 完整校验 → 原子写 0600），
// 非法配置绝不落盘——否则会把一个能正常启动的实例改成启动失败。
func NotifyConfigCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: sbx-core notify-config <set|get|show> [...]")
	}
	switch args[0] {
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
		fmt.Printf("已写入 notify 配置: %s\n", config.ConfPath())
		fmt.Println("需重启面板生效: systemctl restart sbx-core")
		fmt.Println("建议随后执行: sbx-core notify-test")
		return nil

	case "get":
		nc, err := loadNotify()
		if err != nil {
			return err
		}
		if nc == nil {
			fmt.Println("（未配置 notify，推送处于关闭状态）")
			return nil
		}
		out, err := fsx.MarshalIndent(redactNotify(nc))
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		fmt.Println("（凭据已脱敏；查看明文请用 notify-config show --unsafe）")
		return nil

	case "show":
		// 明文凭据需要显式确认：避免误敲时把 token 打到终端历史或 CI 日志里。
		if len(args) < 2 || args[1] != "--unsafe" {
			return fmt.Errorf("show 会打印明文凭据；确认请使用: sbx-core notify-config show --unsafe")
		}
		nc, err := loadNotify()
		if err != nil {
			return err
		}
		if nc == nil {
			fmt.Println("（未配置 notify）")
			return nil
		}
		out, err := fsx.MarshalIndent(nc)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	return fmt.Errorf("未知子命令 %q（可选 set/get/show）", args[0])
}

func loadNotify() (*config.NotifyConfig, error) {
	cfg, err := config.LoadStrict()
	if err != nil {
		return nil, err
	}
	return cfg.Notify, nil
}

// redactNotify 返回凭据已脱敏的副本，供终端展示。
func redactNotify(nc *config.NotifyConfig) *config.NotifyConfig {
	cp := *nc
	cp.Channels = make([]config.NotifyChannel, len(nc.Channels))
	copy(cp.Channels, nc.Channels)
	for i := range cp.Channels {
		ch := &cp.Channels[i]
		ch.BotToken = notify.RedactSecret(ch.BotToken)
		ch.SendKey = notify.RedactSecret(ch.SendKey)
		ch.Secret = notify.RedactSecret(ch.Secret)
		ch.Webhook = notify.RedactURL(ch.Webhook)
		ch.URL = notify.RedactURL(ch.URL)
	}
	return &cp
}

// NotifyTestCmd 向所有渠道发送一条测试消息。
func NotifyTestCmd(args []string) error {
	event := config.EventHeartbeat
	day := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event":
			if i+1 >= len(args) {
				return fmt.Errorf("--event 需要一个取值")
			}
			event = args[i+1]
			i++
		case "--day":
			if i+1 >= len(args) {
				return fmt.Errorf("--day 需要一个取值(YYYY-MM-DD)")
			}
			day = args[i+1]
			i++
		default:
			return fmt.Errorf("未知参数: %s", args[i])
		}
	}
	if event != config.EventHeartbeat && event != config.EventDaily {
		return fmt.Errorf("--event 只能是 heartbeat 或 daily")
	}

	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	if cfg.Notify == nil || !cfg.Notify.Enabled {
		return fmt.Errorf("notify 未启用（panel.json 中需要 notify.enabled=true）")
	}
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()

	// live 传 nil：测试消息不依赖采集器，避免在 nft 不可用的环境下无法自检。
	mgr, err := notify.NewManager(cfg, db, nil)
	if err != nil {
		return err
	}
	if mgr == nil {
		return fmt.Errorf("notify 未启用")
	}
	if day == "" {
		loc := traffic.Location(cfg.TZ)
		t := time.Now().In(loc)
		if event == config.EventDaily {
			t = t.AddDate(0, 0, -1)
		}
		day = t.Format("2006-01-02")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	results := mgr.SendTest(ctx, event, day)
	okCount := 0
	for _, r := range results {
		if r.Err == nil {
			fmt.Printf("  [成功] %s\n", r.Name)
			okCount++
			continue
		}
		fmt.Printf("  [失败] %s: %s\n", r.Name, notify.SafeErr(r.Err))
	}
	fmt.Printf("完成: %d/%d 个渠道成功（事件=%s，数据日=%s）\n", okCount, len(results), event, day)

	for _, ch := range cfg.Notify.Channels {
		// 免费版额度很小，而半小时心跳一天就有 48 条，几乎必然超额。
		if ch.Type == config.ChannelServerChan && ch.HasEvent(config.EventHeartbeat) {
			fmt.Println("提示: Server酱免费额度很小，建议改为 \"events\": [\"daily\"] 只收日结")
		}
	}
	if okCount == 0 {
		return fmt.Errorf("所有渠道均失败")
	}
	return nil
}

// NotifyReplayCmd 从聊天记录（或 Telegram 导出文件）恢复 daily 表。
func NotifyReplayCmd(args []string) error {
	opt := notify.ReplayOptions{}
	input := ""
	exportPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			opt.DryRun = true
		case "--overwrite":
			opt.Overwrite = true
		case "--only-missing":
			opt.OnlyMissing = true
		case "--telegram-export":
			if i+1 >= len(args) {
				return fmt.Errorf("--telegram-export 需要文件路径")
			}
			exportPath = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("未知参数: %s", args[i])
			}
			input = args[i]
		}
	}
	if opt.Overwrite && opt.OnlyMissing {
		return fmt.Errorf("--overwrite 与 --only-missing 不能同时使用")
	}

	var data []byte
	var err error
	switch {
	case exportPath != "":
		raw, rerr := os.ReadFile(exportPath)
		if rerr != nil {
			return fmt.Errorf("读取 %s 失败: %w", exportPath, rerr)
		}
		text, perr := extractTelegramExport(raw)
		if perr != nil {
			return perr
		}
		data = []byte(text)
	case input != "":
		data, err = os.ReadFile(input)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", input, err)
		}
	default:
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("读取标准输入失败: %w", err)
		}
	}

	cfg, err := config.LoadStrict()
	if err != nil {
		return err
	}
	db, closer, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer closer()

	res, err := notify.ReplayInto(db.DB, string(data), opt)
	if err != nil {
		return err
	}

	fmt.Printf("解析出 %d 天数据（日结 %d 天 / 心跳兜底 %d 天），跳过损坏片段 %d 处\n",
		res.Days, res.FromDaily, res.FromHB, res.Skipped)
	for _, d := range res.DayInfos {
		src := "日结(节点级终值)"
		if d.Source == "hb" {
			src = "心跳(仅当日总量，无节点拆分)"
		}
		fmt.Printf("  %s  ← %s  %d 行\n", d.Day, src, d.Rows)
	}
	for _, e := range res.Errors {
		fmt.Printf("  [警告] %s\n", e)
	}
	if opt.DryRun {
		fmt.Println("--dry-run: 未写入数据库")
		return nil
	}
	if res.Days == 0 {
		fmt.Println("未找到可恢复的数据（输入里可能没有 SBX1 行）")
		return nil
	}
	fmt.Println("已写入 daily 表。")
	// 如实告知：累计无法从日数据反推，伪造一个"看起来对"的值比留空更危险。
	fmt.Println("注意: totals（累计）未被恢复，需人工处理。")
	return nil
}

// extractTelegramExport 从 Telegram Desktop 导出的 JSON 里抽取消息文本。
//
// 之所以解析导出文件而不是调 Bot API：getUpdates 只返回最近 24 小时且要求未
// 设置 webhook，**拿不到频道完整历史**。导出聊天记录才是可取回历史的路径。
func extractTelegramExport(data []byte) (string, error) {
	var dump struct {
		Messages []struct {
			Text json.RawMessage `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return "", fmt.Errorf("不是有效的 Telegram 导出 JSON: %w", err)
	}
	if len(dump.Messages) == 0 {
		return "", fmt.Errorf("导出文件里没有 messages 数组（请确认用的是「导出聊天记录 → JSON」）")
	}
	var sb strings.Builder
	for _, m := range dump.Messages {
		var s string
		if err := json.Unmarshal(m.Text, &s); err == nil {
			sb.WriteString(s)
			sb.WriteByte('\n')
			continue
		}
		// 富文本消息会被导出成数组：[{"type":"plain","text":"..."}, ...]
		var parts []any
		if err := json.Unmarshal(m.Text, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			switch v := p.(type) {
			case string:
				sb.WriteString(v)
			case map[string]any:
				if t, ok := v["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}
