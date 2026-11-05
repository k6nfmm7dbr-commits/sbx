package api

// appendCSVField 把单个字符串字段写入 CSV 行。
//
// 正常值（如 2023-11-14、node:1、system）原样写出，保持既有导出格式不变，
// 下游脚本仍能按裸 CSV 解析。仅当值可能破坏结构或被表格软件解释为公式时才处理：
//   - 含逗号 / 双引号 / 换行 → 双引号包裹，内部引号双写，换行折成空格
//     （保持一行一条记录）；
//   - 以 = + - @ 或制表符 / 换行开头 → 在引号内前置单引号，强制按文本解释
//     （CWE-1236 公式注入）。
//
// daily 表当前只有内部生成的 day 与 scope 两个字符串字段，外部不可控；
// 这里是前瞻性输出编码：将来把域名 / 节点名纳入导出时不会立刻变成公式注入。
func appendCSVField(dst []byte, v string) []byte {
	quote := false
	dangerous := false
	if v != "" {
		switch v[0] {
		case '=', '+', '-', '@', '\t', '\r', '\n':
			dangerous = true
		}
		for i := 0; i < len(v); i++ {
			switch v[i] {
			case ',', '"', '\r', '\n':
				quote = true
			}
		}
	}
	if !quote && !dangerous {
		return append(dst, v...)
	}
	dst = append(dst, '"')
	if dangerous {
		// 单引号必须放在引号**内部**：放在外面会让该字段变成 unquoted，
		// 引号又会被当成普通内容，各解析器行为不一致。
		dst = append(dst, '\'')
	}
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"':
			dst = append(dst, '"', '"')
		case '\r', '\n':
			dst = append(dst, ' ')
		default:
			dst = append(dst, v[i])
		}
	}
	return append(dst, '"')
}
