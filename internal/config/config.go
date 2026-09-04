// Package config 负责 panel.json 的加载与写回。
// 行为对齐旧 Python panel.py：内置默认值 + 文件覆盖；config-set 把
// 合并后的完整配置写回（保留未知键，补齐默认键），chmod 600。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

// AppDir 返回数据目录（环境变量 SBX_DIR 优先）。
func AppDir() string {
	if v := os.Getenv("SBX_DIR"); v != "" {
		return v
	}
	return "/etc/sbx"
}

// ConfPath 返回面板配置文件路径（SBX_CONF 优先）。
func ConfPath() string {
	if v := os.Getenv("SBX_CONF"); v != "" {
		return v
	}
	return filepath.Join(AppDir(), "panel.json")
}

func defaultConf() map[string]any {
	dir := AppDir()
	return map[string]any{
		"db":         filepath.Join(dir, "traffic.db"),
		"nodes_file": filepath.Join(dir, "nodes.json"),
		"nft_conf":   filepath.Join(dir, "nft.conf"),
		"web_root":   filepath.Join(dir, "web"),
		"listen":     "0.0.0.0",
		"port":       json.Number("8080"),
		"token":      "",
		"interval":   json.Number("2"),
		"tz":         "Asia/Shanghai",
	}
}

// legacyKeys 是已废弃的历史配置键（v3.0.9 nftables-only 收敛后移除）。
// 语义：SBX 曾支持 nftables/iptables 双后端，用 backend 选择、ipt_script
// 指向生成的 iptables 脚本。现在只有 nftables，这两个键不再有任何作用。
//
// 处理策略（不能因为存在旧键就拒绝读取，那会让老用户升级即炸）：
//   - 读取：忽略其值，不参与任何决策（后端恒为 nftables）；
//   - 写回（config-set / EnsureToken）：顺带删除这两个键，一次性完成迁移，
//     其余用户配置（token/port/db/nodes_file/tz/未知自定义键）一律原样保留。
var legacyKeys = []string{"backend", "ipt_script"}

// dropLegacyKeys 从 raw 配置里剔除废弃键，返回是否发生了删除。
func dropLegacyKeys(raw map[string]any) bool {
	changed := false
	for _, k := range legacyKeys {
		if _, ok := raw[k]; ok {
			delete(raw, k)
			changed = true
		}
	}
	return changed
}

// Config 是合并后的运行配置。raw 保持原始键值（含未知键），供 config-get / config-set。
type Config struct {
	raw map[string]any

	DB           string
	NodesFile    string
	NftConf      string
	WebRoot      string // 兼容保留：Go 版前端内嵌于二进制，此键不再被读取
	Listen       string
	Port         int
	Token        string
	Interval     int
	TZ           string
	SecureCookie bool
}

// Load 读取配置。文件不存在或损坏时使用默认值并记日志（与 Python 行为一致）。
//
// 注意：Load 是宽松读取，仅限纯只读场景（config-get / show / daily 等）。
// serve / config-set / config-ensure-token / apply / rules / clear 等会
// 启动网络服务、修改配置或系统状态的路径必须使用 LoadStrict（fail-closed）。
func Load() *Config {
	c, _ := load(ConfPath(), false)
	// 宽松：语义非法仅记日志（只读场景不强中断）。
	if err := c.Validate(); err != nil {
		slog.Warn("配置语义校验失败(仅告警)", "err", err)
	}
	return c
}

// LoadStrict 严格读取配置（fail-closed）：
//   - 文件不存在：返回默认值（合法的全新安装状态）；
//   - 文件存在但 ReadFile 失败：返回错误；
//   - 文件存在但 JSON 损坏（含尾部多余数据）：返回错误；
//   - 文件存在且合法：正常合并读取并做语义校验。
func LoadStrict() (*Config, error) {
	c, err := load(ConfPath(), true)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// load 是 Load/LoadStrict 共享的读取实现；只在「损坏是否允许 fallback」策略层分开。
func load(path string, strict bool) (*Config, error) {
	c := &Config{raw: defaultConf()}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		slog.Info("配置不存在, 使用默认值", "path", path)
	case err != nil:
		if strict {
			return nil, fmt.Errorf("配置文件读取失败, 拒绝使用默认值继续 (%s): %w", path, err)
		}
		slog.Warn("配置读取失败, 使用默认值", "path", path, "err", err)
	default:
		file, derr := decodeConfig(data)
		if derr != nil {
			if strict {
				return nil, fmt.Errorf("配置文件损坏, 拒绝使用默认值继续(请修复 %s 或恢复备份): %w", path, derr)
			}
			slog.Warn("配置解析失败", "err", derr)
		} else {
			for k, v := range file {
				c.raw[k] = v
			}
		}
	}
	c.normalize()
	return c, nil
}

// decodeConfig 解析单个 JSON 对象；尾部只允许空白与 EOF（`} garbage`、
// 多个 JSON 值视为损坏）。
func decodeConfig(data []byte) (map[string]any, error) {
	var file map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("存在多个 JSON 值")
		}
		return nil, fmt.Errorf("尾部存在非法数据: %w", err)
	}
	return file, nil
}

// Validate 语义校验：拒绝明确会导致服务异常/不安全的配置。
// listen 与 timezone 不做强校验——空 listen 属「未配置」（服务层已按公网拒绝
// 空 token），timezone 由 traffic.Location 宽容兜底（UTC+8 等也合法）。
//
// 注意：不校验 backend——nftables-only 架构下它已不是配置项。老 panel.json 里
// 残留的 backend 值（含 "iptables"）一律忽略，绝不因此拒绝启动。
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port 必须在 1-65535: %d", c.Port)
	}
	if c.Interval < 1 {
		return fmt.Errorf("interval 必须 > 0")
	}
	if c.Interval > 86400 {
		return fmt.Errorf("interval 过大: %d", c.Interval)
	}
	if c.DB == "" || c.NodesFile == "" {
		return fmt.Errorf("db/nodes 路径不能为空")
	}
	if c.NftConf == "" {
		return fmt.Errorf("nft 规则文件路径不能为空")
	}
	return nil
}

func (c *Config) normalize() {
	c.DB = c.Str("db")
	c.NodesFile = c.Str("nodes_file")
	c.NftConf = c.Str("nft_conf")
	c.WebRoot = c.Str("web_root")
	c.Listen = c.Str("listen")
	c.Port = int(c.Int("port"))
	c.Token = strings.TrimSpace(c.Str("token"))
	c.Interval = int(c.Int("interval"))
	if c.Interval < 1 {
		c.Interval = 1
	}
	c.TZ = c.Str("tz")
	c.SecureCookie = c.Bool("secure_cookie")
}

// Bool 读取布尔值；仅接受明确的 true（字符串 "true"/"1"/"yes"，或 JSON true）。
func (c *Config) Bool(key string) bool {
	switch t := c.raw[key].(type) {
	case bool:
		return t
	case string:
		v := strings.ToLower(strings.TrimSpace(t))
		return v == "true" || v == "1" || v == "yes"
	case json.Number:
		return t.String() == "1"
	default:
		return false
	}
}

// Get 返回原始值（供 CLI config-get 打印）。
func (c *Config) Get(key string) any { return c.raw[key] }

// Str 取字符串值；数字按其字面量转成十进制字符串（与 str(v) 对齐）。
func (c *Config) Str(key string) string {
	v := c.raw[key]
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "True" // 与 Python str(True) 对齐，shell 侧判断才不破
		}
		return "False"
	default:
		return fmt.Sprint(t)
	}
}

// Int 尽力把值转为整数；失败返回 0。
func (c *Config) Int(key string) int64 {
	switch t := c.raw[key].(type) {
	case nil:
		return 0
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return int64(f)
		}
	case float64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

// Set 写回单个配置项（int 可解析则存数字，否则存字符串——与 Python 一致），
// 并把合并后的完整配置原子写回 CONF_PATH，权限 600。
// 读取阶段使用 LoadStrict：panel.json 存在但损坏时拒绝修改并原样返回错误，
// 绝不允许“损坏→回退 defaults→把默认值写回正式文件”的覆盖路径。
//
// 顺带完成 nftables-only 迁移：写回时剔除已废弃的 backend / ipt_script。
// 只删这两个 SBX 自己废弃的键——token/port/db/nodes_file/tz 以及用户自定义的
// 未知键一律原样保留，且走 fsx 的「临时文件 → fsync → 原子 rename」。
func Set(key, value string) error {
	c, err := LoadStrict()
	if err != nil {
		return fmt.Errorf("拒绝修改配置: %w", err)
	}
	var v any = value
	trimmed := strings.TrimSpace(value)
	if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil && trimmed != "" {
		v = json.Number(strconv.FormatInt(i, 10))
	}
	c.raw[key] = v
	if dropLegacyKeys(c.raw) {
		slog.Info("已移除废弃配置键(nftables-only)", "keys", legacyKeys)
	}
	c.normalize()
	data, err := fsx.MarshalIndent(c.raw)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsx.WriteFileAtomic(ConfPath(), data, 0o600)
}

// MigrateLegacy 一次性清理已废弃的配置键（backend / ipt_script）。
// 幂等：没有旧键时不写盘、返回 (false, nil)。
//
// 由升级路径显式调用（sbx-core config-migrate）。刻意与 Set 分开：
// 升级时不一定需要修改任何配置值，只想把废弃键摘掉。
//
// 安全性：只删 legacyKeys 里的键，其余内容（含未知自定义键）原样保留；
// 写回走 fsx.WriteFileAtomic（临时文件 + fsync + 原子 rename），权限 600。
// panel.json 不存在时视为无需迁移；损坏时返回错误、绝不覆盖。
func MigrateLegacy() (bool, error) {
	if _, err := os.Stat(ConfPath()); os.IsNotExist(err) {
		return false, nil
	}
	c, err := LoadStrict()
	if err != nil {
		return false, fmt.Errorf("拒绝迁移配置: %w", err)
	}
	if !dropLegacyKeys(c.raw) {
		return false, nil
	}
	data, err := fsx.MarshalIndent(c.raw)
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if err := fsx.WriteFileAtomic(ConfPath(), data, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureToken 保证 token 非空；为空则生成 32 位十六进制随机令牌并写回。
// 替代旧安装器里的内联 python3 secrets.token_hex(16)。
// 读取阶段使用 LoadStrict：panel.json 存在但损坏时直接报错，
// 绝不基于 defaults 生成新文件覆盖原损坏文件（避免丢失原有配置内容）。
func EnsureToken() (string, error) {
	c, err := LoadStrict()
	if err != nil {
		return "", fmt.Errorf("拒绝生成访问令牌: %w", err)
	}
	tok := c.Token
	if tok != "" {
		return tok, nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok = hex.EncodeToString(b)
	c.raw["token"] = tok
	dropLegacyKeys(c.raw) // 顺带完成 nftables-only 迁移
	data, err := fsx.MarshalIndent(c.raw)
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := fsx.WriteFileAtomic(ConfPath(), data, 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// Raw 暴露合并后的原始映射（仅供导出/调试使用）。
func (c *Config) Raw() map[string]any { return c.raw }
