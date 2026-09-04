package config

import (
	"encoding/json"
	"fmt"
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

// ---- nftables-only（v3.0.9）配置收敛 --------------------------------------

// 老 panel.json 里的历史配置（升级前由 v3.0.8 及更早版本生成）。
// 注意 backend=iptables 是最坏情况：旧版会据此真的去跑 iptables。
const legacyConf = `{
  "db": "/etc/sbx/traffic.db",
  "nodes_file": "/etc/sbx/nodes.json",
  "nft_conf": "/etc/sbx/nft.conf",
  "ipt_script": "/etc/sbx/iptables.sh",
  "web_root": "/etc/sbx/web",
  "backend": "iptables",
  "listen": "0.0.0.0",
  "port": 34567,
  "token": "user-token-keep-me",
  "interval": 2,
  "tz": "Asia/Shanghai",
  "custom_user_key": "keep-me-too"
}`

// 新生成的默认配置不得再包含 backend / ipt_script，且必须有 nft_conf。
func TestDefaultsHaveNoBackendKeys(t *testing.T) {
	strictEnv(t)
	c, err := LoadStrict()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"backend", "ipt_script"} {
		if _, ok := c.raw[k]; ok {
			t.Errorf("默认配置不应包含废弃键 %q", k)
		}
	}
	if c.raw["nft_conf"] == nil || c.NftConf == "" {
		t.Errorf("默认配置必须包含 nft_conf, got %v", c.raw["nft_conf"])
	}
}

// 老配置（含 backend=iptables + ipt_script）必须能读取，且不影响其它字段。
// 绝不能因为存在废弃键就拒绝启动——那会让老用户升级即炸。
func TestLoadStrictAcceptsLegacyKeys(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, legacyConf)
	c, err := LoadStrict()
	if err != nil {
		t.Fatalf("含废弃键的老配置必须可读: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("backend=iptables 不得导致校验失败: %v", err)
	}
	if c.Token != "user-token-keep-me" || c.Port != 34567 || c.TZ != "Asia/Shanghai" {
		t.Fatalf("用户配置被破坏: %+v", c)
	}
	if c.NftConf != "/etc/sbx/nft.conf" {
		t.Fatalf("nft_conf 解析错误: %q", c.NftConf)
	}
}

// MigrateLegacy：只删两个废弃键，其余（含用户自定义键）原样保留；幂等。
func TestMigrateLegacyDropsOnlyDeprecatedKeys(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, legacyConf)

	changed, err := MigrateLegacy()
	if err != nil || !changed {
		t.Fatalf("首次迁移应发生变更: changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("迁移后 JSON 损坏: %v\n%s", err, data)
	}
	for _, k := range []string{"backend", "ipt_script"} {
		if _, ok := raw[k]; ok {
			t.Errorf("迁移后仍存在废弃键 %q", k)
		}
	}
	for k, want := range map[string]any{
		"token":           "user-token-keep-me",
		"tz":              "Asia/Shanghai",
		"db":              "/etc/sbx/traffic.db",
		"nodes_file":      "/etc/sbx/nodes.json",
		"nft_conf":        "/etc/sbx/nft.conf",
		"custom_user_key": "keep-me-too",
	} {
		if got := raw[k]; got != want {
			t.Errorf("迁移破坏了配置 %s: %v != %v", k, got, want)
		}
	}
	if got := raw["port"]; fmt.Sprint(got) != "34567" {
		t.Errorf("port 被改动: %v", got)
	}
	// 权限必须仍是 600（含 token）
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("迁移后权限应为 600, got %o", perm)
	}

	// 幂等：再迁移一次不应有变更，且文件字节不变
	before, _ := os.ReadFile(path)
	changed, err = MigrateLegacy()
	if err != nil || changed {
		t.Fatalf("二次迁移应无变更: changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("幂等迁移改动了文件内容")
	}
}

// panel.json 不存在（全新安装）→ 无需迁移，不得创建文件。
func TestMigrateLegacyMissingFileIsNoop(t *testing.T) {
	path := strictEnv(t)
	changed, err := MigrateLegacy()
	if err != nil || changed {
		t.Fatalf("文件不存在应为 no-op: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("MigrateLegacy 不得创建配置文件")
	}
}

// panel.json 损坏 → 拒绝迁移且原文件 byte-for-byte 不变。
func TestMigrateLegacyRejectsCorrupt(t *testing.T) {
	path := strictEnv(t)
	corrupt := "{ broken json"
	mustWrite(t, path, corrupt)
	if _, err := MigrateLegacy(); err == nil {
		t.Fatal("损坏配置必须拒绝迁移")
	}
	got, _ := os.ReadFile(path)
	if string(got) != corrupt {
		t.Fatalf("原文件被改动: %q", got)
	}
}

// config-set 顺带完成迁移：写一个值的同时摘掉废弃键，其它键不动。
func TestSetDropsLegacyKeys(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, legacyConf)
	if err := Set("port", "18080"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"backend", "ipt_script"} {
		if _, ok := raw[k]; ok {
			t.Errorf("config-set 后仍存在废弃键 %q", k)
		}
	}
	if fmt.Sprint(raw["port"]) != "18080" {
		t.Errorf("port 未写入: %v", raw["port"])
	}
	if raw["token"] != "user-token-keep-me" || raw["custom_user_key"] != "keep-me-too" {
		t.Errorf("config-set 破坏了其它配置: %v", raw)
	}
}

// EnsureToken 同样顺带迁移，且不破坏用户其它配置。
func TestEnsureTokenDropsLegacyKeys(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, strings.Replace(legacyConf,
		`"token": "user-token-keep-me"`, `"token": ""`, 1))
	tok, err := EnsureToken()
	if err != nil || len(tok) != 32 {
		t.Fatalf("应生成 32 位令牌: %q %v", tok, err)
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"backend", "ipt_script"} {
		if _, ok := raw[k]; ok {
			t.Errorf("EnsureToken 后仍存在废弃键 %q", k)
		}
	}
	if raw["custom_user_key"] != "keep-me-too" || fmt.Sprint(raw["port"]) != "34567" {
		t.Errorf("EnsureToken 破坏了其它配置: %v", raw)
	}
}

// Validate 不再有 backend 这个概念：任何残留值都不得导致校验失败。
func TestValidateIgnoresLegacyBackendValues(t *testing.T) {
	for _, backend := range []string{"iptables", "auto", "nft", "ipt", "bogus", ""} {
		path := strictEnv(t)
		mustWrite(t, path, `{"listen":"127.0.0.1","port":9999,"token":"t","tz":"UTC","backend":"`+backend+`"}`)
		c, err := LoadStrict()
		if err != nil {
			t.Fatalf("backend=%q 读取失败: %v", backend, err)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("backend=%q 不得导致校验失败: %v", backend, err)
		}
	}
}

// nft_conf 为空仍必须被拒绝（它是唯一的规则文件路径）。
// LoadStrict 内含 Validate，所以这里直接断言 LoadStrict 失败。
func TestValidateRequiresNftConf(t *testing.T) {
	path := strictEnv(t)
	mustWrite(t, path, `{"listen":"127.0.0.1","port":9999,"token":"t","nft_conf":""}`)
	if _, err := LoadStrict(); err == nil {
		t.Fatal("nft_conf 为空必须校验失败")
	}
	// 对照：非空 nft_conf 正常通过
	mustWrite(t, path, `{"listen":"127.0.0.1","port":9999,"token":"t","nft_conf":"/etc/sbx/nft.conf"}`)
	if _, err := LoadStrict(); err != nil {
		t.Fatalf("合法 nft_conf 不应报错: %v", err)
	}
}
