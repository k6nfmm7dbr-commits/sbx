package connection

import "testing"

func TestParseConntrack(t *testing.T) {
	sample := "" +
		"ipv4     2 tcp      6 7199 ESTABLISHED src=112.115.49.228 dst=91.110.232.102 sport=35740 dport=8844 packets=1671 bytes=1906488 src=91.110.232.102 dst=112.115.49.228 sport=8844 dport=35740 packets=1696 bytes=141552 [ASSURED] mark=0 zone=0 use=2\n" +
		"ipv4     2 udp  17 30 src=8.8.8.8 dst=91.110.232.102 sport=53 dport=33333 packets=1 bytes=70 src=91.110.232.102 dst=8.8.8.8 sport=33333 dport=53 packets=1 bytes=80 [ASSURED] mark=0 zone=0 use=2\n" +
		"ipv4     2 tcp      6 431999 TIME_WAIT src=1.2.3.4 dst=91.110.232.102 sport=9999 dport=8844 packets=3 bytes=400 src=91.110.232.102 dst=1.2.3.4 sport=8844 dport=9999 packets=2 bytes=300 mark=0 zone=0 use=2\n"

	flows := ParseConntrack(sample)
	if len(flows) != 1 {
		t.Fatalf("应只保留 1 条 tcp ESTABLISHED 流（udp/TIME_WAIT 都忽略）, got %d", len(flows))
	}
	f := flows[0]
	if f.Proto != "tcp" || f.DstPort != 8844 || f.SrcIP != "112.115.49.228" || f.SrcPort != 35740 {
		t.Fatalf("流解析错误: %+v", f)
	}
	// bytes = 1906488 + 141552（双向）
	if f.Bytes != 1906488+141552 {
		t.Fatalf("bytes 应双向相加, got %d", f.Bytes)
	}
}

func TestParseConntrackEmptyAndMalformed(t *testing.T) {
	if got := ParseConntrack(""); got != nil && len(got) != 0 {
		t.Fatalf("空文本应返回空, got %v", got)
	}
	bad := "this is not conntrack\nipv4 2 tcp 6 1 ESTABLISHED\n"
	flows := ParseConntrack(bad)
	if len(flows) != 0 {
		t.Fatalf("畸形/缺字段行应忽略, got %v", flows)
	}
}
