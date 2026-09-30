package policy

import (
	"reflect"
	"testing"
	"time"
)

func TestBuildActiveIPsPreservesLastActiveOrder(t *testing.T) {
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	st := newIPState()
	st.Slots["203.0.113.3"] = &IPSlot{IP: "203.0.113.3", LastSeen: base}
	st.Slots["203.0.113.1"] = &IPSlot{IP: "203.0.113.1", LastSeen: base.Add(-time.Second)}
	st.Slots["203.0.113.2"] = &IPSlot{IP: "203.0.113.2", LastSeen: base, Provisional: true}
	got := buildActiveIPsFromState(st)
	want := []string{"203.0.113.3", "203.0.113.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("last-active desc/IP tie order: got=%v want=%v", got, want)
	}
}

func TestBuildActiveIPsFastPathSortedByIP(t *testing.T) {
	now := time.Now()
	st := newIPState()
	for _, ip := range []string{"203.0.113.3", "203.0.113.1", "203.0.113.2"} {
		st.Slots[ip] = &IPSlot{IP: ip, LastSeen: now}
	}
	got := buildActiveIPsFromState(st)
	want := []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("same timestamp tie order: got=%v want=%v", got, want)
	}
}
