package promql

import (
	"reflect"
	"testing"
)

func TestExtractLabelSemantics(t *testing.T) {
	t.Run("bare rate preserves all labels", func(t *testing.T) {
		res, err := Extract(`rate(http_requests_total[5m])`)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Accesses) != 1 {
			t.Fatalf("got %d accesses, want 1", len(res.Accesses))
		}
		a := res.Accesses[0]
		if a.MetricName != "http_requests_total" {
			t.Errorf("MetricName = %q", a.MetricName)
		}
		if !a.PreservesAll {
			t.Errorf("expected PreservesAll = true for bare rate()")
		}
	})

	t.Run("sum without preserves all except excluded", func(t *testing.T) {
		res, err := Extract(`sum without (pod, pod_uid) (rate(http_requests_total[5m]))`)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Accesses[0]
		if a.PreservesAll {
			t.Errorf("PreservesAll should be false for sum without")
		}
		wantWithout := []string{"pod", "pod_uid"}
		if !reflect.DeepEqual(a.WithoutLabels, wantWithout) {
			t.Errorf("WithoutLabels = %v, want %v", a.WithoutLabels, wantWithout)
		}
	})

	t.Run("sum by extracts explicit labels and selector matchers", func(t *testing.T) {
		res, err := Extract(`histogram_quantile(0.95, sum by (le, service) (rate(payments_http_duration_seconds_bucket{env="prod"}[5m])))`)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Accesses[0]
		if a.PreservesAll {
			t.Errorf("PreservesAll should be false")
		}
		wantExplicit := []string{"env", "le", "service"}
		if !reflect.DeepEqual(a.ExplicitLabels, wantExplicit) {
			t.Errorf("ExplicitLabels = %v, want %v", a.ExplicitLabels, wantExplicit)
		}
	})

	t.Run("label_replace and label_join string arguments", func(t *testing.T) {
		res, err := Extract(`sum by (cluster) (label_replace(label_join(my_metric, "joined", "-", "src_a", "src_b"), "dst_lbl", "$1", "src_c", "(.*)"))`)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Accesses[0]
		want := []string{"cluster", "dst_lbl", "joined", "src_a", "src_b", "src_c"}
		if !reflect.DeepEqual(a.ExplicitLabels, want) {
			t.Errorf("ExplicitLabels = %v, want %v", a.ExplicitLabels, want)
		}
	})

	t.Run("vector matching on and group_left", func(t *testing.T) {
		res, err := Extract(`sum by (service) (req_total) / on (service) group_left(team, owner) service_info`)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Accesses) != 2 {
			t.Fatalf("got %d accesses, want 2", len(res.Accesses))
		}
		byMetric := map[string]MetricAccess{}
		for _, a := range res.Accesses {
			byMetric[a.MetricName] = a
		}
		if !reflect.DeepEqual(byMetric["req_total"].ExplicitLabels, []string{"service"}) {
			t.Errorf("req_total labels = %v", byMetric["req_total"].ExplicitLabels)
		}
		if !reflect.DeepEqual(byMetric["service_info"].ExplicitLabels, []string{"owner", "service", "team"}) {
			t.Errorf("service_info labels = %v", byMetric["service_info"].ExplicitLabels)
		}
	})

	t.Run("regex metric selector", func(t *testing.T) {
		res, err := Extract(`count({__name__=~".+"})`)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Accesses[0]
		if !a.IsRegex || a.MetricRegex != ".+" {
			t.Errorf("expected regex selector .+, got %+v", a)
		}
	})
}
