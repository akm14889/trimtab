package promrules

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanGlobsAlertsAndRecordingRules(t *testing.T) {
	dir := t.TempDir()
	ruleYAML := `
groups:
  - name: checkout_alerts
    rules:
      - alert: HighErrorRate
        expr: sum by (service) (rate(http_requests_total{status=~"5.."}[5m])) > 10
        labels:
          severity: critical
          pod_name: "{{ $labels.pod }}"
        annotations:
          summary: "High errors on {{ $labels.instance }} in {{ index $labels \"cluster\" }}"
  - name: orders_legacy
    rules:
      - record: orders:deprecated_latency:rate5m
        expr: sum by (le, service) (rate(orders_deprecated_latency_seconds_bucket[5m])) or vector(0)
      - record: orders:deprecated_latency:p99
        expr: histogram_quantile(0.99, orders:deprecated_latency:rate5m)
`
	path := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(path, []byte(ruleYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ScanGlobs([]string{filepath.Join(dir, "*.yaml")})
	if err != nil {
		t.Fatalf("ScanGlobs error: %v", err)
	}
	if res.AlertRuleCount != 1 || res.RecordingRuleCount != 2 || res.TotalRuleCount != 3 {
		t.Fatalf("unexpected counts: %+v", res)
	}
	if len(res.AlertRefs) != 1 {
		t.Fatalf("expected 1 AlertRef, got %d", len(res.AlertRefs))
	}
	wantLabels := []string{"cluster", "instance", "pod", "service", "status"}
	if !reflect.DeepEqual(res.AlertRefs[0].ExplicitLabels, wantLabels) {
		t.Errorf("AlertRef ExplicitLabels = %v, want %v", res.AlertRefs[0].ExplicitLabels, wantLabels)
	}
	if !res.RecordingRules[0].HasOrVectorZero {
		t.Errorf("expected HasOrVectorZero = true on rule %s", res.RecordingRules[0].Name)
	}
	if res.RecordingRules[1].HasOrVectorZero {
		t.Errorf("expected HasOrVectorZero = false on rule %s", res.RecordingRules[1].Name)
	}
}
