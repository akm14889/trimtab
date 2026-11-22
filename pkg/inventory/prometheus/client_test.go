package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/config"
)

func TestFixtureCollapseHonestWithoutSemantics(t *testing.T) {
	dir := t.TempDir()
	// 4 pods x 2 services = 8 series. Each pod has a 1:1 pod_uid and instance.
	// Dropping pod_uid alone leaves pod -> 0% collapse (8 -> 8).
	// Dropping pod, pod_uid, instance together collapses to 2 services -> 75% savings (8 -> 2).
	fixture := `{
		"metricNames": ["payments_http_duration_seconds_bucket"],
		"metadata": {"payments_http_duration_seconds": {"type": "histogram"}},
		"seriesCounts": {"payments_http_duration_seconds_bucket": 8},
		"sampleLabelSets": {
			"payments_http_duration_seconds_bucket": [
				{"service":"pay","le":"0.1","pod":"pay-1","pod_uid":"u1","instance":"10.0.0.1"},
				{"service":"pay","le":"0.1","pod":"pay-2","pod_uid":"u2","instance":"10.0.0.2"},
				{"service":"pay","le":"0.1","pod":"pay-3","pod_uid":"u3","instance":"10.0.0.3"},
				{"service":"pay","le":"0.1","pod":"pay-4","pod_uid":"u4","instance":"10.0.0.4"},
				{"service":"chk","le":"0.1","pod":"chk-1","pod_uid":"u5","instance":"10.0.0.5"},
				{"service":"chk","le":"0.1","pod":"chk-2","pod_uid":"u6","instance":"10.0.0.6"},
				{"service":"chk","le":"0.1","pod":"chk-3","pod_uid":"u7","instance":"10.0.0.7"},
				{"service":"chk","le":"0.1","pod":"chk-4","pod_uid":"u8","instance":"10.0.0.8"}
			]
		}
	}`
	p := filepath.Join(dir, "inv.json")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	_, eval, err := LoadFixture(p, config.InventoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	combined, singles, err := eval.EstimateCollapse(context.Background(), "payments_http_duration_seconds_bucket", []string{"pod", "pod_uid", "instance"}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if combined == nil || combined.CollapsedSeries != 2 || combined.SavingsPercent != 75.0 {
		t.Fatalf("unexpected combined collapse: %+v", combined)
	}
	var podUIDEst *int
	for _, s := range singles {
		if len(s.DroppedLabels) == 1 && s.DroppedLabels[0] == "pod_uid" {
			v := s.CollapsedSeries
			podUIDEst = &v
			if s.SavingsPercent != 0.0 {
				t.Errorf("pod_uid alone should show 0%% collapse when pod remains, got %.1f%%", s.SavingsPercent)
			}
		}
	}
	if podUIDEst == nil || *podUIDEst != 8 {
		t.Fatalf("expected pod_uid alone to stay at 8 series, got %v", podUIDEst)
	}
}

func TestFetchHTTPAndShortRetentionWarningOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/label/__name__/values":
			_, _ = w.Write([]byte(`{"status":"success","data":["up","http_requests_total","weekly_cron_job_runs"]}`))
		case "/api/v1/metadata":
			_, _ = w.Write([]byte(`{"status":"success","data":{"http_requests_total":[{"type":"counter","help":"reqs"}]}}`))
		case "/api/v1/status/tsdb":
			_, _ = w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[{"name":"http_requests_total","value":1200},{"name":"up","value":10}]}}`))
		case "/api/v1/status/runtimeinfo":
			_, _ = w.Write([]byte(`{"status":"success","data":{"storageRetention":"15d"}}`))
		case "/api/v1/labels":
			_, _ = w.Write([]byte(`{"status":"success","data":["__name__","service","pod","pod_uid"]}`))
		case "/api/v1/query":
			q := r.URL.Query().Get("query")
			if strings.Contains(q, "pod, pod_uid") {
				_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"value":[1700000000,"120"]}]}}`))
			} else {
				_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"value":[1700000000,"1200"]}]}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.InventoryConfig{PrometheusURL: srv.URL, MaxCollapseFamilies: 5}
	snap, eval, err := FetchHTTP(context.Background(), cfg, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("FetchHTTP should NOT hard-fail on 15d local retention: %v", err)
	}
	if len(snap.Warnings) == 0 || !strings.Contains(snap.Warnings[0], "15d") {
		t.Errorf("expected short-retention warning, got %v", snap.Warnings)
	}
	if len(snap.MetricNames) != 3 {
		t.Errorf("expected 3 historical metric names (including weekly_cron_job_runs), got %v", snap.MetricNames)
	}
	combined, _, err := eval.EstimateCollapse(context.Background(), "http_requests_total", []string{"pod", "pod_uid"}, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if combined.CollapsedSeries != 120 || combined.SavingsPercent != 90.0 {
		t.Fatalf("unexpected HTTP collapse estimate: %+v", combined)
	}
}
