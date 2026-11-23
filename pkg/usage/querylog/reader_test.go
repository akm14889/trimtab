package querylog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	// Verify excluded entry produced no usage ref
	for _, r := range res.Refs {
		if r.Origin == metrics.OriginExcludedExplorer {
			t.Fatalf("excluded explorer query should not emit UsageRef")
		}
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
