package querylog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestOriginClassificationAndGlob(t *testing.T) {
	dir := t.TempDir()
	log1 := `{"params":{"query":"rate(http_requests_total[5m])"},"httpRequest":{"clientIP":"10.0.0.1","path":"/api/v1/query_range","headers":{"User-Agent":"Grafana/11"}},"ts":"2026-10-22T00:00:00Z"}
{"params":{"query":"sum(orders_deprecated_latency_seconds_bucket)"},"ruleGroup":{"file":"rules/orders.yaml","name":"orders_legacy"},"ts":"2026-11-01T00:00:00Z"}
{"params":{"query":"count({__name__=~\".+\"})"},"httpRequest":{"clientIP":"10.0.0.9","path":"/api/v1/query","headers":{"User-Agent":"cardinality-explorer"}},"ts":"2026-11-10T00:00:00Z"}
{"params":{"query":"up"},"ts":"2026-11-21T00:00:00Z"}
`
	if err := os.WriteFile(filepath.Join(dir, "replica-0.jsonl"), []byte(log1), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	res, err := ReadSources(
		context.Background(),
		[]config.QueryLogSourceConfig{{Glob: filepath.Join(dir, "*.jsonl")}},
		[]config.ExcludeSource{{Type: "query_caller", Value: "cardinality-explorer"}},
		30*24*time.Hour,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.APIUserCount != 1 {
		t.Errorf("APIUserCount = %d, want 1", res.APIUserCount)
	}
	if res.RuleEvalCount != 2 { // 1 explicit ruleGroup + 1 nil httpRequest
		t.Errorf("RuleEvalCount = %d, want 2", res.RuleEvalCount)
	}
	if res.ExcludedCount != 1 {
		t.Errorf("ExcludedCount = %d, want 1", res.ExcludedCount)
	}
	if days := res.LatestTimestamp.Sub(res.EarliestTimestamp).Hours() / 24.0; days < 29.9 {
		t.Errorf("coverage = %.2fd, want ~30d", days)
	}
	if len(res.ObservedDates) != 4 {
		t.Errorf("ObservedDates len = %d, want 4 (%+v)", len(res.ObservedDates), res.ObservedDates)
	}
	// Verify excluded entry produced no usage ref
	for _, r := range res.Refs {
		if r.Origin == metrics.OriginExcludedExplorer {
			t.Fatalf("excluded explorer query should not emit UsageRef")
		}
	}
}

func TestDashboardUIDHeaderExtractionAndExclusion(t *testing.T) {
	dir := t.TempDir()
	logContent := `{"params":{"query":"rate(kept_metric[5m])"},"httpRequest":{"clientIP":"10.0.0.1","path":"/api/v1/query_range","headers":{"User-Agent":"Grafana/11","X-Dashboard-UID":"dash-keep"}},"ts":"2026-11-20T00:00:00Z"}
{"params":{"query":"count({__name__=~\".+\"})"},"httpRequest":{"clientIP":"10.0.0.1","path":"/api/v1/query","headers":{"User-Agent":"Grafana/11","X-Dashboard-UID":"dash-exp"}},"ts":"2026-11-21T00:00:00Z"}
`
	if err := os.WriteFile(filepath.Join(dir, "q.jsonl"), []byte(logContent), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	res, err := ReadSources(
		context.Background(),
		[]config.QueryLogSourceConfig{{Glob: filepath.Join(dir, "*.jsonl")}},
		[]config.ExcludeSource{{Type: "dashboard_uid", Value: "dash-exp"}},
		30*24*time.Hour,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.APIUserCount != 1 || res.ExcludedCount != 1 {
		t.Fatalf("got api=%d excluded=%d, want 1/1", res.APIUserCount, res.ExcludedCount)
	}
	if len(res.Refs) != 1 || res.Refs[0].DashboardUID != "dash-keep" || res.Refs[0].MetricName != "kept_metric" {
		t.Fatalf("unexpected refs: %+v", res.Refs)
	}
}

func TestPromAnalyticsProxyArrayFormat(t *testing.T) {
	dir := t.TempDir()
	content := `[
		{"query":"sum by (service) (payments_total)","timestamp":"2026-11-01T00:00:00Z","clientIP":"10.1.2.3"},
		{"queryParam":"rate(checkout_total[5m])","timestamp":"2026-11-21T00:00:00Z","caller":"grafana"}
	]`
	p := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ReadSources(context.Background(), []config.QueryLogSourceConfig{{Glob: p, Format: "prom-analytics-proxy"}}, nil, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.APIUserCount != 2 || len(res.Refs) != 2 {
		t.Fatalf("expected 2 api-user entries & refs, got api=%d refs=%d", res.APIUserCount, len(res.Refs))
	}
}

func TestLokiQueryLogSource(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scope-OrgID") != "prod-tenant" {
			http.Error(w, "missing tenant header", http.StatusUnauthorized)
			return
		}
		resp := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "streams",
				"result": []any{
					map[string]any{
						"stream": map[string]string{"job": "prometheus"},
						"values": [][2]string{
							{strconv.FormatInt(t0.UnixNano(), 10), `{"params":{"query":"rate(loki_api_metric[5m])"},"httpRequest":{"clientIP":"10.0.0.1"},"ts":"2026-11-20T10:00:00Z"}`},
							{strconv.FormatInt(t1.UnixNano(), 10), `level=info caller=grafana query="sum by (service) (mimir_frontend_metric)"`},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	res, err := ReadSources(
		context.Background(),
		[]config.QueryLogSourceConfig{{
			Loki: &config.LokiQueryLogConfig{
				URL:       srv.URL,
				Query:     `{job="prometheus"}`,
				TenantID:  "prod-tenant",
				Window:    config.Duration(48 * time.Hour),
				ChunkSize: config.Duration(48 * time.Hour),
			},
		}},
		nil,
		48*time.Hour,
		t1.Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("Loki ReadSources error: %v", err)
	}
	if res.APIUserCount != 2 {
		t.Fatalf("APIUserCount = %d, want 2", res.APIUserCount)
	}
	seen := map[string]bool{}
	for _, ref := range res.Refs {
		seen[ref.MetricName] = true
	}
	if !seen["loki_api_metric"] || !seen["mimir_frontend_metric"] {
		t.Fatalf("missing expected metrics from Loki: %+v", seen)
	}
}

func TestLokiPaginationParamQuerySelfExclusionAndStreamingDedup(t *testing.T) {
	t.Run("multi-page Loki pagination, param_query, and trimtab User-Agent auto-exclusion", func(t *testing.T) {
		t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
		allValues := [][2]string{
			{strconv.FormatInt(t0.UnixNano(), 10), `level=info caller=grafana param_query="rate(page1_a[5m])"`},
			{strconv.FormatInt(t0.Add(time.Second).UnixNano(), 10), `level=info caller=grafana param_query="rate(page1_b[5m])"`},
			{strconv.FormatInt(t0.Add(2*time.Second).UnixNano(), 10), `level=info user_agent="trimtab/v0.1.0" param_query="count(count without (pod) (should_be_excluded))"`},
			{strconv.FormatInt(t0.Add(3*time.Second).UnixNano(), 10), `level=info caller=grafana param_match[]="{__name__=\"series_match_metric\"}"`},
			{strconv.FormatInt(t0.Add(4*time.Second).UnixNano(), 10), `level=info caller=grafana query="rate(page3_c[5m])"`},
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startNs, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			var page [][2]string
			for _, v := range allValues {
				tsNs, _ := strconv.ParseInt(v[0], 10, 64)
				if tsNs >= startNs {
					page = append(page, v)
					if len(page) == limit {
						break
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{
					"resultType": "streams",
					"result": []any{
						map[string]any{"stream": map[string]string{"job": "mimir"}, "values": page},
					},
				},
			})
		}))
		defer srv.Close()

		res, err := ReadSources(
			context.Background(),
			[]config.QueryLogSourceConfig{{
				Loki: &config.LokiQueryLogConfig{
					URL:       srv.URL,
					Query:     `{job="mimir"}`,
					Limit:     2,
					Window:    config.Duration(24 * time.Hour),
					ChunkSize: config.Duration(24 * time.Hour),
				},
			}},
			nil,
			24*time.Hour,
			t0.Add(time.Hour),
		)
		if err != nil {
			t.Fatalf("paginated Loki ReadSources failed: %v", err)
		}
		if res.TotalEntries != 5 || res.APIUserCount != 4 || res.ExcludedCount != 1 {
			t.Fatalf("unexpected counts: total=%d api=%d excluded=%d", res.TotalEntries, res.APIUserCount, res.ExcludedCount)
		}
		seen := map[string]bool{}
		for _, ref := range res.Refs {
			seen[ref.MetricName] = true
		}
		for _, want := range []string{"page1_a", "page1_b", "series_match_metric", "page3_c"} {
			if !seen[want] {
				t.Errorf("missing paginated metric %q in %+v", want, seen)
			}
		}
		if seen["should_be_excluded"] {
			t.Fatal("trimtab/ User-Agent query must be auto-excluded and never emit UsageRef")
		}
	})

	t.Run("Loki tie-guard fails closed", func(t *testing.T) {
		t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{
					"resultType": "streams",
					"result": []any{
						map[string]any{
							"stream": map[string]string{"job": "mimir"},
							"values": [][2]string{
								{strconv.FormatInt(t0.UnixNano(), 10), `level=info caller=grafana query="m1"`},
								{strconv.FormatInt(t0.UnixNano(), 10), `level=info caller=grafana query="m2"`},
							},
						},
					},
				},
			})
		}))
		defer srv.Close()

		_, err := ReadSources(
			context.Background(),
			[]config.QueryLogSourceConfig{{
				Loki: &config.LokiQueryLogConfig{
					URL:       srv.URL,
					Query:     `{job="mimir"}`,
					Limit:     2,
					Window:    config.Duration(24 * time.Hour),
					ChunkSize: config.Duration(24 * time.Hour),
				},
			}},
			nil,
			24*time.Hour,
			t0.Add(time.Hour),
		)
		if err == nil || !strings.Contains(err.Error(), "tie-guard") {
			t.Fatalf("expected tie-guard fail-closed error, got %v", err)
		}
	})

	t.Run("streaming dedup preserves HitCount and LatestTS for regret detection", func(t *testing.T) {
		dir := t.TempDir()
		lines := `{"params":{"query":"rate(dropped_m[5m])"},"httpRequest":{"clientIP":"10.0.0.1","headers":{"User-Agent":"Grafana/11"}},"ts":"2026-11-21T08:00:00Z"}
{"params":{"query":"rate(dropped_m[5m])"},"httpRequest":{"clientIP":"10.0.0.1","headers":{"User-Agent":"Grafana/11"}},"ts":"2026-11-21T10:00:00Z"}
{"params":{"query":"rate(dropped_m[5m])"},"httpRequest":{"clientIP":"10.0.0.1","headers":{"User-Agent":"Grafana/11"}},"ts":"2026-11-21T12:00:00Z"}
{"params":{"query":"rate(self_check[5m])"},"httpRequest":{"clientIP":"10.0.0.2","headers":{"User-Agent":"trimtab/v0.1.0"}},"ts":"2026-11-21T12:00:00Z"}
`
		if err := os.WriteFile(filepath.Join(dir, "q.jsonl"), []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)
		res, err := ReadSources(context.Background(), []config.QueryLogSourceConfig{{Glob: filepath.Join(dir, "*.jsonl")}}, nil, 24*time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		if res.TotalEntries != 4 || res.APIUserCount != 3 || res.ExcludedCount != 1 {
			t.Fatalf("unexpected totals: %+v", res)
		}
		if len(res.Entries) != 2 {
			t.Fatalf("expected 2 deduplicated entries (1 api-user + 1 excluded), got %d", len(res.Entries))
		}
		dedup := res.Entries[0]
		if dedup.HitCount != 3 {
			t.Errorf("HitCount = %d, want 3", dedup.HitCount)
		}
		if !dedup.EarliestTimestamp.Equal(time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)) {
			t.Errorf("EarliestTimestamp = %v", dedup.EarliestTimestamp)
		}
		if !dedup.Timestamp.Equal(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("LatestTS = %v", dedup.Timestamp)
		}
		if len(res.Refs) != 1 || !res.Refs[0].Timestamp.Equal(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("expected single deduplicated UsageRef with LatestTS, got %+v", res.Refs)
		}
	})
}
