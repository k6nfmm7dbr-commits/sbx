package connection

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// 构造接近真实规模的 /proc/net/tcp 文本（含表头 + n 行 ESTABLISHED）。
func synthTCP(n int) string {
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i := 0; i < n; i++ {
		b.WriteString("   ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(": 0100007F:")
		b.WriteString(strings.ToUpper(strconv.FormatInt(int64(30000+i%20000), 16)))
		b.WriteString(" 5DB8D822:")
		b.WriteString(strings.ToUpper(strconv.FormatInt(int64(40000+i%20000), 16)))
		b.WriteString(" 01 00000000:00000000 02:00000000 00000000  1000        0 ")
		b.WriteString(strconv.Itoa(100000 + i))
		b.WriteString(" 1 0000000000000000 100 0 0 10 0\n")
	}
	return b.String()
}

// 审计项「连接数读取 /proc」要求补基准：量化解析成本，防止将来回退到
// 每行 strings.Fields 的分配放大实现。
func BenchmarkParseLocalPorts(b *testing.B) {
	text := synthTCP(10000)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	keep := func(st, rem string) bool { return st == tcpEstablished }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(ParseLocalPorts(text, keep)); got != 10000 {
			b.Fatalf("解析行数 %d, 期望 10000", got)
		}
	}
}

func BenchmarkParseRemoteIPs(b *testing.B) {
	text := synthTCP(10000)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	keep := func(st, rem string) bool { return st == tcpEstablished }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseRemoteIPs(text, keep)
	}
}

func BenchmarkRemoteIPsByPort(b *testing.B) {
	text := synthTCP(10000)
	b.ReportAllocs()
	read := func(string) (string, error) { return text, nil }
	keep := func(st, rem string) bool { return st == tcpEstablished }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		RemoteIPsByPort(tcpProcFiles, keep, read)
	}
}

// 真实 /proc 规模（本机实际数据），仅在 Linux 上运行。
func BenchmarkParseLocalPortsReal(b *testing.B) {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		b.Skip("no /proc/net/tcp")
	}
	text := string(data)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	keep := func(st, rem string) bool { return st == tcpEstablished }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseLocalPorts(text, keep)
	}
}

// 端到端对比：模拟"1 万连接、仅 1 个节点端口"的真实形态。
// 旧实现（allPorts）会为 1 万个临时源端口各建一个 map 条目；
// 过滤后只保留命中节点端口的那一个。这组基准就是该优化的证据。
func BenchmarkCountByPortAllPorts(b *testing.B) {
	text := synthTCP(10000)
	b.ReportAllocs()
	read := func(string) (string, error) { return text, nil }
	keep := func(st, rem string) bool { return st == tcpEstablished }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CountByPortFiltered(tcpProcFiles, keep, read, allPorts)
	}
}

func BenchmarkCountByPortNodePortOnly(b *testing.B) {
	text := synthTCP(10000)
	b.ReportAllocs()
	read := func(string) (string, error) { return text, nil }
	keep := func(st, rem string) bool { return st == tcpEstablished }
	// 与 synthTCP 生成的端口区间(30000..49999)相交的单个端口
	want := PortFilter(func(p int) bool { return p == 30005 })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, _ := CountByPortFiltered(tcpProcFiles, keep, read, want)
		if len(hits) != 1 {
			b.Fatalf("应只命中 1 个端口, got %d", len(hits))
		}
	}
}

func BenchmarkRemoteIPsByPortNodePortOnly(b *testing.B) {
	text := synthTCP(10000)
	b.ReportAllocs()
	read := func(string) (string, error) { return text, nil }
	keep := func(st, rem string) bool { return st == tcpEstablished }
	want := PortFilter(func(p int) bool { return p == 30005 })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, _ := RemoteIPsByPortFiltered(tcpProcFiles, keep, read, want)
		if len(out) != 1 {
			b.Fatalf("应只记录 1 个端口, got %d", len(out))
		}
	}
}

// 语义等价性：优化后的迭代实现必须与「Split + Fields」参考实现逐字段一致。
func TestProcParsersMatchReference(t *testing.T) {
	text := synthTCP(500) + "\n" + "  999: bad line\n\n"
	keep := func(st, rem string) bool { return st == tcpEstablished }

	// 参考实现（旧写法），仅用于本测试对照
	refPorts := func() []int {
		var ports []int
		lines := strings.Split(text, "\n")
		for _, line := range lines[min(1, len(lines)):] {
			parts := strings.Fields(line)
			if len(parts) < 4 {
				continue
			}
			local, rem, st := parts[1], parts[2], parts[3]
			i := strings.IndexByte(local, ':')
			if i < 0 || !keep(st, rem) {
				continue
			}
			p, err := strconv.ParseInt(local[i+1:], 16, 64)
			if err != nil {
				continue
			}
			ports = append(ports, int(p))
		}
		return ports
	}()

	got := ParseLocalPorts(text, keep)
	if len(got) != len(refPorts) {
		t.Fatalf("端口数不一致: got %d want %d", len(got), len(refPorts))
	}
	for i := range got {
		if got[i] != refPorts[i] {
			t.Fatalf("第 %d 个端口不一致: got %d want %d", i, got[i], refPorts[i])
		}
	}

	refIPs := func() map[string]bool {
		out := map[string]bool{}
		lines := strings.Split(text, "\n")
		for _, line := range lines[min(1, len(lines)):] {
			parts := strings.Fields(line)
			if len(parts) < 4 {
				continue
			}
			rem, st := parts[2], parts[3]
			if !keep(st, rem) {
				continue
			}
			if ip := parseRemoteIP(rem); ip != "" {
				out[ip] = true
			}
		}
		return out
	}()
	gotIPs := ParseRemoteIPs(text, keep)
	if len(gotIPs) != len(refIPs) {
		t.Fatalf("远端 IP 数不一致: got %d want %d", len(gotIPs), len(refIPs))
	}
	for ip := range refIPs {
		if !gotIPs[ip] {
			t.Errorf("缺少远端 IP %s", ip)
		}
	}
}

// 边界：空文本、仅表头、无换行结尾、超短行都不能 panic。
func TestProcParsersEdgeCases(t *testing.T) {
	keep := func(string, string) bool { return true }
	for _, text := range []string{"", "\n", "header\n", "header", "header\na", "header\n\n\n",
		"header\n  0: 0100007F:1F90\n"} {
		if got := ParseLocalPorts(text, keep); len(got) != 0 {
			t.Errorf("ParseLocalPorts(%q)=%v, 期望空", text, got)
		}
		if got := ParseRemoteIPs(text, keep); len(got) != 0 {
			t.Errorf("ParseRemoteIPs(%q)=%v, 期望空", text, got)
		}
	}
}
