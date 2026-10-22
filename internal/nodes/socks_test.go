package nodes

import "testing"

// SOCKS5 协议支持回归：inbound 生成、分享链接、传输层归属。
func TestSocksNode(t *testing.T) {
	if !ValidType("socks") {
		t.Fatal("socks 应为合法节点类型")
	}
	withAuth := Node{"id": 9, "type": "socks", "port": 30110, "name": "s1",
		"username": "u", "password": "pw123"}
	ib, err := BuildInbound(withAuth)
	if err != nil {
		t.Fatal(err)
	}
	if ib["type"] != "socks" || ib["listen_port"] != int64(30110) {
		t.Fatalf("inbound 基础字段错误: %#v", ib)
	}
	users, ok := ib["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("带密码必须生成 users: %#v", ib)
	}
	u0 := users[0].(map[string]any)
	if u0["username"] != "u" || u0["password"] != "pw123" {
		t.Fatalf("认证字段错误: %#v", u0)
	}
	// 空密码：不设 users（开放代理，sing-box 语义），由使用者自担风险
	open, err := BuildInbound(Node{"id": 9, "type": "socks", "port": 30110})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := open["users"]; exists {
		t.Fatalf("空密码不应生成 users: %#v", open)
	}
	// 分享链接
	if l := (&Store{}).LinkFor(withAuth, "1.2.3.4", ""); l != "socks5://u:pw123@1.2.3.4:30110#s1" {
		t.Fatalf("分享链接错误: %q", l)
	}
	if l := (&Store{}).LinkFor(Node{"id": 9, "type": "socks", "port": 30110, "name": "s1"}, "1.2.3.4", ""); l != "socks5://1.2.3.4:30110#s1" {
		t.Fatalf("无密码分享链接错误: %q", l)
	}
	// 传输层：tcp+udp（SOCKS5 支持 UDP ASSOCIATE）
	if ps := Protocols(withAuth); len(ps) != 2 || ps[0] != "tcp" || ps[1] != "udp" {
		t.Fatalf("socks 传输层应为 tcp+udp: %#v", ps)
	}
}
