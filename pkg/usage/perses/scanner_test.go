package perses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestScanPersesLocalDirAndHTTP(t *testing.T) {
	dir := t.TempDir()

	// YAML Dashboard + non-Dashboard resource + JSON Dashboard
	const dashYAML = `
kind: Project
metadata:
  name: payments
---
kind: Dashboard
metadata:
  name: service-overview
  project: payments
spec:
  display:
    name: "Payments Overview"
  variables:
    - kind: ListVariable
      spec:
        name: env
        plugin:
          kind: PrometheusLabelValuesVariable
          spec:
            labelName: env
            matchers:
              - 'perses_var_metric{team="pay"}'
    - kind: ListVariable
      spec:
        name: service
        plugin:
          kind: PrometheusPromQLVariable
          spec:
            expr: 'label_values(perses_promql_var_metric, service)'
            labelName: region
  panels:
    latency:
      kind: Panel
      spec:
        display:
          name: "Latency p99"
        queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: 'histogram_quantile(0.99, sum by (le) (rate(perses_panel_duration_seconds_bucket{cluster="$cluster"}[$__rate_interval])))'
                  seriesNameFormat: "{{pod}} - {{instance}}"
          - kind: TraceQuery
            spec:
              plugin:
                kind: TempoTraceQuery
                spec:
                  query: '{span.http.status_code >= 500}'
`
	if err := os.WriteFile(filepath.Join(dir, "payments.yaml"), []byte(dashYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	const dashJSON = `{
  "kind": "Dashboard",
  "metadata": { "name": "wildcard-explorer", "project": "ops" },
  "spec": {
    "display": { "name": "Ops Wildcard" },
    "panels": {
      "all": {
        "kind": "Panel",
        "spec": {
          "display": { "name": "Explorer" },
          "queries": [
            {
              "kind": "TimeSeriesQuery",
              "spec": {
                "plugin": {
                  "kind": "PrometheusTimeSeriesQuery",
                  "spec": { "query": "count({__name__=~\".+\"})" }
                }
              }
            }
          ]
        }
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "explorer.json"), []byte(dashJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	localRes, err := Scan(context.Background(), &config.PersesConfig{
		DashboardsDir: dir,
	})
	if err != nil {
		t.Fatalf("Scan(dir) failed: %v", err)
	}
	if localRes.DashboardCount != 2 {
		t.Fatalf("DashboardCount = %d, want 2 (ignoring kind: Project)", localRes.DashboardCount)
	}

	foundPanel := false
	foundVar := false
	foundWild := false
	for _, ref := range localRes.Refs {
		if ref.Origin != metrics.OriginPersesDashboard || !ref.Origin.IsLiveSink() {
			t.Fatalf("unexpected origin %q", ref.Origin)
		}
		if ref.MetricName == "perses_panel_duration_seconds_bucket" {
			foundPanel = true
			for _, want := range []string{"cluster", "instance", "le", "pod"} {
				if !slices.Contains(ref.ExplicitLabels, want) {
					t.Errorf("panel ref missing label %q in %v", want, ref.ExplicitLabels)
				}
			}
		}
		if ref.MetricName == "perses_var_metric" {
			foundVar = true
			if !slices.Contains(ref.ExplicitLabels, "env") || !slices.Contains(ref.ExplicitLabels, "team") {
				t.Errorf("var ref missing env/team in %v", ref.ExplicitLabels)
			}
		}
		if ref.IsRegex && ref.MetricRegex == ".+" && ref.DashboardUID == "ops/wildcard-explorer" {
			foundWild = true
		}
	}
	if !foundPanel || !foundVar || !foundWild {
		t.Fatalf("missing expected refs: panel=%v var=%v wild=%v refs=%+v", foundPanel, foundVar, foundWild, localRes.Refs)
	}

	// REST API test
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer perses-tok" {
			http.Error(w, "denied perses-tok", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = w.Write([]byte(`[{"metadata":{"name":"checkout"}}]`))
		case "/api/v1/projects/checkout/dashboards":
			_, _ = w.Write([]byte(`[{
  "kind": "Dashboard",
  "metadata": { "name": "checkout-http", "project": "checkout" },
  "spec": {
    "display": { "name": "Checkout HTTP" },
    "panels": {
      "rps": {
        "spec": {
          "display": { "name": "RPS" },
          "queries": [{
            "spec": { "plugin": { "kind": "PrometheusTimeSeriesQuery", "spec": { "query": "sum by (status) (rate(perses_api_http_total[5m]))" } } }
          }]
        }
      }
    }
  }
}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	httpRes, err := Scan(context.Background(), &config.PersesConfig{
		URL:    srv.URL,
		APIKey: "perses-tok",
	})
	if err != nil {
		t.Fatalf("Scan(HTTP) failed: %v", err)
	}
	if httpRes.DashboardCount != 1 || len(httpRes.Refs) != 1 || httpRes.Refs[0].MetricName != "perses_api_http_total" {
		t.Fatalf("unexpected HTTP result: %+v", httpRes)
	}

	// Verify 403 fails closed and redacts token
	_, err = Scan(context.Background(), &config.PersesConfig{
		URL:    srv.URL,
		APIKey: "wrong-perses-secret",
	})
	if err == nil || strings.Contains(err.Error(), "wrong-perses-secret") {
		t.Fatalf("expected fail-closed redacted error on 403, got %v", err)
	}
}
