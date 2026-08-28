package policy

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func act(ip string, tcp, udp int, traffic bool) IPActivity {
	return IPActivity{IP: ip, TCPSessions: tcp, UDPSessions: udp, Traffic: traffic}
}

// 满 Slot / 超卖测试：max=2，A 已在线，同轮出现 B/C/D，只能授予 A、B，C/D 拒绝。
func TestSlotAdmissionNoOversell(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, 2, testNow, time.Hour, time.Hour)

	granted, hasRejected := st.Reconcile(map[string]IPActivity{
		"A": act("A", 1, 0, true),
		"B": act("B", 1, 0, true),
		"C": act("C", 1, 0, true),
		"D": act("D", 1, 0, true),
	}, 2, testNow.Add(time.Second), time.Hour, time.Hour)

	if !hasRejected {
		t.Fatal("应有新拒绝")
	}
	if _, ok := granted["A"]; !ok {
		t.Fatal("A 应保持 granted")
	}
	if _, ok := granted["B"]; !ok {
		t.Fatal("B 应被 granted")
	}
	if _, ok := granted["C"]; ok {
		t.Fatal("C 不应被 granted")
	}
	if _, ok := granted["D"]; ok {
		t.Fatal("D 不应被 granted")
	}
	if st.grantedCount() != 2 {
		t.Fatalf("granted 应为 2, got %d", st.grantedCount())
	}
	if _, ok := st.Rejected["C"]; !ok {
		t.Fatal("C 应被记为 rejected")
	}
	if _, ok := st.Rejected["D"]; !ok {
		t.Fatal("D 应被记为 rejected")
	}
}

// 满 Slot 测试：满额后新 IP 不得进入 granted。
func TestFullSlotRejects(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)},
		2, testNow, time.Hour, time.Hour)
	granted, _ := st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "C": act("C", 1, 0, true)},
		2, testNow.Add(time.Second), time.Hour, time.Hour)
	if _, ok := granted["C"]; ok {
		t.Fatal("满额后 C 不得进入 granted")
	}
}

// Slot 释放测试：B 离线但仍在 grace 内，C 不得补位；超过 idle 后 B 释放，C 可授予。
func TestSlotReleaseAfterIdle(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true), "B": act("B", 1, 0, true)},
		2, testNow, 30*time.Second, time.Hour)

	// B 消失，但还在 grace（未超 idle）
	granted, _ := st.Reconcile(map[string]IPActivity{
		"A": act("A", 1, 0, true),
		"C": act("C", 1, 0, true),
	}, 2, testNow.Add(10*time.Second), 30*time.Second, time.Hour)
	if _, ok := granted["B"]; !ok {
		t.Fatal("B 在 grace 内不应被释放")
	}
	if _, ok := granted["C"]; ok {
		t.Fatal("grace 内 C 不得补位")
	}

	// 超过 idle → B 释放，C 补位
	granted, _ = st.Reconcile(map[string]IPActivity{
		"A": act("A", 1, 0, true),
		"C": act("C", 1, 0, true),
	}, 2, testNow.Add(41*time.Second), 30*time.Second, time.Hour)
	if _, ok := granted["B"]; ok {
		t.Fatal("超 idle 后 B 应被释放")
	}
	if _, ok := granted["C"]; !ok {
		t.Fatal("B 释放后 C 应能补位")
	}
}

// NAT 测试：同一 IP 多个 TCP 流，online_ip 仍算 1。
func TestNATSingleIP(t *testing.T) {
	st := newIPState()
	granted, _ := st.Reconcile(map[string]IPActivity{"223.1.1.1": act("223.1.1.1", 10, 3, true)},
		5, testNow, time.Hour, time.Hour)
	if st.grantedCount() != 1 {
		t.Fatalf("同一公网 IP 应算 1, got %d", st.grantedCount())
	}
	if len(granted) != 1 {
		t.Fatalf("granted 应为 1, got %d", len(granted))
	}
}

// TCP+UDP 测试：同一 IP 同时有 TCP 与 UDP，只算 1 个 IP。
func TestTCPUDPSingleIP(t *testing.T) {
	st := newIPState()
	granted, _ := st.Reconcile(map[string]IPActivity{"9.9.9.9": act("9.9.9.9", 2, 4, true)},
		0, testNow, time.Hour, time.Hour)
	if st.grantedCount() != 1 || len(granted) != 1 {
		t.Fatalf("TCP+UDP 同一 IP 应算 1, got count=%d granted=%d", st.grantedCount(), len(granted))
	}
	slot := st.Slots["9.9.9.9"]
	if slot == nil || !slot.TCP || !slot.UDP {
		t.Fatalf("slot 应带 TCP/UDP 标记: %+v", slot)
	}
}

// 不限制（maxIPs<=0）时，所有活跃 IP 授予。
func TestUnlimitedGrantsAll(t *testing.T) {
	st := newIPState()
	granted, hasRejected := st.Reconcile(map[string]IPActivity{
		"A": act("A", 1, 0, true), "B": act("B", 1, 0, true), "C": act("C", 1, 0, true),
	}, 0, testNow, time.Hour, time.Hour)
	if hasRejected || len(granted) != 3 {
		t.Fatalf("不限制应全授予且无拒绝, got granted=%d rejected=%v", len(granted), hasRejected)
	}
}

// Rejected 表 TTL 清理：防端口扫描无限增长。
func TestRejectedGC(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, 1, testNow, time.Hour, 30*time.Second)
	// 大量拒绝
	for i := 0; i < 100; i++ {
		st.Rejected["ip-"+string(rune('a'+i))] = testNow
	}
	// 超过 TTL 后（无新活跃 IP 触发 admission）再 reconcile
	st.Reconcile(map[string]IPActivity{}, 1, testNow.Add(31*time.Second), time.Hour, 30*time.Second)
	if len(st.Rejected) != 0 {
		t.Fatalf("Rejected 超 TTL 应被清理, got %d", len(st.Rejected))
	}
}

// Observed 表 GC：无 slot、无拒绝、久未活跃的观察项应被清理。
func TestObservedGC(t *testing.T) {
	st := newIPState()
	st.Reconcile(map[string]IPActivity{"A": act("A", 1, 0, true)}, 0, testNow, 10*time.Second, time.Hour)
	if _, ok := st.Observed["A"]; !ok {
		t.Fatal("A 应被 observed")
	}
	// A 不再出现，超过 idle 后应被 GC
	st.Reconcile(map[string]IPActivity{}, 0, testNow.Add(20*time.Second), 10*time.Second, time.Hour)
	if _, ok := st.Observed["A"]; ok {
		t.Fatal("A 超 idle 且不在线应被 GC")
	}
}

// IPv6 测试：v6 地址正常授予，不被当成多个 IP。
func TestIPv6Slot(t *testing.T) {
	st := newIPState()
	granted, _ := st.Reconcile(map[string]IPActivity{"2001:db8::1": act("2001:db8::1", 1, 0, true)},
		1, testNow, time.Hour, time.Hour)
	if len(granted) != 1 {
		t.Fatalf("IPv6 应授予 1 个, got %d", len(granted))
	}
	for ip := range granted {
		if !isV6(ip) {
			t.Fatalf("授予的应是 IPv6, got %s", ip)
		}
	}
}

func isV6(ip string) bool {
	for i := 0; i < len(ip); i++ {
		if ip[i] == ':' {
			return true
		}
	}
	return false
}
