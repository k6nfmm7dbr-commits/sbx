package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

var benchmarkRatePortSink map[int64]int

func benchmarkRateScenario(b *testing.B) (*Service, []nodes.Node, map[string]int) {
	b.Helper()
	list := make([]nodes.Node, 50)
	for i := range list {
		list[i] = nodes.Node{
			"id": json.Number(fmt.Sprint(i + 1)), "type": "vless",
			"port": json.Number(fmt.Sprint(10000 + i)),
		}
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Service{
		appDir: b.TempDir(), policyConf: filepath.Join(b.TempDir(), "policy.nft"),
		now:           func() time.Time { return base },
		appliedPaused: map[string]bool{}, appliedIPLimit: map[string]map[string]bool{},
		appliedRate: map[string]int{"25": 100}, tableProbe: func() bool { return true },
		lastProbeAt: base, lastProbeOK: true, nftApply: func(context.Context, string) error { return nil },
	}
	s.appliedShape = nodesShape(list)
	s.cachedNodesShape(list) // 预热节点摘要缓存，基准测稳态 no-op 路径
	rate := map[string]int{"25": 100}
	return s, list, rate
}

func BenchmarkApplyEnforcementNoopRateSet(b *testing.B) {
	s, list, rate := benchmarkRateScenario(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.applyEnforcement(ctx, nil, nil, rate, list); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLegacyRatePortExpansion captures work the old no-op path performed
// before discovering that applied state was unchanged.
func BenchmarkLegacyRatePortExpansion(b *testing.B) {
	_, list, rate := benchmarkRateScenario(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ratePorts := map[int64]int{}
		for id, mbps := range rate {
			for _, n := range list {
				if nodes.IDString(n) == id {
					for _, r := range nodes.ParsePorts(n) {
						for p := r[0]; p <= r[1]; p++ {
							ratePorts[p] = mbps
						}
					}
				}
			}
		}
		benchmarkRatePortSink = ratePorts
	}
}
