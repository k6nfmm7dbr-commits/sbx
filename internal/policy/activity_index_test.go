package policy

import (
	"reflect"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

func TestActivityPortIndexInvalidatesWhenNodesSliceReplaced(t *testing.T) {
	s := &Service{}
	first := []nodes.Node{{"id": 1, "type": "vless", "port": 443}}
	idx1 := s.activityPortIndex(first)
	if idx1[443] != "1" || idx1[8443] != "" {
		t.Fatalf("初始端口索引错误: %#v", idx1)
	}
	// 同一个不可变 slice 应复用同一索引 map。
	if reflect.ValueOf(s.activityPortIndex(first)).Pointer() != reflect.ValueOf(idx1).Pointer() {
		t.Fatal("节点未变时端口索引未复用")
	}
	second := []nodes.Node{{"id": 1, "type": "vless", "port": 8443}}
	idx2 := s.activityPortIndex(second)
	if idx2[443] != "" || idx2[8443] != "1" {
		t.Fatalf("节点替换后端口索引陈旧: %#v", idx2)
	}
}
