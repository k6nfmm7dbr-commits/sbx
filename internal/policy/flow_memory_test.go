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
