package nodes

import (
	"fmt"
	"os"
	"strings"
)

// TagPrefix 是 sbx 管理的 inbound 标签前缀。
const TagPrefix = "sbx-n"

// Types 与旧 TYPES 一致。
var Types = []string{"vless", "shadowsocks", "trojan", "anytls"}

// ValidType 报告类型是否受支持。
func ValidType(t string) bool {
	for _, v := range Types {
		if v == t {
			return true
		}
	}
	return false
}

// TagOf 返回节点 inbound 标签。
func TagOf(id string) string { return TagPrefix + id }

// BuildInbound 由一条节点记录生成 sing-box inbound（单一数据源）。
func BuildInbound(n Node) (map[string]any, error) {
	t := Str(n, "type")
	tag := TagOf(IDString(n))
	port, err := toInt(n["port"])
	if err != nil {
		return nil, fmt.Errorf("端口非法")
	}
	base := map[string]any{
		"type":        t,
		"tag":         tag,
		"listen":      "::",
		"listen_port": port,
	}

	sni := Str(n, "sni")
	switch t {
	case "vless":
		user := map[string]any{"name": "u", "uuid": Str(n, "uuid")}
		if Truthy(n, "flow") {
			user["flow"] = Str(n, "flow")
		}
		base["users"] = []any{user}
		base["tls"] = map[string]any{
			"enabled":     true,
			"server_name": sni,
			"reality": map[string]any{
				"enabled":     true,
				"handshake":   map[string]any{"server": sni, "server_port": 443},
				"private_key": Str(n, "private_key"),
				"short_id":    []any{Str(n, "short_id")},
			},
		}
	case "shadowsocks":
		method := Str(n, "method")
		if method == "" {
			// 历史节点兼容：旧版默认 method，不重置 password
			method = SS2022Method128
		}
		base["method"] = method
		base["password"] = Str(n, "password")
	case "trojan":
		base["users"] = []any{map[string]any{"name": "u", "password": Str(n, "password")}}
		base["tls"] = tlsSelfsigned(n)
	case "anytls":
		base["users"] = []any{map[string]any{"name": "u", "password": Str(n, "password")}}
		base["tls"] = tlsSelfsigned(n)
	default:
		return nil, fmt.Errorf("不支持的节点类型: %s", t)
	}
	return base, nil
}

func tlsSelfsigned(n Node) map[string]any {
	return map[string]any{
		"enabled":          true,
		"server_name":      Str(n, "sni"),
		"certificate_path": Str(n, "cert"),
		"key_path":         Str(n, "key"),
	}
}

// RebuildConfig 保留用户其它字段，只重建由 sbx 管理的 inbounds。
func RebuildConfig(store *Store, list []Node) (map[string]any, error) {
	data, err := os.ReadFile(store.SBConf)
	if err != nil {
		return nil, fmt.Errorf("读取 sing-box 配置失败: %s", store.SBConf)
	}
	v, derr := DecodeJSON(data)
	if derr != nil {
		return nil, fmt.Errorf("读取 sing-box 配置失败: %s", store.SBConf)
	}
	cfg, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("读取 sing-box 配置失败: %s", store.SBConf)
	}

	others := make([]any, 0)
	if raw, exists := cfg["inbounds"].([]any); exists {
		for _, it := range raw {
			m, isMap := it.(map[string]any)
			if !isMap {
				continue // 防御：非对象条目跳过
			}
			tag, _ := m["tag"].(string)
			if strings.HasPrefix(tag, TagPrefix) {
				continue
			}
			others = append(others, it)
		}
	}

	built := make([]any, 0, len(list))
	for _, n := range list {
		inbound, berr := BuildInbound(n)
		if berr != nil {
			return nil, berr
		}
		built = append(built, inbound)
	}
	cfg["inbounds"] = append(others, built...)

	if _, exists := cfg["outbounds"]; !exists {
		cfg["outbounds"] = []any{map[string]any{"type": "direct", "tag": "direct"}}
	}
	route, exists := cfg["route"].(map[string]any)
	if !exists {
		route = map[string]any{}
		cfg["route"] = route
	}
	if _, hasFinal := route["final"]; !hasFinal {
		route["final"] = "direct"
	}
	return cfg, nil
}

// WriteCandidate 写出候选配置文件（SB_CONF.candidate），返回路径。
func WriteCandidate(store *Store, cfg map[string]any) (string, error) {
	path := store.SBConf + ".candidate"
	if err := saveJSONFile(path, cfg, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// WriteNodesCandidate 写出候选 nodes.json。
func WriteNodesCandidate(store *Store, list []Node) (string, error) {
	path := store.NodesPath() + ".candidate"
	arr := make([]any, len(list))
	for i, n := range list {
		arr[i] = map[string]any(n)
	}
	if err := saveJSONFile(path, arr, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
