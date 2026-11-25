package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

var (
	buckets10 = []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "+Inf"}
	buckets8  = []string{"0.01", "0.05", "0.1", "0.25", "0.5", "1", "5", "+Inf"}
)

func main() {
	podName := getenv("POD_NAME", "noisy-app-0")
	podUID := getenv("POD_UID", "uid-local-0")

	var scrapeTick atomic.Uint64

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		tick := scrapeTick.Add(1)
		enableNew := strings.EqualFold(os.Getenv("ENABLE_NEW_METRIC"), "true")

		var buf bytes.Buffer

		// 1. e2e_used_requests_total: 36 routes x 5 statuses x 2 methods x 3 pods = 1,080 series
		buf.WriteString("# HELP e2e_used_requests_total Requests handled by noisy-app (live used counter).\n")
		buf.WriteString("# TYPE e2e_used_requests_total counter\n")
		for i := 0; i < 36; i++ {
			route := fmt.Sprintf("/api/v1/item/%02d", i)
			for _, st := range []string{"200", "201", "400", "404", "500"} {
				for _, m := range []string{"GET", "POST"} {
					fmt.Fprintf(&buf, "e2e_used_requests_total{pod=%q,route=%q,status=%q,method=%q} %d\n",
						podName, route, st, m, tick*10+uint64(i))
				}
			}
		}

		// 2. e2e_hist_full_seconds: 15 routes x (10 buckets + sum + count) x 3 pods = 540 series
		buf.WriteString("# HELP e2e_hist_full_seconds Request duration queried via histogram_quantile.\n")
		buf.WriteString("# TYPE e2e_hist_full_seconds histogram\n")
		for i := 0; i < 15; i++ {
			route := fmt.Sprintf("/checkout/%02d", i)
			for bIdx, le := range buckets10 {
				fmt.Fprintf(&buf, "e2e_hist_full_seconds_bucket{pod=%q,route=%q,le=%q} %d\n",
					podName, route, le, tick*uint64(bIdx+1)+uint64(i))
			}
			fmt.Fprintf(&buf, "e2e_hist_full_seconds_sum{pod=%q,route=%q} %.2f\n",
				podName, route, float64(tick)*0.42)
			fmt.Fprintf(&buf, "e2e_hist_full_seconds_count{pod=%q,route=%q} %d\n",
				podName, route, tick*10)
		}

		// 3. e2e_labels_http_duration_seconds: 4 routes x 10 request_id x (8 buckets + sum + count) x 3 pods = 1,200 series
		// Has unused pod, pod_uid (1:1 per pod), and request_id so single-label without(pod_uid) == 0% while combined > 0%.
		buf.WriteString("# HELP e2e_labels_http_duration_seconds Histogram with pod, pod_uid, and request_id unused labels.\n")
		buf.WriteString("# TYPE e2e_labels_http_duration_seconds histogram\n")
		for rIdx := 0; rIdx < 4; rIdx++ {
			route := fmt.Sprintf("/pay/%02d", rIdx)
			for req := 0; req < 10; req++ {
				reqID := fmt.Sprintf("req-%02d", req)
				for bIdx, le := range buckets8 {
					fmt.Fprintf(&buf, "e2e_labels_http_duration_seconds_bucket{pod=%q,pod_uid=%q,route=%q,request_id=%q,le=%q} %d\n",
						podName, podUID, route, reqID, le, tick*uint64(bIdx+1))
				}
				fmt.Fprintf(&buf, "e2e_labels_http_duration_seconds_sum{pod=%q,pod_uid=%q,route=%q,request_id=%q} %.2f\n",
					podName, podUID, route, reqID, float64(tick)*0.15)
				fmt.Fprintf(&buf, "e2e_labels_http_duration_seconds_count{pod=%q,pod_uid=%q,route=%q,request_id=%q} %d\n",
					podName, podUID, route, reqID, tick*8)
			}
		}

		// 4. e2e_alert_only_gauge: 10 services x 3 pods = 30 series (protected by Mimir alerting rule only)
		buf.WriteString("# HELP e2e_alert_only_gauge Alert-only queue depth gauge.\n")
		buf.WriteString("# TYPE e2e_alert_only_gauge gauge\n")
		for i := 0; i < 10; i++ {
			fmt.Fprintf(&buf, "e2e_alert_only_gauge{pod=%q,service=%q} %d\n",
				podName, fmt.Sprintf("svc-%02d", i), 10+i)
		}

		// 5. e2e_rr_source_total: 10 services x 3 pods = 30 series (transitively protected by e2e:rr_live:rate5m)
		buf.WriteString("# HELP e2e_rr_source_total Recording-rule live chain source counter.\n")
		buf.WriteString("# TYPE e2e_rr_source_total counter\n")
		for i := 0; i < 10; i++ {
			fmt.Fprintf(&buf, "e2e_rr_source_total{pod=%q,service=%q} %d\n",
				podName, fmt.Sprintf("rr-svc-%02d", i), tick*5+uint64(i))
		}

		// 6. e2e_autoscaler_metric: 5 queues x 3 pods = 15 series (queried bare by autoscaler traffic-gen loop)
		buf.WriteString("# HELP e2e_autoscaler_metric Queue lag gauge consumed by autoscaler.\n")
		buf.WriteString("# TYPE e2e_autoscaler_metric gauge\n")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&buf, "e2e_autoscaler_metric{pod=%q,queue=%q} %d\n",
				podName, fmt.Sprintf("q-%02d", i), 3+i)
		}

		// 7. e2e_scalar_cmp_total: 4 env x 4 tier x 3 pods = 48 series (queried via sum by (job) (rate(x[5m]) > 0))
		buf.WriteString("# HELP e2e_scalar_cmp_total Counter for scalar-comparison label precision test.\n")
		buf.WriteString("# TYPE e2e_scalar_cmp_total counter\n")
		for _, env := range []string{"dev", "qa", "stage", "canary"} {
			for _, tier := range []string{"web", "api", "worker", "db"} {
				fmt.Fprintf(&buf, "e2e_scalar_cmp_total{pod=%q,env=%q,tier=%q} %d\n",
					podName, env, tier, tick*20)
			}
		}

		// 8. e2e_rotation_gauge: 5 slots x 3 pods = 15 series (queried early in S9, then traffic stopped)
		buf.WriteString("# HELP e2e_rotation_gauge Metric queried early then stopped to test ledger rotation survival.\n")
		buf.WriteString("# TYPE e2e_rotation_gauge gauge\n")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&buf, "e2e_rotation_gauge{pod=%q,slot=%q} %d\n",
				podName, fmt.Sprintf("rot-%02d", i), 42+i)
		}

		// 9. e2e_unused_big_gauge: 200 shards x 3 pods = 600 series (largest droppable family, ~14% of estate -> Tranche 1)
		buf.WriteString("# HELP e2e_unused_big_gauge High-cardinality unqueried gauge across 200 shards.\n")
		buf.WriteString("# TYPE e2e_unused_big_gauge gauge\n")
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&buf, "e2e_unused_big_gauge{pod=%q,shard=%q} %d\n",
				podName, fmt.Sprintf("shard-%03d", i), i%17)
		}

		// 10. e2e_latency_seconds: 8 routes x (10 buckets + sum + count) x 3 pods = 288 series (240 buckets droppable)
		buf.WriteString("# HELP e2e_latency_seconds Classic histogram queried only via _sum and _count.\n")
		buf.WriteString("# TYPE e2e_latency_seconds histogram\n")
		for i := 0; i < 8; i++ {
			route := fmt.Sprintf("/catalog/%02d", i)
			for bIdx, le := range buckets10 {
				fmt.Fprintf(&buf, "e2e_latency_seconds_bucket{pod=%q,route=%q,le=%q} %d\n",
					podName, route, le, tick*uint64(bIdx+1))
			}
			fmt.Fprintf(&buf, "e2e_latency_seconds_sum{pod=%q,route=%q} %.2f\n",
				podName, route, float64(tick)*0.25)
			fmt.Fprintf(&buf, "e2e_latency_seconds_count{pod=%q,route=%q} %d\n",
				podName, route, tick*12)
		}

		// 11. e2e_regret_victim_gauge: 40 slots x 3 pods = 120 series (unused initially, queried in S8 Regret)
		buf.WriteString("# HELP e2e_regret_victim_gauge Unused gauge dropped then queried in S8 regret scenario.\n")
		buf.WriteString("# TYPE e2e_regret_victim_gauge gauge\n")
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&buf, "e2e_regret_victim_gauge{pod=%q,slot=%q} %d\n",
				podName, fmt.Sprintf("vic-%02d", i), 100+i)
		}

		// 12. e2e_unused_legacy_gauge: 35 slots x 3 pods = 105 series
		buf.WriteString("# HELP e2e_unused_legacy_gauge Moderate-cardinality unused legacy gauge.\n")
		buf.WriteString("# TYPE e2e_unused_legacy_gauge gauge\n")
		for i := 0; i < 35; i++ {
			fmt.Fprintf(&buf, "e2e_unused_legacy_gauge{pod=%q,slot=%q} %d\n",
				podName, fmt.Sprintf("leg-%02d", i), 7+i)
		}

		// 13. e2e_rr_dead_source_total: 30 services x 3 pods = 90 series (dead recording chain input)
		buf.WriteString("# HELP e2e_rr_dead_source_total Input counter for dead recording-rule chain.\n")
		buf.WriteString("# TYPE e2e_rr_dead_source_total counter\n")
		for i := 0; i < 30; i++ {
			fmt.Fprintf(&buf, "e2e_rr_dead_source_total{pod=%q,service=%q} %d\n",
				podName, fmt.Sprintf("dead-%02d", i), tick*3+uint64(i))
		}

		// 14. e2e_new_metric: 5 slots x 3 pods = 15 series (enabled mid-test for S12 observation window)
		if enableNew {
			buf.WriteString("# HELP e2e_new_metric Newly introduced gauge protected by minObservationWindow.\n")
			buf.WriteString("# TYPE e2e_new_metric gauge\n")
			for i := 0; i < 5; i++ {
				fmt.Fprintf(&buf, "e2e_new_metric{pod=%q,slot=%q} %d\n",
					podName, fmt.Sprintf("new-%02d", i), tick+uint64(i))
			}
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	log.Printf("noisy-app listening on :8080 (pod=%s)", podName)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
