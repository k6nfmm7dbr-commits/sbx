// Package nodes 是节点领域模型：nodes.json/state.json 读写、端口与协议归属、
// sing-box inbound 构造、分享链接生成，以及原 nodes_tool.py 的全部 CLI 行为。
package nodes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Node 用通用映射表示，保证 nodes.json 中的未知字段（cert/key/method/...）
// 在 /api/nodes 与写回时原样保留（旧 Python 行为）。
type Node map[string]any

// Store 持有节点相关文件路径。
type Store struct {
	AppDir string
	SBConf string
}

// NewStore 从环境变量构造（SBX_DIR / SBX_SB_CONF），默认值与旧实现一致。
func NewStore() *Store {
	app := os.Getenv("SBX_DIR")
	if app == "" {
		app = "/etc/sbx"
	}
	sb := os.Getenv("SBX_SB_CONF")
	if sb == "" {
		sb = "/etc/sing-box/config.json"
	}
	return &Store{AppDir: app, SBConf: sb}
}

func (s *Store) NodesPath() string { return filepath.Join(s.AppDir, "nodes.json") }
func (s *Store) StatePath() string { return filepath.Join(s.AppDir, "state.json") }
func (s *Store) CertDir() string   { return filepath.Join(s.AppDir, "certs") }

// DecodeJSON 以保留数字字面量的方式解析 JSON。
func DecodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// LoadToolNodes 对齐 nodes_tool.load_nodes：只接受顶层数组。
func LoadToolNodes(path string) []Node {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := DecodeJSON(data)
	if err != nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]Node, 0, len(list))
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			out = append(out, Node(m))
		}
	}
	return out
}

// LoadPanelNodes 对齐 panel.load_nodes：容忍 {"nodes":[...]} 包装，
// 且只保留含 "id" 字段的对象。
func LoadPanelNodes(path string) []Node {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := DecodeJSON(data)
	if err != nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		v = m["nodes"]
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]Node, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if _, has := m["id"]; has {
			out = append(out, Node(m))
		}
	}
	return out
}

// SaveNodesFile 原子写 nodes.json（indent=2 + 结尾换行，Python 兼容格式）。
func SaveNodesFile(path string, list []Node) error {
	arr := make([]any, len(list))
	for i, n := range list {
		arr[i] = map[string]any(n)
	}
	return saveJSONFile(path, arr, 0o644)
}

// LoadState 读取 state.json（损坏时返回空表，不报错——对齐 read_json 容错）。
func LoadState(path string) map[string]any {
	st := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	v, err := DecodeJSON(data)
	if err != nil {
		return st
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return st
}

// SaveState 原子写 state.json。
func SaveState(path string, st map[string]any) error {
	return saveJSONFile(path, st, 0o644)
}

// NextID 单调递增分配节点 ID，永不回收；立即持久化游标到 state.json。
// 兜底兼容“已有节点 id 更大”的手工数据（对齐 next_id 注释语义）。
func NextID(store *Store, loaded []Node) (int64, error) {
	st := LoadState(store.StatePath())
	var base int64
	if v, ok := st["next_node_id"]; ok {
		if n, err := toInt(v); err == nil {
			base = n
		}
	}
	var usedMax int64
	for _, n := range loaded {
		if id, err := IDOf(n); err == nil && id > usedMax {
			usedMax = id
		}
	}
	nid := max(base, usedMax) + 1
	st["next_node_id"] = json.Number(strconv.FormatInt(nid, 10))
	if err := SaveState(store.StatePath(), st); err != nil {
		return 0, err
	}
	return nid, nil
}

// IDOf 提取节点 id 为 int64。非法时返回错误（等价 Python 的类型异常路径）。
func IDOf(n Node) (int64, error) {
	v, ok := n["id"]
	if !ok {
		return 0, fmt.Errorf("节点缺少 id")
	}
	return toInt(v)
}

// IDString 返回与 Python "%s" % id 一致的字符串形式。
func IDString(n Node) string {
	if v, ok := n["id"]; ok {
		switch t := v.(type) {
		case json.Number:
			return t.String()
		case string:
			return t
		default:
			return fmt.Sprint(t)
		}
	}
	return ""
}

func toInt(v any) (int64, error) {
	switch t := v.(type) {
	case json.Number:
		return t.Int64()
	case float64:
		return int64(t), nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	}
	return 0, fmt.Errorf("不是整数: %v", v)
}

// ParsePorts 对齐 panel.parse_ports：唯一监听端口 -> [(p,p)]，非法返回空。
func ParsePorts(n Node) [][2]int64 {
	p, err := toInt(n["port"])
	if err != nil || p < 1 || p > 65535 {
		return nil
	}
	return [][2]int64{{p, p}}
}

var protoTransports = map[string][]string{
	"vless":       {"tcp"},
	"trojan":      {"tcp"},
	"anytls":      {"tcp"},
	"shadowsocks": {"tcp", "udp"},
}

// Protocols 对齐 node_protocols：计数规则与连接数显示的传输层归属；
// 未知类型默认 TCP+UDP 双栈。
func Protocols(n Node) []string {
	t := strings.ToLower(Str(n, "type"))
	if ps, ok := protoTransports[t]; ok {
		return ps
	}
	return []string{"tcp", "udp"}
}

// Str 取字符串字段；缺失返回 ""。
func Str(n Node, key string) string {
	switch t := n[key].(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// Truthy 判断字段是否为 Python 意义上的真值（非空串/非零/非 null）。
func Truthy(n Node, key string) bool {
	switch t := n[key].(type) {
	case nil:
		return false
	case string:
		return t != ""
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	case bool:
		return t
	default:
		return true
	}
}

// DisplayName 对齐 name 兜底链：name -> type -> "node<id>"。
func DisplayName(n Node) string {
	if s := Str(n, "name"); s != "" {
		return s
	}
	if s := Str(n, "type"); s != "" {
		return s
	}
	return "node" + IDString(n)
}

// TruncateRunes 按“字符数”截断（对齐 Python s[:16] 的字符语义，中文按 1 字符计）。
func TruncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
