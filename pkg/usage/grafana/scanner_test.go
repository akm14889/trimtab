package grafana

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestScanDashboardsAndManagedAlertsHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_, _ = w.Write([]byte(`[{"uid":"dash-1","title":"Payments Overview","folderTitle":"Payments","tags":["prod"]}]`))
		case "/api/dashboards/uid/dash-1":
			_, _ = w.Write([]byte(`{
				"meta": {"folderTitle": "Payments"},
				"dashboard": {
					"uid": "dash-1",
					"title": "Payments Overview",
					"tags": ["prod"],
					"templating": {
						"list": [
							{"name": "region", "type": "query", "query": "label_values(payments_http_requests_total, region)"}
						]
					},
					"panels": [
						{
							"title": "Latency",
							"links": [{"title": "Inspect", "url": "/explore?host=${__field.labels.host}"}],
							"targets": [
								{
									"expr": "sum by (service) (rate(payments_http_requests_total{env=\"$env\"}[$__rate_interval]))",
									"legendFormat": "{{service}} - {{status_code}}"
								}
							],
							"transformations": [
								{"id": "organize", "options": {"renameByName": {"zone": "AZ"}}}
							]
						}
					]
				}
			}`))
		case "/api/v1/provisioning/alert-rules":
			_, _ = w.Write([]byte(`[
				{
					"uid": "alert-1",
					"title": "Checkout SLO Burn",
					"ruleGroup": "slo",
					"annotations": {"summary": "Burn on {{ $labels.cluster }}"},
					"data": [
						{"refId": "A", "model": {"expr": "sum by (service) (rate(checkout_errors_total[5m]))"}}
					]
				}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	res, err := Scan(context.Background(), &config.GrafanaConfig{URL: srv.URL, APIKey: "secret"})
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if res.DashboardCount != 1 || res.AlertRuleCount != 1 {
		t.Fatalf("counts: dashboards=%d alerts=%d", res.DashboardCount, res.AlertRuleCount)
	}

	// Verify panel ref extracted legendFormat, link label, transform label, and PromQL labels
	var panelRef *metrics.UsageRef
	var alertRef *metrics.UsageRef
	var varRef *metrics.UsageRef
	for i := range res.Refs {
		r := &res.Refs[i]
		if r.Origin == metrics.OriginGrafanaAlert {
			alertRef = r
		} else if r.Query == "label_values(payments_http_requests_total, region)" {
			varRef = r
		} else {
			panelRef = r
		}
	}
	if panelRef == nil {
		t.Fatal("missing panelRef")
	}
	wantPanelLabels := []string{"env", "host", "service", "status_code", "zone"}
	if !reflect.DeepEqual(panelRef.ExplicitLabels, wantPanelLabels) {
		t.Errorf("panelRef ExplicitLabels = %v, want %v", panelRef.ExplicitLabels, wantPanelLabels)
	}
	if varRef == nil || !reflect.DeepEqual(varRef.ExplicitLabels, []string{"region"}) {
		t.Errorf("varRef = %+v", varRef)
	}
	if alertRef == nil || alertRef.MetricName != "checkout_errors_total" {
		t.Fatalf("missing or wrong alertRef: %+v", alertRef)
	}
	if !reflect.DeepEqual(alertRef.ExplicitLabels, []string{"cluster", "service"}) {
		t.Errorf("alertRef ExplicitLabels = %v", alertRef.ExplicitLabels)
	}
}

func TestScanLocalDir(t *testing.T) {
	dir := t.TempDir()
	dash := `{"uid":"ce","title":"Cardinality Explorer","panels":[{"title":"All","targets":[{"expr":"count({__name__=~\".+\"})"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "explorer.json"), []byte(dash), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(context.Background(), &config.GrafanaConfig{DashboardsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.DashboardCount != 1 || len(res.Refs) != 1 || !res.Refs[0].IsRegex {
		t.Fatalf("unexpected local scan result: %+v", res)
	}
}
