package nodes

// PublicNodeDTO 是 /api/nodes 对外暴露的脱敏节点信息。
// 内部 Node（map[string]any）可能含 password / uuid / Reality private_key /
// public_key / short_id / cert / key 等服务器私密材料，绝不能直接序列化给
// 普通面板客户端——面板 token 不应等价于代理节点私钥。
type PublicNodeDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Port int    `json:"port"`
}

// PublicNodes 把内部节点列表转为脱敏 DTO 列表。仅保留展示所需字段。
// id/port 解析失败时按 0 输出（节点数据异常时宁可展示 0，也不回退到秘密字段）。
func PublicNodes(list []Node) []PublicNodeDTO {
	out := make([]PublicNodeDTO, 0, len(list))
	for _, n := range list {
		id, _ := toInt(n["id"])
		port, _ := toInt(n["port"])
		out = append(out, PublicNodeDTO{
			ID:   id,
			Name: DisplayName(n),
			Type: Str(n, "type"),
			Port: int(port),
		})
	}
	return out
}
