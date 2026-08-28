package connection

import "testing"

func TestParseConntrack(t *testing.T) {
	sample := "" +
		"ipv4     2 tcp      6 7199 ESTABLISHED src=112.115.49.228 dst=91.110.232.102 sport=35740 dport=8844 packets=1671 bytes=1906488 src=91.110.232.102 dst=112.115.49.228 sport=8844 dport=35740 packets=1696 bytes=141552 [ASSURED] mark=0 zone=0 use=2\n" +
		"ipv4     2 udp  17 30 src=8.8.8.8 dst=91.110.232.102 sport=53 dport=33333 packets=1 bytes=70 src=91.110.232.102 dst=8.8.8.8 sport=33333 dport=53 packets=1 bytes=80 [ASSURED] mark=0 zone=0 use=2\n" +
		"ipv4     2 tcp      6 431999 TIME_WAIT src=1.2.3.4 dst=91.110.232.102 sport=9999 dport=8844 packets=3 bytes=400 src=91.110.232.102 dst=1.2.3.4 sport=8844 dport=9999 packets=2 bytes=300 mark=0 zone=0 use=2\n"

	flows := ParseConntrack(sample)
	// 1 条 tcp ESTABLISHED + 1 条 udp；tcp TIME_WAIT 忽略。
	if len(flows) != 2 {
		t.Fatalf("应保留 tcp ESTABLISHED 与 udp 各 1 条，got %d", len(flows))
	}
	var tcp, udp *ConntrackFlow
	for i := range flows {
		f := &flows[i]
		if f.Proto == "tcp" {
			tcp = f
		} else {
			udp = f
		}
	}
	if tcp == nil || tcp.DstPort != 8844 || tcp.SrcIP != "112.115.49.228" || tcp.SrcPort != 35740 {
		t.Fatalf("tcp 流解析错误: %+v", tcp)
	}
	if tcp.Bytes != 1906488+141552 {
		t.Fatalf("tcp bytes 应双向相加, got %d", tcp.Bytes)
	}
	if udp == nil || udp.Proto != "udp" || udp.SrcIP != "8.8.8.8" || udp.DstPort != 33333 {
		t.Fatalf("udp 流解析错误: %+v", udp)
	}
}

func TestParseConntrackKeepsSynStates(t *testing.T) {
	sample := "" +
		"ipv4     2 tcp      6 120 SYN_SENT src=1.2.3.4 dst=91.110.232.102 sport=50000 dport=8844 packets=1 bytes=60 src=91.110.232.102 dst=1.2.3.4 sport=8844 dport=50000 packets=0 bytes=0 [UNREPLIED] mark=0 zone=0 use=2\n" +
		"ipv4     2 tcp      6 60  SYN_RECV src=5.6.7.8 dst=91.110.232.102 sport=51000 dport=8844 packets=1 bytes=60 src=91.110.232.102 dst=5.6.7.8 sport=8844 dport=51000 packets=1 bytes=60 mark=0 zone=0 use=2\n"

	flows := ParseConntrack(sample)
	if len(flows) != 2 {
		t.Fatalf("应保留 SYN_SENT + SYN_RECV 两条候选, got %d", len(flows))
	}
	for i := range flows {
		f := flows[i]
		if f.State != "SYN_SENT" && f.State != "SYN_RECV" {
			t.Fatalf("应保留握手态, got state=%q", f.State)
		}
	}
}

func TestParseConntrackEmptyAndMalformed(t *testing.T) {
	if got := ParseConntrack(""); len(got) != 0 {
		t.Fatalf("空文本应返回空, got %v", got)
	}
	bad := "this is not conntrack\nipv4 2 tcp 6 1 ESTABLISHED\n"
	if got := ParseConntrack(bad); len(got) != 0 {
		t.Fatalf("畸形/缺字段行应忽略, got %v", got)
	}
}
