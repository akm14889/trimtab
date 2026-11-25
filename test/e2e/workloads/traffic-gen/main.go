package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	mimirBase := strings.TrimRight(getenv("MIMIR_URL", "http://mimir.monitoring.svc.cluster.local:8080/prometheus"), "/")
	tenantID := getenv("TENANT_ID", "e2e")

	client := &http.Client{Timeout: 10 * time.Second}

	runQuery := func(ua, promql string, isRange bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		now := time.Now().UTC()
		params := url.Values{}
		params.Set("query", promql)
		endpoint := "/api/v1/query"
		if isRange {
			endpoint = "/api/v1/query_range"
			params.Set("start", now.Add(-10*time.Minute).Format(time.RFC3339))
			params.Set("end", now.Format(time.RFC3339))
			params.Set("step", "30s")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, mimirBase+endpoint+"?"+params.Encode(), nil)
		if err != nil {
			log.Printf("build request ua=%s err=%v", ua, err)
			return
		}
		req.Header.Set("User-Agent", ua)
		if tenantID != "" {
			req.Header.Set("X-Scope-OrgID", tenantID)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("query failed ua=%s err=%v", ua, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("query non-2xx ua=%s status=%d query=%q", ua, resp.StatusCode, promql)
		}
	}

	humanQueries := []string{
		`sum by (route, status) (rate(e2e_used_requests_total[5m]))`,
		`histogram_quantile(0.95, sum by (le, route) (rate(e2e_hist_full_seconds_bucket[5m])))`,
		`sum by (le, route) (rate(e2e_labels_http_duration_seconds_bucket[5m]))`,
		`sum by (route) (rate(e2e_latency_seconds_sum[5m])) / sum by (route) (rate(e2e_latency_seconds_count[5m]))`,
		`sum by (job) (rate(e2e_scalar_cmp_total[5m]) > 0)`,
		`sum by (service) (e2e:rr_live:rate5m)`,
	}

	// Wait briefly until Mimir is reachable, then emit initial queries immediately.
	log.Printf("traffic-gen starting against %s (tenant=%s, REGRET_MODE=%s, ROTATION_ACTIVE=%s)",
		mimirBase, tenantID, getenv("REGRET_MODE", "false"), getenv("ROTATION_ACTIVE", "true"))

	fireCycle := func(cycle int) {
		regretMode := strings.EqualFold(os.Getenv("REGRET_MODE"), "true")
		rotationActive := !strings.EqualFold(os.Getenv("ROTATION_ACTIVE"), "false")

		for _, q := range humanQueries {
			runQuery("grafana/12.3.1", q, true)
		}
		// Autoscaler query: bare metric matching KEDA-style consumer
		runQuery("autoscaler/keda-standin-1.0", `e2e_autoscaler_metric`, false)

		if rotationActive {
			runQuery("grafana/12.3.1", `sum by (pod) (e2e_rotation_gauge)`, true)
		}
		if regretMode {
			runQuery("grafana/12.3.1", `sum by (slot) (e2e_regret_victim_gauge)`, true)
		}
		if cycle == 0 || cycle%6 == 0 {
			runQuery("explorer/cardinality-audit-1.0", `count by (__name__) ({__name__=~".+"})`, false)
		}
	}

	fireCycle(0)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	cycle := 1
	for range ticker.C {
		fireCycle(cycle)
		cycle++
	}
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
