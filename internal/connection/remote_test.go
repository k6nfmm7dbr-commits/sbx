package connection

import (
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

func TestParseRemoteIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// IPv4 小端：0100007F = 127.0.0.1
		{"0100007F:8AE4", "127.0.0.1"},
		// 08080808 = 8.8.8.8
		{"08080808:0035", "8.8.8.8"},
		// IPv6：2001:db8::1（/proc 格式：每 4 字节 word 内小端）
		{"B80D0120000000000000000001000000:8AE4", "2001:db8::1"},
		// 全零 → 0.0.0.0
		{"00000000:0000", "0.0.0.0"},
		// 非法
		{"zzzz:0000", ""},
		{"0100007", ""},
	}
	for _, c := range cases {
		if got := parseRemoteIP(c.in); got != c.want {
			t.Errorf("parseRemoteIP(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseRemoteIPsDedup(t *testing.T) {
	// 同一 IP 多连接只算 1 个
	text := `  sl local_address rem_address   st tx_queue rx_queue
   0: 0100007F:1F90 0100007F:8AE4 01 00000000:00000000
   1: 0100007F:1F90 0100007F:8AE5 01 00000000:00000000
   2: 0100007F:1F90 08080808:8AE6 01 00000000:00000000
`
	ips := ParseRemoteIPs(text, func(st, rem string) bool { return st == "01" })
	if len(ips) != 2 {
		t.Fatalf("应去重为 2 个 IP, got %v", ips)
	}
	if !ips["127.0.0.1"] || !ips["8.8.8.8"] {
		t.Errorf("IP 集合异常: %v", ips)
	}
}

func TestRemoteIPsByPort(t *testing.T) {
	files := map[string]string{
		"/proc/net/tcp": `  sl local_address rem_address   st tx_queue rx_queue
   0: 0100007F:1F90 08080808:8AE4 01 00000000:00000000
   1: 0100007F:1F90 08080808:8AE5 01 00000000:00000000
`,
	}
	reader := readFake(files)
	out, partial := RemoteIPsByPort(tcpProcFiles[:1], func(st, rem string) bool { return st == "01" }, reader)
	if partial {
		t.Error("不应 partial")
	}
	set := out[8080] // 0x1F90 = 8080
	if len(set) != 1 || !set["8.8.8.8"] {
		t.Fatalf("端口 8080 应只有 8.8.8.8 一个 IP, got %v", set)
	}
}

func TestNodeRemoteIPsSplit(t *testing.T) {
	files := map[string]string{
		"/proc/net/tcp": `  sl local_address rem_address   st tx_queue rx_queue
   0: 0100007F:1F90 08080808:8AE4 01 00000000:00000000
`,
		"/proc/net/udp": `  sl local_address rem_address   st tx_queue rx_queue
   0: 00000000:007B 08080808:0035 07 00000000:00000000
`,
		"/proc/net/tcp6": "",
		"/proc/net/udp6": "",
	}
	reader := readFake(files)
	list := []nodes.Node{{"id": int64(1), "type": "shadowsocks", "port": int64(8080)}}
	split, partial, err := NodeRemoteIPsSplit(list, reader)
	if err != nil || partial {
		t.Fatalf("err=%v partial=%v", err, partial)
	}
	rs := split["1"]
	if !rs.TCP["8.8.8.8"] {
		t.Errorf("TCP 应含 8.8.8.8: %v", rs.TCP)
	}
	if rs.UDP["8.8.8.8"] {
		t.Errorf("UDP 不应含 8.8.8.8（UDP fixture 端口是 123 非 8080）: %v", rs.UDP)
	}
}
