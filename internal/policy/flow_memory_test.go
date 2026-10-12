package policy

import (
	"encoding/json"
	"testing"
	"time"
	"unsafe"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// conntrack/parser 会返回指向整张输入文件的 substring。持久状态必须 Clone IP，
// 否则一个在线 IP 会让几 MB 的原始 conntrack buffer 长期存活。
func TestIPSlotClonesConntrackSubstring(t *testing.T) {
	buf := make([]byte, 1<<20)
	const offset = 8192
	ip := "203.0.113.77"
	copy(buf[offset:], ip)
	src := unsafe.String(&buf[offset], len(ip))
	inputLo := uintptr(unsafe.Pointer(&buf[0]))
	inputHi := inputLo + uintptr(len(buf))
	inside := func(s string) bool {
		p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
		return p >= inputLo && p < inputHi
	}

	st := newIPState()
	active := map[string]IPActivity{src: {IP: src, TCPSessions: 1, Traffic: true}}
	st.Reconcile(active, nil, 0, time.Now(), time.Minute, time.Minute, 10*time.Second)
	for key := range st.Slots {
		if inside(key) {
			t.Fatal("slot key 仍引用 1MiB 原始 conntrack buffer")
		}
	}
	for key := range st.Observed {
		if inside(key) {
			t.Fatal("observed key 仍引用 1MiB 原始 conntrack buffer")
		}
	}
}

func TestRepeatedConntrackParsesDoNotPersistInputBuffer(t *testing.T) {
	const sample = "ipv4 2 tcp 6 7199 ESTABLISHED src=198.51.100.23 dst=203.0.113.5 sport=44000 dport=443 packets=1 bytes=64 src=203.0.113.5 dst=198.51.100.23 sport=443 dport=44000 packets=1 bytes=64 [ASSURED] mark=0 zone=0 use=2\n"
	buf1, buf2 := []byte(sample), []byte(sample)
	text1 := unsafe.String(unsafe.SliceData(buf1), len(buf1))
	text2 := unsafe.String(unsafe.SliceData(buf2), len(buf2))
	flows1, flows2 := connection.ParseConntrack(text1), connection.ParseConntrack(text2)
	if len(flows1) != 1 || len(flows2) != 1 {
		t.Fatalf("重复解析都应各得到一条流，got %d/%d", len(flows1), len(flows2))
	}

	svc := &Service{flows: map[flowKey]flowState{}, ipIdle: time.Minute}
	list := []nodes.Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("443")}}
	now := time.Now()
	svc.buildActivity(list, connection.ConntrackResult{Available: true, Flows: flows1}, nil, now)
	svc.buildActivity(list, connection.ConntrackResult{Available: true, Flows: flows2}, nil, now.Add(time.Second))
	if len(svc.flows) != 1 {
		t.Fatalf("重复解析不应制造重复 flow key，got %d", len(svc.flows))
	}

	lo1, hi1 := uintptr(unsafe.Pointer(unsafe.SliceData(buf1))), uintptr(unsafe.Pointer(unsafe.SliceData(buf1)))+uintptr(len(buf1))
	lo2, hi2 := uintptr(unsafe.Pointer(unsafe.SliceData(buf2))), uintptr(unsafe.Pointer(unsafe.SliceData(buf2)))+uintptr(len(buf2))
	for key := range svc.flows {
		p := uintptr(unsafe.Pointer(unsafe.StringData(key.srcIP)))
		if (p >= lo1 && p < hi1) || (p >= lo2 && p < hi2) {
			t.Fatal("持久 flow key 与任一 conntrack 输入缓冲区共享底层存储")
		}
		for i := range buf1 {
			buf1[i] = 'x'
			buf2[i] = 'y'
		}
		if key.srcIP != "198.51.100.23" {
			t.Fatalf("修改解析输入后持久 key 被污染: %q", key.srcIP)
		}
	}
}

func TestFlowKeyClonesConntrackSubstring(t *testing.T) {
	buf := make([]byte, 1<<20)
	const offset = 16384
	ip := "198.51.100.23"
	copy(buf[offset:], ip)
	src := unsafe.String(&buf[offset], len(ip))
	inputLo := uintptr(unsafe.Pointer(&buf[0]))
	inputHi := inputLo + uintptr(len(buf))

	svc := &Service{flows: map[flowKey]flowState{}, ipIdle: time.Minute}
	list := []nodes.Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("443")}}
	cr := connection.ConntrackResult{
		Available: true,
		Flows:     []connection.ConntrackFlow{{Proto: "tcp", State: "ESTABLISHED", DstPort: 443, SrcIP: src, SrcPort: 44000, Bytes: 1024}},
	}
	svc.buildActivity(list, cr, nil, time.Now())
	if len(svc.flows) != 1 {
		t.Fatalf("期望 1 个 flow，得到 %d", len(svc.flows))
	}
	for key := range svc.flows {
		p := uintptr(unsafe.Pointer(unsafe.StringData(key.srcIP)))
		if p >= inputLo && p < inputHi {
			t.Fatal("flowKey.srcIP 仍引用 1MiB 原始 conntrack buffer")
		}
	}
}
