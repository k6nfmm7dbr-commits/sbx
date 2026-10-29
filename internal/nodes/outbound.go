package nodes

import "fmt"

// OutboundFor 把服务端节点记录翻译成**客户端** outbound——它必须是
// BuildInbound 的镜像，否则"面板能开、手机连不上"。
//
// 返回值约定：
//   - (outbound, "", nil)        成功
//   - (nil, reason, nil)         该协议无法被 sing-box 客户端承载（跳过，不算错误）
//   - (nil, "", err)             节点数据本身非法
//
// 为什么用 reason 而不是 error 表达"跳过"：Snell v5 是合法节点，只是
// sing-box 客户端没有对应实现。若当成错误，一个 v5 节点会让整份订阅
// 生成失败，用户看到的是"订阅打不开"而不是"少了一个节点"。
func OutboundFor(n Node, host, tag string) (map[string]any, string, error) {
	t := Str(n, "type")
	port, err := toInt(n["port"])
	if err != nil {
		return nil, "", fmt.Errorf("端口非法")
	}
	base := map[string]any{
		"type":        t,
		"tag":         tag,
		"server":      host,
		"server_port": port,
	}
	sni := Str(n, "sni")

	switch t {
	case "vless":
		base["uuid"] = Str(n, "uuid")
		if Truthy(n, "flow") {
			base["flow"] = Str(n, "flow")
		}
		base["tls"] = map[string]any{
			"enabled":     true,
			"server_name": sni,
			"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
			"reality": map[string]any{
				"enabled":    true,
				"public_key": Str(n, "public_key"),
				"short_id":   Str(n, "short_id"),
			},
		}

	case "shadowsocks":
		method := Str(n, "method")
		if method == "" {
			// 与 BuildInbound 保持一致：历史节点缺 method 时按 SS2022-128 处理。
			method = SS2022Method128
		}
		base["method"] = method
		base["password"] = Str(n, "password")

	case "trojan", "anytls":
		base["password"] = Str(n, "password")
		// 服务端用的是自签证书（tlsSelfsigned），客户端必须跳过校验，
		// 否则握手直接失败——这与分享链接里的 allowInsecure=1 是同一决定。
		base["tls"] = map[string]any{
			"enabled":     true,
			"server_name": sni,
			"insecure":    true,
			"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
		}

	case "socks":
		base["version"] = "5"
		if pw := Str(n, "password"); pw != "" {
			u := Str(n, "username")
			if u == "" {
				u = "u"
			}
			base["username"] = u
			base["password"] = pw
		}

	case "snell":
		v, _ := toInt(n["version"])
		if v < 6 {
			// 实测 sing-box 1.14.3：inbound 只认 v5/v6，outbound 只认 v4/v6。
			// 也就是说面板开的 v5 节点，sing-box 客户端根本没有对应实现，
			// 硬写进去会让整份订阅 check 失败（"snell: unsupported version: 5"），
			// 用户看到的是"订阅打不开"而不是"少了一个节点"。
			return nil, "Snell v5 无法被 sing-box 客户端导入，请用 v6 或改用 Shadowrocket", nil
		}
		base["version"] = 6
		base["psk"] = Str(n, "psk")
		// mode 留空则由客户端按 default 处理；v6 的 mode 与 inbound 的
		// mode 同名同义（default/unshaped），透传即可。
		if m := Str(n, "mode"); m != "" {
			base["mode"] = m
		}

	default:
		return nil, "", fmt.Errorf("不支持的节点类型: %s", t)
	}
	return base, "", nil
}
