package policy

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkSlotReconcileStable(b *testing.B) {
	for _, count := range []int{5, 50, 250} {
		b.Run(fmt.Sprintf("ips=%d", count), func(b *testing.B) {
			st := newIPState()
			active := make(map[string]IPActivity, count)
			for i := 0; i < count; i++ {
				ip := fmt.Sprintf("203.0.113.%d", i%254+1)
				active[ip] = IPActivity{IP: ip, TCPSessions: 1, Traffic: true}
			}
			now := time.Now()
			st.Reconcile(active, nil, count+1, now, time.Minute, time.Minute, 10*time.Second)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				now = now.Add(time.Second)
				st.Reconcile(active, nil, count+1, now, time.Minute, time.Minute, 10*time.Second)
			}
		})
	}
}

func BenchmarkSlotReconcileAdmission(b *testing.B) {
	for _, count := range []int{5, 50, 250} {
		b.Run(fmt.Sprintf("ips=%d", count), func(b *testing.B) {
			st := newIPState()
			active := make(map[string]IPActivity, count)
			for i := 0; i < count; i++ {
				ip := fmt.Sprintf("203.0.113.%d", i%254+1)
				active[ip] = IPActivity{IP: ip, TCPSessions: 1, Traffic: true}
			}
			candidates := map[string]IPActivity{"198.51.100.1": {IP: "198.51.100.1", TCPSessions: 1}}
			now := time.Now()
			st.Reconcile(active, candidates, count+1, now, time.Minute, time.Minute, 10*time.Second)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				now = now.Add(time.Second)
				st.Reconcile(active, candidates, count+1, now, time.Minute, time.Minute, 10*time.Second)
			}
		})
	}
}

func BenchmarkSlotReconcileNoAllowSet(b *testing.B) {
	for _, count := range []int{50, 250} {
		b.Run(fmt.Sprintf("ips=%d", count), func(b *testing.B) {
			st := newIPState()
			active := make(map[string]IPActivity, count)
			for i := 0; i < count; i++ {
				ip := fmt.Sprintf("203.0.113.%d", i%254+1)
				active[ip] = IPActivity{IP: ip, TCPSessions: 1, Traffic: true}
			}
			now := time.Now()
			st.reconcile(active, nil, 0, now, time.Minute, time.Minute, 10*time.Second, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				now = now.Add(time.Second)
				st.reconcile(active, nil, 0, now, time.Minute, time.Minute, 10*time.Second, false)
			}
		})
	}
}
