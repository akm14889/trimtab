package metrics

import (
	"reflect"
	"testing"
)

func TestLabelUsageSemantics(t *testing.T) {
	allLabels := []string{"__name__", "le", "quantile", "service", "status", "pod", "pod_uid", "instance"}

	// Case 1: bare rate(m[5m]) -> preserves all labels
	u1 := NewLabelUsage()
	u1.MarkPreservesAll()
	if got := u1.UnusedLabels(allLabels); len(got) != 0 {
		t.Fatalf("PreservesAll should have 0 unused labels, got %v", got)
	}

	// Case 2: sum without (pod, pod_uid, instance) -> service & status used; pod/pod_uid/instance unused; le/quantile protected
	u2 := NewLabelUsage()
	u2.MarkWithout([]string{"pod", "pod_uid", "instance"})
	got := u2.UnusedLabels(allLabels)
	want := []string{"instance", "pod", "pod_uid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnusedLabels = %v, want %v", got, want)
	}

	// Case 3: union of two different without clauses marks labels unused only if excluded by ALL
	u3 := NewLabelUsage()
	u3.MarkWithout([]string{"pod", "pod_uid"})
	u3.MarkWithout([]string{"pod_uid", "instance"})
	got3 := u3.UnusedLabels(allLabels)
	want3 := []string{"pod_uid"}
	if !reflect.DeepEqual(got3, want3) {
		t.Fatalf("Union of two without clauses: got %v, want %v", got3, want3)
	}

	// Case 4: sum by (service) + explicit legendFormat {{status}}
	u4 := NewLabelUsage()
	u4.MarkExplicit("service")
	u4.MarkExplicit("status")
	got4 := u4.UnusedLabels(allLabels)
	want4 := []string{"instance", "pod", "pod_uid"}
	if !reflect.DeepEqual(got4, want4) {
		t.Fatalf("Explicit labels: got %v, want %v", got4, want4)
	}
}
