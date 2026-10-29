package nodes

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestOutboundForMirrorsInbound(t *testing.T) {
	cases := []struct {
		name string
		node Node
		want map[string]any
	}{
		{
			name: "vless+reality 必须带 utls 与 reality 公钥",
			node: Node{"id": 1, "type": "vless", "port": 30001, "name": "v1",
				"uuid": "11111111-1111-1111-1111-111111111111", "sni": "a.example.com",
				"public_key": "PBK", "short_id": "ab12"},
			want: map[string]any{"uuid": "11111111-1111-1111-1111-111111111111"},
		},
		{
			name: "shadowsocks 缺 method 时回退 SS2022-128",
			node: Node{"id": 2, "type": "shadowsocks", "port": 30002, "name": "s1", "password": "pw"},
			want: map[string]any{"method": SS2022Method128},
		},
		{
			name: "trojan 自签证书必须 insecure",
			node: Node{"id": 3, "type": "trojan", "port": 30003, "name": "t1",
				"password": "pw", "sni": "b.example.com"},
			want: map[string]any{"password": "pw"},
		},
		{
			name: "socks 带认证",
			node: Node{"id": 4, "type": "socks", "port": 30004, "name": "k1",
				"username": "u", "password": "pw"},
			want: map[string]any{"username": "u", "version": "5"},
		},
		{
			name: "snell v6",
			node: Node{"id": 5, "type": "snell", "port": 30005, "name": "sn1",
				"psk": "psk", "version": 6},
			want: map[string]any{"psk": "psk", "version": 6},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ob, reason, err := OutboundFor(tc.node, "1.2.3.4", "tag-"+tc.name)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if reason != "" {
				t.Fatalf("不应跳过: %s", reason)
			}
			if ob["server"] != "1.2.3.4" {
				t.Fatalf("server = %v", ob["server"])
			}
			if ob["tag"] != "tag-"+tc.name {
				t.Fatalf("tag = %v", ob["tag"])
			}
			for k, v := range tc.want {
				if ob[k] != v {
					t.Fatalf("%s = %v, want %v", k, ob[k], v)
				}
			}
		})
	}
}

func TestOutboundForTLSDetails(t *testing.T) {
	vless := Node{"id": 1, "type": "vless", "port": 1, "uuid": "u", "sni": "a.com",
		"public_key": "PBK", "short_id": "sid", "flow": "xtls-rprx-vision"}
	ob, _, err := OutboundFor(vless, "h", "t")
	if err != nil {
		t.Fatal(err)
	}
	tls := ob["tls"].(map[string]any)
	reality := tls["reality"].(map[string]any)
	if reality["public_key"] != "PBK" || reality["short_id"] != "sid" {
		t.Fatalf("reality 字段错误: %+v", reality)
	}
	if ob["flow"] != "xtls-rprx-vision" {
		t.Fatalf("flow 未透传: %v", ob["flow"])
	}

	trojan := Node{"id": 2, "type": "trojan", "port": 2, "password": "p", "sni": "b.com"}
	ob2, _, _ := OutboundFor(trojan, "h", "t2")
	if ob2["tls"].(map[string]any)["insecure"] != true {
		t.Fatal("trojan 必须 insecure=true（服务端为自签证书）")
	}
}

func TestOutboundForSnellV5Skipped(t *testing.T) {
	// 实测 sing-box 1.14.3 的 outbound 只认 v4/v6，v5 会让整份配置解析失败。
	// 所以 v5 必须被跳过并说明原因，而不是写进配置。
	for _, v := range []int{0, 5} {
		n := Node{"id": 1, "type": "snell", "port": 1, "psk": "p", "version": v}
		ob, reason, err := OutboundFor(n, "h", "t")
		if err != nil {
			t.Fatalf("v%d 不应是错误（否则一个节点会毁掉整份订阅）: %v", v, err)
		}
		if ob != nil || reason == "" {
			t.Fatalf("v%d 应被跳过并给出原因，得到 ob=%v reason=%q", v, ob, reason)
		}
	}
}

func TestBuildSingboxProfile(t *testing.T) {
	list := []Node{
		{"id": 1, "type": "shadowsocks", "port": 30001, "name": "香港", "method": "aes-128-gcm", "password": "p1"},
		{"id": 2, "type": "shadowsocks", "port": 30002, "name": "香港", "method": "aes-128-gcm", "password": "p2"},
		{"id": 3, "type": "snell", "port": 30003, "name": "v5", "psk": "p3", "version": 5},
	}
	profile, skipped, err := BuildSingboxProfile(&Store{}, list, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "Snell v5") {
		t.Fatalf("skipped = %v", skipped)
	}

	obs := profile["outbounds"].([]any)
	// 2 个节点 + selector + direct
	if len(obs) != 4 {
		t.Fatalf("outbounds 数量 = %d, want 4", len(obs))
	}
	// 同名节点必须被去重成不同 tag，否则 sing-box 拒绝加载。
	t1 := obs[0].(map[string]any)["tag"]
	t2 := obs[1].(map[string]any)["tag"]
	if t1 == t2 {
		t.Fatalf("重名节点 tag 未去重: %v", t1)
	}
	sel := obs[2].(map[string]any)
	if sel["type"] != "selector" || sel["default"] != t1 {
		t.Fatalf("selector 错误: %+v", sel)
	}
	// 默认选中第一个节点，且 selector 的候选必须是真实节点 tag。
	cand := sel["outbounds"].([]string)
	if len(cand) != 2 {
		t.Fatalf("selector 候选 = %v", cand)
	}
	if profile["inbounds"].([]any) == nil {
		t.Fatal("inbounds 必须是空数组（由客户端注入 tun）")
	}
}

func TestBuildSingboxProfileNoHost(t *testing.T) {
	_, _, err := BuildSingboxProfile(&Store{}, []Node{{"id": 1, "type": "shadowsocks", "port": 1, "password": "p"}}, "")
	if err == nil {
		t.Fatal("缺分享地址时必须报错，而不是生成一个 server 为空的配置")
	}
}

func TestEncodeSubscription(t *testing.T) {
	got := EncodeSubscription([]string{"ss://a", "ss://b"})
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("必须是无填充以外的标准 base64: %v", err)
	}
	if string(raw) != "ss://a\nss://b" {
		t.Fatalf("解码 = %q", raw)
	}
}
