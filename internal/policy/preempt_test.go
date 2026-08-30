package policy

import (
	"testing"
	"time"
)

// 只发 SYN 的陌生 IP 会先拿到 provisional slot。若「持有 slot」优先级最高，
// 它就能在名额满时把真正 ESTABLISHED 的客户端推进 Rejected；每
// provisionalTTL 内重发一次 SYN 即可持续拒服（max_ips=1 时最明显）。
func TestActiveClientPreemptsSynOnlySquatter(t *testing.T) {
	st := newIPState()
	base := time.Now()
	idle, rejTTL, provTTL := 60*time.Second, 60*time.Second, 10*time.Second

	// 第 1 轮：只有攻击者的 SYN
	allow, _ := st.Reconcile(
		map[string]IPActivity{},
		map[string]IPActivity{"198.51.100.66": {IP: "198.51.100.66", TCPSessions: 1}},
		1, base, idle, rejTTL, provTTL)
	if !allow["198.51.100.66"] {
		t.Fatal("候选应获 provisional slot 以完成握手")
	}
	if st.activeGrantedCount() != 0 {
		t.Fatal("provisional 不算在线")
	}

	// 第 2 轮：真实客户端已 ESTABLISHED；攻击者仍只发 SYN
	now := base.Add(2 * time.Second)
	allow, _ = st.Reconcile(
		map[string]IPActivity{"203.0.113.20": {IP: "203.0.113.20", TCPSessions: 1, Traffic: true}},
		map[string]IPActivity{"198.51.100.66": {IP: "198.51.100.66", TCPSessions: 1}},
		1, now, idle, rejTTL, provTTL)

	if _, rejected := st.Rejected["203.0.113.20"]; rejected {
		t.Errorf("真实客户端被仅发 SYN 的陌生 IP 挤出名额; slots=%v rejected=%v",
			slotIPs(st), rejectedIPs(st))
	}
	if !allow["203.0.113.20"] {
		t.Errorf("真实客户端应在 allow set 内, got %v", allow)
	}
	if st.activeGrantedCount() != 1 {
		t.Errorf("在线 IP 应为 1, got %d", st.activeGrantedCount())
	}
	if _, ok := st.Slots["198.51.100.66"]; ok {
		t.Errorf("名额满时 provisional 应让位给真实客户端, slots=%v", slotIPs(st))
	}
}

// 已经在用（非 provisional）的客户端绝不能被新来的 active IP 挤掉。
func TestEstablishedSlotNotPreemptedByNewActive(t *testing.T) {
	st := newIPState()
	base := time.Now()
	idle, rejTTL, provTTL := 60*time.Second, 60*time.Second, 10*time.Second

	st.Reconcile(map[string]IPActivity{"203.0.113.1": {IP: "203.0.113.1", TCPSessions: 1, Traffic: true}},
		nil, 1, base, idle, rejTTL, provTTL)
	if st.activeGrantedCount() != 1 {
		t.Fatal("首个客户端应在线")
	}

	now := base.Add(time.Second)
	allow, hasRej := st.Reconcile(map[string]IPActivity{
		"203.0.113.1": {IP: "203.0.113.1", TCPSessions: 1, Traffic: true},
		"203.0.113.2": {IP: "203.0.113.2", TCPSessions: 1, Traffic: true},
	}, nil, 1, now, idle, rejTTL, provTTL)

	if !allow["203.0.113.1"] {
		t.Error("在用客户端必须保留名额")
	}
	if allow["203.0.113.2"] {
		t.Error("超限 IP 不得进入 allow set")
	}
	if !hasRej {
		t.Error("应报告有被拒绝的 IP")
	}
}

func slotIPs(st *NodeIPState) []string {
	out := []string{}
	for ip := range st.Slots {
		out = append(out, ip)
	}
	return out
}

func rejectedIPs(st *NodeIPState) []string {
	out := []string{}
	for ip := range st.Rejected {
		out = append(out, ip)
	}
	return out
}
