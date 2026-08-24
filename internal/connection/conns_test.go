package connection

import (
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

const tcpFixture = `  sl local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1
   1: 0100007F:1F90 0100007F:8AE4 01 00000000:00000000 00:00000000 00000000     0        0 2
   2: 00000000:0050 0100007F:8AE5 06 00000000:00000000 00:00000000 00000000     0        0 3
`

const tcp6Fixture = `  sl local_address remote_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 10
   1: 00000000000000000000000000000000:1F90 20010DB8000000000000000000000001:8AE4 01 00000000:00000000 00:00000000 00000000 0 0 11
`

const udpFixture = `  sl local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
   0: 00000000:007B 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 20 1 ...
   1: 00000000:007B 08080808:0035 07 00000000:00000000 00:00000000 00000000     0        0 21 1 ...
`

// RemConnected 边界：全零远端=未连接；其余（含无冒号防御分支）=已连接。
func TestRemConnected(t *testing.T) {
	cases := []struct {
		rem  string
		want bool
	}{
		{"00000000:0000", false},
		{"00000000000000000000000000000000:0000", false},
		{"08080808:0035", true},
		{"00000000:0001", true},         // 端口非零
		{"0100007F:0000", true},         // IP 非零
		{"0000000000000001:0000", true}, // v6 loopback
	}
	for _, c := range cases {
		if got := RemConnected(c.rem); got != c.want {
			t.Errorf("RemConnected(%q)=%v, want %v", c.rem, got, c.want)
		}
	}
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func readFake(files map[string]string) func(string) (string, error) {
	return func(path string) (string, error) {
		if text, ok := files[path]; ok {
			return text, nil
		}
		return "", errNotFound{}
	}
}

type errNotFound struct{}

func (errNotFound) Error() string { return "not found" }

func TestCountForNodes(t *testing.T) {
	files := map[string]string{
		"/proc/net/tcp":  tcpFixture,
		"/proc/net/tcp6": tcp6Fixture,
		"/proc/net/udp":  udpFixture,
		"/proc/net/udp6": "",
	}
	reader := readFake(files)

	list := []nodes.Node{
		{"id": int64(1), "type": "vless", "port": int64(8080)},      // 0x1F90 纯 TCP
		{"id": int64(2), "type": "shadowsocks", "port": int64(123)}, // 0x007B 双栈
	}
	got, err := countForNodes(list, reader)
	if err != nil {
		t.Fatal(err)
	}

	n1 := got["1"]
	if n1.TCP == nil || *n1.TCP != 2 { // tcp + tcp6 各一条 ESTABLISHED
		t.Errorf("node1 TCP=%v", n1.TCP)
	}
	if n1.UDP != nil {
		t.Error("vless 不应有 UDP 计数")
	}
	n2 := got["2"]
	if n2.TCP == nil || *n2.TCP != 0 {
		t.Errorf("node2 TCP 应为 0, got %v", n2.TCP)
	}
	if n2.UDP == nil || *n2.UDP != 1 { // 仅已 connect 的 DNS 会话计入
		t.Errorf("node2 UDP 应为 1, got %v", derefInt(n2.UDP))
	}
}

func TestParseLocalPortsSkipsHeaderAndBadLines(t *testing.T) {
	text := "header\nbadline\n   3: zz:XX 00000000:0000 01 x\n   4: 0100007F:0050 00000000:0000 01 x\n"
	var ports []int
	for _, p := range ParseLocalPorts(text, func(st, rem string) bool { return st == "01" }) {
		ports = append(ports, p)
	}
	if len(ports) != 1 || ports[0] != 80 {
		t.Errorf("应只解析出端口 80, got %v", ports)
	}
}
