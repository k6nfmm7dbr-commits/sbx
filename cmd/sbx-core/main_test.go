package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CLI 契约测试（审计项「测试覆盖」+「CLI 分发重构」的替代改进）。
//
// 目的：把 sbx.sh 正在依赖的**输出格式与退出码**变成可回归断言。
// 这些是隐式接口——改 CLI 框架或调整输出时，这里会先红。
//
// 用真实二进制而不是直接调 main()：main() 通过 os.Exit 结束进程，
// 无法在进程内断言退出码；而退出码正是 shell 侧在用的契约。
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sbxcli")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(dir, "sbx-core")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		panic("构建 sbx-core 失败: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	return code, out.String(), errb.String()
}

// 版本输出格式：sbx.sh 与安装器用 `sbx-core version` 做安装后自检，
// 必须是一行且含 "sbx-core v"。
func TestCLIVersionFormat(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		code, out, _ := run(t, arg)
		if code != 0 {
			t.Errorf("%s: 退出码应为 0, got %d", arg, code)
		}
		if !strings.HasPrefix(out, "sbx-core v") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
			t.Errorf("%s: 输出格式异常: %q", arg, out)
		}
	}
}

// 未知命令 / 缺参数：退出码必须是 2（usage 错误），且用法打到 stdout。
func TestCLIUsageErrors(t *testing.T) {
	cases := [][]string{
		{"不存在的命令"},
		{"config-get"},          // 缺 key
		{"config-set", "only"},  // 缺 value
		{"secret", "hex"},       // 缺 n
		{"secret", "hex", "0"},  // n 非法
		{"secret", "bad", "16"}, // 类型非法
	}
	for _, args := range cases {
		code, out, _ := run(t, args...)
		if code != 2 {
			t.Errorf("%v: 退出码应为 2, got %d", args, code)
		}
		if !strings.Contains(out, "用法") {
			t.Errorf("%v: 应打印用法, got %q", args, out)
		}
	}
}

// config-get 对缺失键必须打印**空行**而不是 Go 的 "<nil>"：
// shell 侧用 `[[ -z "$(sbx-core config-get k)" ]]` 判空，输出 "<nil>" 会让判断失效。
func TestCLIConfigGetMissingKeyPrintsEmptyLine(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "panel.json")
	// 空配置（合法）：只给最小必需字段
	if err := os.WriteFile(conf, []byte(`{"db":"`+dir+`/t.db","nodes_file":"`+dir+
		`/nodes.json","nft_conf":"`+dir+`/nft.conf"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binPath, "config-get", "definitely_missing_key")
	cmd.Env = append(os.Environ(), "SBX_CONF="+conf)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("config-get 应成功: %v", err)
	}
	if string(out) != "\n" {
		t.Errorf("缺失键应打印单个空行, got %q", string(out))
	}
}

// help 必须覆盖全部已实现子命令（避免文档漂移）。
func TestCLIHelpListsSubcommands(t *testing.T) {
	_, out, _ := run(t, "--help")
	for _, sub := range []string{
		"serve", "once", "show", "daily", "reset", "rules", "apply", "clear",
		"selftest", "config-get", "config-set", "config-ensure-token",
		"config-migrate", "node", "secret", "version",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("help 未列出子命令 %q", sub)
		}
	}
}

// secret 子命令必须产出符合长度契约的随机值（shell 侧用它们生成
// panel token / Trojan / AnyTLS 密码）。
func TestCLISecretOutput(t *testing.T) {
	code, out, _ := run(t, "secret", "hex", "16")
	if code != 0 {
		t.Fatalf("secret hex 失败: rc=%d", code)
	}
	if got := len(strings.TrimSpace(out)); got != 32 {
		t.Errorf("secret hex 16 应产出 32 个十六进制字符, got %d", got)
	}
	// base64 是**标准** Base64（含 +/= 与填充），不是 URL-safe——这是既有契约：
	// 密码进分享链接时由 PyQuote 负责把 +/= 百分号转义（见 internal/nodes/pyquote.go），
	// 因此这里只断言"能按标准 Base64 解码回 n 字节"，不要顺手改成 URL-safe
	// （那会改变已发出的分享链接里的密码形态）。
	code, out, _ = run(t, "secret", "base64", "32")
	if code != 0 {
		t.Fatalf("secret base64 失败: rc=%d", code)
	}
	raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if derr != nil {
		t.Errorf("base64 secret 应按标准 Base64 解码: %v (%q)", derr, out)
	}
	if len(raw) != 32 {
		t.Errorf("base64 secret 应解出 32 字节, got %d", len(raw))
	}
	// 两次调用必须不同（crypto/rand）
	_, out2, _ := run(t, "secret", "hex", "16")
	if out == out2 {
		t.Error("两次 secret 输出相同，随机源异常")
	}
}

// 会改动系统/数据库状态的子命令在配置损坏时必须 fail-closed（非 0），
// 绝不在 defaults 的猜测下动防火墙或数据库。
func TestCLIFailsClosedOnBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "panel.json")
	if err := os.WriteFile(conf, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"once"}, {"rules"}, {"apply"}, {"clear"}, {"reset"}} {
		cmd := exec.Command(binPath, args...)
		cmd.Env = append(os.Environ(), "SBX_CONF="+conf, "SBX_DIR="+dir)
		if err := cmd.Run(); err == nil {
			t.Errorf("%v: 配置损坏时应非 0 退出（fail-closed）", args)
		}
	}
}

// 纯只读子命令（config-get）刻意保持宽松读取：配置损坏时用默认值继续，
// 便于用户登不上服务时仍能读出当前（可能是默认的）配置来排查。
// 这与上面的 fail-closed 是**有意为之的差异**，在此锁定以免被误改。
func TestCLIConfigGetStaysLenientOnBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "panel.json")
	if err := os.WriteFile(conf, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binPath, "config-get", "port")
	cmd.Env = append(os.Environ(), "SBX_CONF="+conf, "SBX_DIR="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("config-get 应保持宽松（不因配置损坏而失败）: %v", err)
	}
	if strings.TrimSpace(string(out)) != "8080" {
		t.Errorf("应回退到默认端口 8080, got %q", string(out))
	}
}
