// sbx-core 是 SBX 的 Go 后端：流量采集、HTTP 面板 API 与节点管理，
// 替代旧 panel.py / nodes_tool.py（服务器不再需要 Python 运行时）。
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx/internal/service"
	"github.com/k6nfmm7dbr-commits/sbx/internal/version"
)

func main() {
	setupLogging()

	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(serveOrDefault())
	}

	cmd := args[0]
	switch cmd {
	case "serve":
		os.Exit(service.Serve())
	case "once":
		must(service.Once())
	case "show":
		must(service.Show())
	case "daily":
		days := 14
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				usageFatal()
			}
			days = n
		}
		must(service.Daily(days))
	case "reset":
		scope := ""
		if len(args) > 1 {
			scope = args[1]
		}
		must(service.Reset(scope))
	case "rules":
		if _, err := service.Rules(); err != nil {
			fail(err)
		}
	case "apply":
		os.Exit(service.Apply())
	case "clear":
		os.Exit(service.Clear())
	case "selftest":
		os.Exit(service.SelfTest())
	case "config-get":
		if len(args) < 2 {
			usageFatal()
		}
		// 缺失键打印空行而非 Go 的 "<nil>"，便于 shell 用 $(config-get k) 判空。
		if v := config.Load().Get(args[1]); v != nil {
			fmt.Println(fmt.Sprint(v))
		} else {
			fmt.Println()
		}
	case "config-set":
		if len(args) < 3 {
			usageFatal()
		}
		if err := config.Set(args[1], args[2]); err != nil {
			fail(err)
		}
	case "config-ensure-token":
		tok, err := config.EnsureToken()
		if err != nil {
			fail(err)
		}
		fmt.Println(tok)
	case "node":
		cli := &nodes.CLI{Store: nodes.NewStore(), Stdout: os.Stdout, Stderr: os.Stderr}
		os.Exit(cli.Run(args[1:]))
	case "secret":
		// sbx-core secret <hex|base64> <n>：crypto/rand 生成随机 secret，
		// 供 shell 生成 panel token / Trojan / AnyTLS 密码（统一随机源）。
		if len(args) < 3 {
			usageFatal()
		}
		n, err := strconv.Atoi(args[2])
		if err != nil || n <= 0 {
			usageFatal()
		}
		switch args[1] {
		case "hex":
			s, gerr := nodes.GenerateHex(n)
			if gerr != nil {
				fail(gerr)
			}
			fmt.Println(s)
		case "base64":
			s, gerr := nodes.GenerateBase64(n)
			if gerr != nil {
				fail(gerr)
			}
			fmt.Println(s)
		default:
			usageFatal()
		}
	case "config-migrate":
		// 一次性清理已废弃的配置键（backend / ipt_script）。
		// nftables-only 收敛后这两个键不再有任何作用；升级路径调用此命令，
		// 只删这两个键、其余配置（token/port/db/nodes_file/tz/自定义键）原样保留。
		changed, err := config.MigrateLegacy()
		if err != nil {
			fail(err)
		}
		if changed {
			fmt.Println("已清理废弃配置键: backend, ipt_script")
		} else {
			fmt.Println("配置无需迁移")
		}
	case "version", "--version", "-v":
		fmt.Printf("sbx-core v%s\n", version.Version)
	case "help", "-h", "--help":
		printUsage()
	default:
		usageFatal()
	}
}

func serveOrDefault() int {
	return service.Serve()
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-core]", err)
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "[sbx-core]", err)
	os.Exit(1)
}

func usageFatal() {
	printUsage()
	os.Exit(2)
}

func printUsage() {
	fmt.Print(`sbx-core — SBX Go 后端 (流量采集 / 面板 API / 节点管理)

用法:
  sbx-core serve                          启动面板与采集器
  sbx-core once                           执行单轮采集
  sbx-core show                           命令行查看今日/累计
  sbx-core daily [N]                      每日流量表（默认14天）
  sbx-core reset [scope]                  清空统计数据（如 node:2）
  sbx-core rules                          生成计数规则文件
  sbx-core apply                          重建并应用计数规则
  sbx-core clear                          移除计数规则
  sbx-core selftest                       计数器自检
  sbx-core config-get <key>               读配置项
  sbx-core config-set <key> <value>       写配置项
  sbx-core config-ensure-token            保证访问令牌存在
  sbx-core config-migrate                 清理废弃配置键(backend/ipt_script)
  sbx-core node <sub-command> [...]       节点管理（add/edit/remove/...）
  sbx-core secret <hex|base64> <n>         生成随机 secret（crypto/rand）
  sbx-core version                        版本信息
`)
}

func setupLogging() {
	level := slog.LevelInfo
	switch os.Getenv("SBX_LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}
