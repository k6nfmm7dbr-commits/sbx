package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// strictEnv 把 SBX_DIR/SBX_CONF 指向临时目录，返回 panel.json 路径。
func strictEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SBX_DIR", dir)
	t.Setenv("SBX_CONF", filepath.Join(dir, "panel.json"))
	return filepath.Join(dir, "panel.json")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
}

const validConf = `{"listen":"127.0.0.1","port":9999,"token":"secret-token","tz":"UTC"}`

// 文件不存在 = 合法全新安装状态，允许 defaults。
func TestLoadStrictMissingFileAllowsDefaults(t *testing.T) {
	path := strictEnv(t)
	c, err := LoadStrict()
	if err != nil {
		t.Fatalf("文件不存在应允许 defaults, 得到错误: %v", err)
	}
	if c.Listen != "0.0.0.0" || c.Token != "" {
		t.Fatalf("defaults 不正确: listen=%q token=%q", c.Listen, c.Token)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("LoadStrict 不得创建配置文件")
	}
}

// 文件存在但读取失败（路径是目录）→ 必须报错。
func TestLoadStrictReadErrorFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBX_DIR", dir)
	path := filepath.Join(dir, "panel.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBX_CONF", path)
	if _, err := LoadStrict(); err == nil {
		t.Fatal("ReadFile 失败必须返回错误")
	}
}

// 文件存在但 JSON 损坏 → 必须报错（绝不回退 defaults）。
func TestLoadStrictCorruptJSONFails(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, "{ invalid json")
	c, err := LoadStrict()
	if err == nil {
		t.Fatal("损坏的 JSON 必须返回错误")
	}
	if c != nil {
		t.Fatal("损坏时不得返回可用配置")
	}
}

// 尾部多余数据同样视为损坏。
func TestLoadStrictTrailingGarbageFails(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, validConf+" garbage")
	if _, err := LoadStrict(); err == nil {
		t.Fatal("尾部非法数据必须返回错误")
	}
	mustWrite(t, path, validConf+`{"x":1}`)
	if _, err := LoadStrict(); err == nil {
		t.Fatal("多个 JSON 值必须返回错误")
	}
}

// 合法文件正常读取。
func TestLoadStrictValidParses(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, validConf)
	c, err := LoadStrict()
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if c.Token != "secret-token" || c.Port != 9999 || c.Listen != "127.0.0.1" || c.TZ != "UTC" {
		t.Fatalf("字段解析不正确: %+v", c)
	}
}

// 场景 C：panel.json 损坏时 config-set 必须失败且原文件 byte-for-byte 不变。
func TestSetRejectsCorruptPanelJSON(t *testing.T) {
	path := strictEnv(t)
	corrupt := "{ broken json"
	mustWrite(t, path, corrupt)
	if err := Set("port", "1234"); err == nil {
		t.Fatal("损坏配置下 config-set 必须失败")
	}
	got, _ := os.ReadFile(path)
	if string(got) != corrupt {
		t.Fatalf("原文件被改动: %q", got)
	}
}

// 场景 C：panel.json 损坏时 config-ensure-token 必须失败且原文件不变。
func TestEnsureTokenRejectsCorruptPanelJSON(t *testing.T) {
	path := strictEnv(t)
	corrupt := "{ broken json"
	mustWrite(t, path, corrupt)
	if _, err := EnsureToken(); err == nil {
		t.Fatal("损坏配置下 config-ensure-token 必须失败")
	}
	got, _ := os.ReadFile(path)
	if string(got) != corrupt {
		t.Fatalf("原文件被改动: %q", got)
	}
}

// 正常路径回归：EnsureToken 在无 token 时生成并保留既有键；有 token 时直接返回。
func TestEnsureTokenHappyPathUnchanged(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, validConf)
	tok, err := EnsureToken()
	if err != nil || tok != "secret-token" {
		t.Fatalf("已有 token 应原样返回: %q %v", tok, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"listen":"127.0.0.1"`) {
		t.Fatalf("EnsureToken 不应破坏其它键: %s", data)
	}
}
