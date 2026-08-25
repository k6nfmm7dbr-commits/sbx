// Package nodes 是节点领域模型：nodes.json/state.json 读写、端口与协议归属、
// sing-box inbound 构造、分享链接生成，以及原 nodes_tool.py 的全部 CLI 行为。
package nodes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// LoadToolNodes 对齐 nodes_tool.load_nodes：只接受顶层数组，任何异常静默返回 nil。
// 仅限只读场景（list/count/last/info/links 等）；修改类操作必须用 LoadToolNodesStrict。
func LoadToolNodes(path string) []Node {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	list, _, err := decodeNodesFile(data)
	if err != nil {
		return nil
	}
	return list
}

// LoadToolNodesStrict 严格读取：文件不存在视为空列表（保持“全新安装可直接添加”）；
// 但存在却损坏/顶层结构错误/节点语义非法时返回错误——修改类操作必须据此拒绝
// 覆盖原文件，防止把损坏误判为空而写回只剩新数据的文件。
func LoadToolNodesStrict(path string) ([]Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 不存在 = 空列表（与旧行为一致）
		}
		return nil, fmt.Errorf("nodes.json 读取失败: %w", err)
	}
	list, _, err := decodeNodesFile(data)
	if err != nil {
		return nil, fmt.Errorf("nodes.json 解析失败，已拒绝修改。原文件未被覆盖: %w", err)
	}
	if err := validateNodes(list); err != nil {
		return nil, fmt.Errorf("nodes.json 校验失败，已拒绝修改。原文件未被覆盖: %w", err)
	}
	return list, nil
}

// decodeNodesFile 解析顶层数组；返回 (列表, 是否为合法 JSON, 错误)。
// 严格模式要求：一个合法 JSON value 之后只允许空白与 EOF——
// `[] garbage`、`[] {}` 这类 trailing data 一律拒绝。
func decodeNodesFile(data []byte) ([]Node, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, true, fmt.Errorf("顶层必须是数组")
	}
	out := make([]Node, 0, len(list))
	for _, it := range list {
		m, isMap := it.(map[string]any)
		if !isMap {
			return nil, true, fmt.Errorf("数组元素必须是对象")
		}
		out = append(out, Node(m))
	}
	var extra any
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return out, true, nil
		}
		return nil, true, fmt.Errorf("尾部存在非法数据: %w", err)
	}
	return nil, true, fmt.Errorf("存在多个 JSON 值")
}

// LoadPanelNodes 对齐 panel.load_nodes：容忍 {"nodes":[...]} 包装，
// 且只保留含 "id" 字段的对象。
//
// 注意：宽松读取仅限纯展示场景。所有会生成/覆盖防火墙规则
// （nftables / iptables）或改写 counter_state / epoch 的路径必须使用
// LoadPanelNodesStrict。
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

// LoadPanelNodesStrict 严格读取面板节点列表：
//   - 文件不存在：返回空列表（全新安装合法状态，允许以 0 节点首次建规则）；
//   - 文件存在但读取失败 / JSON 损坏 / 顶层结构错误 / 节点语义非法：返回错误。
//
// 与宽松版不同：数组元素必须都是对象且含 id（不再 continue 跳过），并额外
// 校验 id/port/type。典型事故场景：nodes.json 损坏 → 宽松读取得到空列表 →
// Apply 以 0 节点重建防火墙并清掉全部 per-node 计数基线。strict 模式在改动
// 任何防火墙状态之前即中止。
func LoadPanelNodesStrict(path string) ([]Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 不存在 = 空列表（与全新安装行为一致）
		}
		return nil, fmt.Errorf("nodes.json 读取失败: %w", err)
	}
	list, err := decodePanelNodesStrict(data)
	if err != nil {
		return nil, fmt.Errorf("nodes.json 损坏, 拒绝据此修改防火墙规则(请修复 %s): %w", path, err)
	}
	if err := validateNodes(list); err != nil {
		return nil, fmt.Errorf("nodes.json 校验失败, 拒绝据此修改防火墙规则(%s): %w", path, err)
	}
	return list, nil
}

// decodePanelNodesStrict 严格解析面板节点文件（容忍 {"nodes":[...]} 包装）：
// 顶层只能是一个 JSON value + EOF；数组元素必须都是对象且含 id。
func decodePanelNodesStrict(data []byte) ([]Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		v = m["nodes"]
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("顶层必须是数组或 {\"nodes\":[...]}")
	}
	out := make([]Node, 0, len(list))
	for _, it := range list {
		m, isMap := it.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("数组元素必须是对象")
		}
		if _, has := m["id"]; !has {
			return nil, fmt.Errorf("节点缺少 id")
		}
		out = append(out, Node(m))
	}
	var extra any
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		return nil, fmt.Errorf("尾部存在非法数据: %w", err)
	}
	return nil, fmt.Errorf("存在多个 JSON 值")
}

// SaveNodesFile 原子写 nodes.json（indent=2 + 结尾换行，Python 兼容格式）。
func SaveNodesFile(path string, list []Node) error {
	arr := make([]any, len(list))
	for i, n := range list {
		arr[i] = map[string]any(n)
	}
	return saveJSONFile(path, arr, 0o600)
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
	return saveJSONFile(path, st, 0o600)
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

// strictInt64 严格整数校验：id/port 这类字段必须是真正的整数类型
// （json.Number / int64），不接受字符串形式（如 "1"）——字符串 id 属数据损坏。
func strictInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if t == float64(int64(t)) {
			return int64(t), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// validateNodes 对节点列表做语义校验：id 为正整数且不重复、port 在 1..65535
// 且不重复、type 属于受支持协议。用于所有 state-changing 路径前的兜底，
// 防止「明显损坏的节点数据」被当作空列表/合法数据继续修改 firewall/config。
func validateNodes(list []Node) error {
	seenID := map[int64]bool{}
	seenPort := map[int64]bool{}
	for _, n := range list {
		id, ok := strictInt64(n["id"])
		if !ok || id <= 0 {
			return fmt.Errorf("节点 id 非法(必须为正整数): %v", n["id"])
		}
		if seenID[id] {
			return fmt.Errorf("节点 id 重复: %d", id)
		}
		seenID[id] = true

		port, ok := strictInt64(n["port"])
		if !ok || port < 1 || port > 65535 {
			return fmt.Errorf("节点 %d 端口非法: %v", id, n["port"])
		}
		if seenPort[port] {
			return fmt.Errorf("节点端口重复: %d", port)
		}
		seenPort[port] = true

		t := Str(n, "type")
		if !ValidType(t) {
			return fmt.Errorf("节点 %d 类型不受支持: %q", id, t)
		}
	}
	return nil
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
