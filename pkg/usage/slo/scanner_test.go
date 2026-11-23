package slo

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestScanSlothPyrraAndOpenSLO(t *testing.T) {
	dir := t.TempDir()

	const slothCRD = `
apiVersion: sloth.slok.dev/v1
kind: PrometheusServiceLevel
metadata:
  name: payments-api
  namespace: prod
spec:
  service: payments-api
  slos:
    - name: requests-availability
      sli:
        events:
          errorQuery: sum(rate(sloth_http_requests_total{job="payments",code=~"5.."}[{{.window}}]))
          totalQuery: sum(rate(sloth_http_requests_total{job="payments"}[{{.window}}]))
    - name: raw-ratio
      sli:
        raw:
          errorRatioQuery: rate(sloth_raw_errors[{{.rawInterval}}]) / rate(sloth_raw_total[5m])
---
version: "prometheus/v1"
service: standalone-svc
slos:
  - name: latency-ok
    sli:
      events:
        errorQuery: sum(rate(sloth_standalone_failures_total[{{.window}}]))
        totalQuery: sum(rate(sloth_standalone_requests_total[{{.window}}]))
`
	if err := os.WriteFile(filepath.Join(dir, "sloth.yaml"), []byte(slothCRD), 0o600); err != nil {
		t.Fatal(err)
	}

	const pyrraYAML = `
apiVersion: pyrra.dev/v1alpha1
kind: ServiceLevelObjective
metadata:
  name: checkout-ratio
  namespace: prod
spec:
  indicator:
    ratio:
      errors:
        metric: pyrra_http_errors_total{job="checkout",status="500"}
      total:
        metric: pyrra_http_requests_total{job="checkout"}
      grouping:
        - service
        - region
---
apiVersion: pyrra.dev/v1alpha1
kind: ServiceLevelObjective
metadata:
  name: checkout-latency
  namespace: prod
spec:
  indicator:
    latency:
      success:
        metric: pyrra_duration_seconds_bucket{le="0.5",job="checkout"}
      total:
        metric: pyrra_duration_seconds_count{job="checkout"}
      grouping:
        - endpoint
---
apiVersion: pyrra.dev/v1alpha1
kind: ServiceLevelObjective
metadata:
  name: worker-up
  namespace: prod
spec:
  indicator:
    bool_gauge:
      metric: pyrra_worker_healthy{env="prod"}
      grouping:
        - cluster
`
	if err := os.WriteFile(filepath.Join(dir, "pyrra.yaml"), []byte(pyrraYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	const openSLOYAML = `
apiVersion: openslo/v1
kind: SLO
metadata:
  name: openslo-orders
spec:
  indicator:
    spec:
      ratioMetric:
        good:
          metricSource:
            type: Prometheus
            spec:
              query: sum(rate(openslo_orders_good_total{team="orders"}[5m]))
        total:
          metricSource:
            type: Prometheus
            spec:
              query: sum(rate(openslo_orders_all_total{team="orders"}[5m]))
`
	if err := os.WriteFile(filepath.Join(dir, "openslo.yaml"), []byte(openSLOYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(&config.SLOConfig{Globs: []string{dir}})
	if err != nil {
		t.Fatalf("Scan(dir) failed: %v", err)
	}
	if len(res.Warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// 3 Sloth SLOs + 3 Pyrra SLOs + 1 OpenSLO = 7
	if res.SLOCount != 7 {
		t.Fatalf("SLOCount = %d, want 7", res.SLOCount)
	}

	assertSLO := func(metricName string, wantPreservesAll bool, wantLabels ...string) {
		t.Helper()
		for _, ref := range res.Refs {
			if ref.MetricName != metricName {
				continue
			}
			if ref.Origin != metrics.OriginSLO || !ref.Origin.IsLiveSink() {
				t.Fatalf("unexpected origin %q on %s", ref.Origin, metricName)
			}
			if ref.PreservesAll != wantPreservesAll {
				t.Errorf("%s PreservesAll=%v, want %v", metricName, ref.PreservesAll, wantPreservesAll)
			}
			for _, lbl := range wantLabels {
				if !slices.Contains(ref.ExplicitLabels, lbl) {
					t.Errorf("%s missing expected label %q in %v", metricName, lbl, ref.ExplicitLabels)
				}
			}
			return
		}
		t.Errorf("missing expected SLO ref for %s", metricName)
	}

	assertSLO("sloth_http_requests_total", false, "job", "code")
	assertSLO("sloth_raw_errors", true)
	assertSLO("sloth_standalone_failures_total", false)
	assertSLO("pyrra_http_errors_total", false, "job", "status", "service", "region")
	assertSLO("pyrra_duration_seconds_bucket", false, "le", "job", "endpoint")
	assertSLO("pyrra_worker_healthy", false, "env", "cluster")
	assertSLO("openslo_orders_good_total", false, "team")

	// Missing glob fails closed
	if _, err := ScanGlobs([]string{filepath.Join(dir, "missing-*.yaml")}); err == nil {
		t.Fatal("expected missing slo glob to fail closed")
	}
}
