package policy

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func act(ip string, tcp, udp int, traffic bool) IPActivity {
	return IPActivity{IP: ip, TCPSessions: tcp, UDPSessions: udp, Traffic: traffic}
}

// cand 表示一个「发起了 TCP 握手、尚未 ESTABLISHED」的候选。
func cand(ip string) IPActivity {
	return IPActivity{IP: ip, TCPSessions: 1}
}

const (
	hour = time.Hour
	ttl  = time.Hour
	prov = 10 * time.Second
)

// 满 Slot / 超卖测试：max=2，A 已在线，同轮出现 B/C/D，只能授予 A、B，C/D 拒绝。
func TestSlotAdmissionNoOversell(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 2, testNow, hour, ttl, prov)

	allow, hasRejected := st.Reconcile(map[string]IPActivity{
		"A": act("A", 1, 0, true),
		"B": act("B", 1, 0, true),
		"C": act("C", 1, 0, true),
		"D": act("D", 1, 0, true),
	}, nil, 2, testNow.Add(time.Second), hour, ttl, prov)

	if !hasRejected {
		t.Fatal("应有新拒绝")
	}
	if _, ok := allow["A"]; !ok {
		t.Fatal("A 应保持 granted")
	}
	if _, ok := allow["B"]; !ok {
		t.Fatal("B 应被 granted")
	}
	if _, ok := allow["C"]; ok {
		t.Fatal("C 不应被 granted")
	}
	if _, ok := allow["D"]; ok {
		t.Fatal("D 不应被 granted")
	}
	if st.grantedCount() != 2 {
		t.Fatalf("granted 应为 2, got %d", st.grantedCount())
	}
}

// 满 Slot 测试：A/B 都仍在线、满额后新候选 C 不得进入 granted。
func TestFullSlotRejects(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)}, nil,
		2, testNow, hour, ttl, prov)
	allow, _ := st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)},
		map[string]IPActivity{"C": cand("C")}, 2, testNow.Add(time.Second), hour, ttl, prov)
	if _, ok := allow["C"]; ok {
		t.Fatal("满额后 C 不得进入 granted")
	}
}

// 释放测试：本轮不再 active（conntrack 已无 ESTABLISHED 流）→ 立即释放，下轮新 IP 可补位。
func TestSlotReleaseImmediateWhenNotSeen(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)}, nil,
		2, testNow, hour, ttl, prov)
	// B 不再 active（断开）→ 立即释放；C 尝试 → 补位。
	allow, _ := st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "C": act("C", 1, 0, true)}, nil,
		2, testNow.Add(time.Second), hour, ttl, prov)
	if _, ok := allow["B"]; ok {
		t.Fatal("B 断开后应立即释放")
	}
	if _, ok := allow["C"]; !ok {
		t.Fatal("B 释放后 C 应能补位")
	}
}

// NAT 测试：同一 IP 多个 TCP 流，online_ip 仍算 1。
func TestNATSingleIP(t *testing.T) {
	st := newIPState()
	allow, _ := st.Reconcile(map[string]IPActivity{"223.1.1.1": act("223.1.1.1", 10, 3, true)}, nil,
		5, testNow, hour, ttl, prov)
	if st.grantedCount() != 1 || len(allow) != 1 {
		t.Fatalf("同一公网 IP 应算 1, got granted=%d allow=%d", st.grantedCount(), len(allow))
	}
}

// TCP+UDP 测试：同一 IP 同时有 TCP 与 UDP，只算 1 个 IP。
func TestTCPUDPSingleIP(t *testing.T) {
	st := newIPState()
	allow, _ := st.Reconcile(map[string]IPActivity{"9.9.9.9": act("9.9.9.9", 2, 4, true)}, nil,
		0, testNow, hour, ttl, prov)
	if st.grantedCount() != 1 || len(allow) != 1 {
		t.Fatalf("TCP+UDP 同一 IP 应算 1, got count=%d allow=%d", st.grantedCount(), len(allow))
	}
}

// 候选（SYN 未建立）在有名额时应被临时授予（进 allow set）。
func TestCandidateGrantedBeforeEstablished(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 2, testNow, hour, ttl, prov)
	allow, _ := st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, map[string]IPActivity{"B": cand("B")},
		2, testNow.Add(time.Second), hour, ttl, prov)
	if _, ok := allow["B"]; !ok {
		t.Fatal("有余位时候选 B 应被临时授予进 allow set")
	}
	slot := st.Slots["B"]
	if slot == nil || !slot.Provisional {
		t.Fatalf("B 应为 provisional slot: %+v", slot)
	}
	// 候选不计在线 IP
	if st.activeGrantedCount() != 1 {
		t.Fatalf("候选未建立不应计在线 IP, got %d", st.activeGrantedCount())
	}
}

// 第二个 IP 获取第二个 slot：A 已 Granted，B 发 SYN → B 获得 slot 并最终建立。
func TestSecondIPAcquiresSecondSlot(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 2, testNow, hour, ttl, prov)
	// B 先以候选出现
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, map[string]IPActivity{"B": cand("B")}, 2, testNow.Add(time.Second), hour, ttl, prov)
	if st.grantedCount() != 2 {
		t.Fatalf("B 候选应获第二个 slot, granted=%d", st.grantedCount())
	}
	// B 建立 → provisional 清除，在线 IP=2
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)}, nil, 2, testNow.Add(2*time.Second), hour, ttl, prov)
	if st.activeGrantedCount() != 2 {
		t.Fatalf("B 建立后在线应为 2, got %d", st.activeGrantedCount())
	}
	if st.Slots["B"].Provisional {
		t.Fatal("B 建立后不应再是 provisional")
	}
}

// 第三个 IP 在两个 slot 已满时拒绝，且不进 allow set。
func TestThirdIPRejectedWhenTwoSlotsFull(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)}, nil, 2, testNow, hour, ttl, prov)
	allow, hasRejected := st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)},
		map[string]IPActivity{"C": cand("C")}, 2, testNow.Add(time.Second), hour, ttl, prov)
	if !hasRejected {
		t.Fatal("C 应被拒绝")
	}
	if _, ok := allow["C"]; ok {
		t.Fatal("C 不得进 allow set")
	}
	if _, ok := st.Rejected["C"]; !ok {
		t.Fatal("C 应被记为 rejected")
	}
}

// 候选 slot 超时释放：SYN 后始终未建立 → 释放 provisional slot。
func TestProvisionalSlotTimeout(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 2, testNow, hour, ttl, 5*time.Second)
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, map[string]IPActivity{"B": cand("B")}, 2, testNow.Add(time.Second), hour, ttl, 5*time.Second)
	if st.grantedCount() != 2 {
		t.Fatalf("B 候选应获 slot, got %d", st.grantedCount())
	}
	// B 一直不建立，超过 5s 后释放；此时 A 仍在线，B 候选再次出现但已被释放
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, map[string]IPActivity{"B": cand("B")}, 2, testNow.Add(7*time.Second), hour, ttl, 5*time.Second)
	if _, ok := st.Slots["B"]; ok {
		t.Fatal("B 超时未建立应释放 provisional slot")
	}
}

// 并发候选不超卖：max=2，已有 A，同时 B/C/D/E 候选，granted 永远 <= 2。
func TestConcurrentCandidatesNeverOversell(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 2, testNow, hour, ttl, prov)
	for i := 0; i < 20; i++ {
		st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)},
			map[string]IPActivity{"B": cand("B"), "C": cand("C"), "D": cand("D"), "E": cand("E")},
			2, testNow.Add(time.Duration(i+1)*time.Second), hour, ttl, prov)
		if st.grantedCount() > 2 {
			t.Fatalf("并发候选超卖: 第 %d 轮 granted=%d", i, st.grantedCount())
		}
	}
}

// Rejected 表 TTL 清理：防端口扫描无限增长。
func TestRejectedGC(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 1, testNow, hour, 30*time.Second, prov)
	for i := 0; i < 100; i++ {
		st.Rejected["ip-"+string(rune('a'+i))] = testNow
	}
	st.Reconcile(map[string]IPActivity{}, nil, 1, testNow.Add(31*time.Second), hour, 30*time.Second, prov)
	if len(st.Rejected) != 0 {
		t.Fatalf("Rejected 超 TTL 应被清理, got %d", len(st.Rejected))
	}
}

// IPv6 测试：v6 地址正常授予。
func TestIPv6Slot(t *testing.T) {
	st := newIPState()
	allow, _ := st.Reconcile(map[string]IPActivity{"2001:db8::1": act("2001:db8::1", 1, 0, true)}, nil,
		1, testNow, hour, ttl, prov)
	if len(allow) != 1 || st.grantedCount() != 1 {
		t.Fatalf("IPv6 应授予 1 个, got allow=%d", len(allow))
	}
}

// Observed GC：无 slot、无拒绝、久未活跃的观察项清理。
func TestObservedGC(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, nil, 0, testNow, 10*time.Second, ttl, prov)
	if _, ok := st.Observed["A"]; !ok {
		t.Fatal("A 应被 observed")
	}
	st.Reconcile(map[string]IPActivity{}, nil, 0, testNow.Add(20*time.Second), 10*time.Second, ttl, prov)
	if _, ok := st.Observed["A"]; ok {
		t.Fatal("A 超 idle 且不在线应被 GC")
	}
}
