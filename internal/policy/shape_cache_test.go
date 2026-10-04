package policy

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

func TestNodesShapeCacheAndActivityIndexInvalidateIndependently(t *testing.T) {
	s := &Service{}
	first := []nodes.Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("443")}}
	second := []nodes.Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("8443")}}

	if got := s.activityPortIndex(first); got[443] != "1" {
		t.Fatalf("initial activity index incorrect: %#v", got)
	}
	shape1 := s.cachedNodesShape(first)
	if shape1 != nodesShape(first) {
		t.Fatalf("initial shape mismatch: got %q want %q", shape1, nodesShape(first))
	}

	// Shape cache 先看到替换后的 slice 时，不能误更新 activityPortIndex 的
	// 身份标记；反过来也一样。两个 memo 独立失效。
	shape2 := s.cachedNodesShape(second)
	if shape2 == shape1 || shape2 != nodesShape(second) {
		t.Fatalf("shape cache did not invalidate: old=%q new=%q", shape1, shape2)
	}
	idx2 := s.activityPortIndex(second)
	if idx2[443] != "" || idx2[8443] != "1" {
		t.Fatalf("activity index remained stale after shape refresh: %#v", idx2)
	}

	// 再反向顺序更新：端口索引刷新后，形态缓存也必须独立观察到替换。
	third := []nodes.Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("9443")}}
	_ = s.activityPortIndex(third)
	shape3 := s.cachedNodesShape(third)
	if shape3 == shape2 || shape3 != nodesShape(third) {
		t.Fatalf("shape cache stale after activity refresh: %q", shape3)
	}
}

var benchmarkShapeSink string

func BenchmarkNodesShapeRecompute(b *testing.B) {
	list := make([]nodes.Node, 50)
	for i := range list {
		id := strconv.Itoa(i + 1)
		port := strconv.Itoa(10000 + i)
		list[i] = nodes.Node{"id": json.Number(id), "type": "vless", "port": json.Number(port)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkShapeSink = nodesShape(list)
	}
}

func BenchmarkNodesShapeCached(b *testing.B) {
	list := make([]nodes.Node, 50)
	for i := range list {
		id := strconv.Itoa(i + 1)
		port := strconv.Itoa(10000 + i)
		list[i] = nodes.Node{"id": json.Number(id), "type": "vless", "port": json.Number(port)}
	}
	s := &Service{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkShapeSink = s.cachedNodesShape(list)
	}
}
