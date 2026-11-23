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

func TestFetchHTTPLiveScaleHardeningAndAuth(t *testing.T) {
	t.Run("tsdb limit, selector batched counts, scoped collapse, and ProbeFirstSeen", func(t *testing.T) {
		var sawTSDBLimit bool
		var sawScopedNames bool
		var sawBatchedCount bool
		var sawScopedCollapse bool
		var sawProbeWindow bool

		now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		window := 30 * 24 * time.Hour
		startStr := "1792756800" // 2026-10-23T12:00:00Z

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Scope-OrgID") != "prod-tenant" {
				http.Error(w, "missing tenant", http.StatusUnauthorized)
				return
			}
			if !strings.HasPrefix(r.Header.Get("User-Agent"), "trimtab/") {
				t.Errorf("expected User-Agent trimtab/..., got %q", r.Header.Get("User-Agent"))
			}
			switch r.URL.Path {
			case "/api/v1/label/__name__/values":
				match := r.URL.Query().Get("match[]")
				if strings.Contains(match, `cluster="staging"`) {
					sawScopedNames = true
				}
				if r.URL.Query().Get("start") == startStr && r.URL.Query().Get("end") != "" {
					// Check if this is the 24h ProbeFirstSeen call
					if r.URL.Query().Get("end") == "1792843200" {
						sawProbeWindow = true
						_, _ = w.Write([]byte(`{"status":"success","data":["m_a"]}`))
						return
					}
				}
				_, _ = w.Write([]byte(`{"status":"success","data":["m_a","m_b"]}`))
			case "/api/v1/metadata":
				_, _ = w.Write([]byte(`{"status":"success","data":{"m_a":[{"type":"gauge","help":"a"}]}}`))
			case "/api/v1/status/tsdb":
				if r.URL.Query().Get("limit") == "10000" {
					sawTSDBLimit = true
				}
				_, _ = w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[{"name":"m_a","value":100}]}}`))
			case "/api/v1/status/runtimeinfo":
				_, _ = w.Write([]byte(`{"status":"success","data":{"storageRetention":"30d"}}`))
			case "/api/v1/labels":
				_, _ = w.Write([]byte(`{"status":"success","data":["__name__","service","pod"]}`))
			case "/api/v1/query":
				q := r.URL.Query().Get("query")
				if strings.HasPrefix(q, "count by (__name__)") && strings.Contains(q, `cluster="staging"`) {
					sawBatchedCount = true
					_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"__name__":"m_a"},"value":[1,"42"]},{"metric":{"__name__":"m_b"},"value":[1,"8"]}]}}`))
					return
				}
				if strings.Contains(q, `without (pod)`) && strings.Contains(q, `m_a{cluster="staging"}`) {
					sawScopedCollapse = true
					_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"value":[1,"10"]}]}}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			}
		}))
		defer srv.Close()

		// 1. Unscoped checks ?limit=10000
		t.Setenv("TEST_PROM_TOKEN", "prom-tok")
		cfgUnscoped := config.InventoryConfig{
			PrometheusURL: srv.URL,
			TenantID:      "prod-tenant",
			TSDBLimit:     10000,
			Auth:          &config.HTTPAuthConfig{BearerTokenEnv: "TEST_PROM_TOKEN"},
		}
		if _, _, err := FetchHTTP(context.Background(), cfgUnscoped, window, now); err != nil {
			t.Fatal(err)
		}
		if !sawTSDBLimit {
			t.Fatal("expected /api/v1/status/tsdb?limit=10000")
		}

		// 2. Scoped selector uses match[], batched count by (__name__), ProbeFirstSeen, and scoped collapse
		cfgScoped := config.InventoryConfig{
			PrometheusURL:       srv.URL,
			TenantID:            "prod-tenant",
			Selector:            `{cluster="staging"}`,
			ProbeFirstSeen:      true,
			MaxCollapseFamilies: 5,
			Auth:                &config.HTTPAuthConfig{BearerTokenEnv: "TEST_PROM_TOKEN"},
		}
		snap, eval, err := FetchHTTP(context.Background(), cfgScoped, window, now)
		if err != nil {
			t.Fatal(err)
		}
		if !sawScopedNames || !sawBatchedCount || !sawProbeWindow {
			t.Fatalf("missing scoped behavior: names=%v batched=%v probe=%v", sawScopedNames, sawBatchedCount, sawProbeWindow)
		}
		if snap.SeriesCounts["m_a"] != 42 || snap.SeriesCounts["m_b"] != 8 {
			t.Fatalf("unexpected scoped series counts: %+v", snap.SeriesCounts)
		}
		if snap.FirstSeenHints["m_a"].IsZero() {
			t.Fatal("expected FirstSeenHints populated for m_a via ProbeFirstSeen")
		}
		est, _, err := eval.EstimateCollapse(context.Background(), "m_a", []string{"pod"}, 42)
		if err != nil {
			t.Fatal(err)
		}
		if !sawScopedCollapse || est.CollapsedSeries != 10 {
			t.Fatalf("expected scoped collapse query, got saw=%v est=%+v", sawScopedCollapse, est)
		}
	})

	t.Run("401 fails closed immediately", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer srv.Close()

		_, _, err := FetchHTTP(context.Background(), config.InventoryConfig{PrometheusURL: srv.URL}, 30*24*time.Hour, time.Now())
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("expected 401 fail-closed error, got %v", err)
		}
	})
}
