package nodes

// PublicNodeDTO 是 /api/nodes 对外暴露的管理元数据。
// 内部 Node（map[string]any）可能含 password / uuid / Reality private_key /
// public_key / short_id / cert / key 等私密材料，绝不能直接序列化给普通面板客户端。
type PublicNodeDTO struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Protocol      string `json:"protocol"`
	Port          int    `json:"port"`
	SNI           string `json:"sni,omitempty"`
	Method        string `json:"method,omitempty"`
	Version       int64  `json:"version,omitempty"`
	Paused        bool   `json:"paused,omitempty"`
	IPLimitOn     bool   `json:"ip_limit_enabled,omitempty"`
	IPLimitMax    int    `json:"ip_limit_max,omitempty"`
	RateLimitOn   bool   `json:"rate_limit_enabled,omitempty"`
	RateLimitMbps int    `json:"rate_limit_mbps,omitempty"`
	// PortConflictWith 仅供节点管理恢复 UI 标记历史重复端口，不改变策略/enforcement。
	PortConflictWith []int64 `json:"port_conflict_with,omitempty"`
}

// PublicNodes 把内部节点列表转为仅含展示/编辑元数据的脱敏 DTO。
// id/port 解析失败时按 0 输出（节点数据异常时宁可展示 0，也不回退到秘密字段）。
func PublicNodes(list []Node) []PublicNodeDTO {
	out := make([]PublicNodeDTO, 0, len(list))
	for _, n := range list {
		id, _ := toInt(n["id"])
		port, _ := toInt(n["port"])
		version, _ := toInt(n["version"])
		out = append(out, PublicNodeDTO{
			ID:       id,
			Name:     DisplayName(n),
			Type:     DisplayType(n),
			Protocol: Str(n, "type"),
			Port:     int(port),
			SNI:      Str(n, "sni"),
			Method:   Str(n, "method"),
			Version:  version,
		})
	}
	return out
}
