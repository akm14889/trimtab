package report

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestRenderFormats(t *testing.T) {
	rep := &metrics.AuditReport{
		Name:        "prod-metrics",
		GeneratedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		WindowDays:  30,
		SoakWindow:  7 * 24 * time.Hour,
		Target:      "alloy",
		Summary: metrics.AuditSummary{
			TotalFamilies:            1240,
			TotalActiveSeries:        391000,
			DroppableFamilies:        412,
			DroppableSeries:          184200,
			DroppablePercent:         47.1,
			Confidence:               metrics.ConfidenceHigh,
			ActiveTranche:            1,
			TotalTranches:            4,
			SynthesizedFamilies:      2,
			SynthesizedSeries:        57868,
			SynthesizedPercent:       14.8,
			MaxDropPercentPerTranche: 15.0,
			ProtectedFamilies:        828,
			ActiveDropWatchCount:     14,
		},
		Safety: metrics.SafetyGateStatus{
			QueryLogConfigured:  true,
			QueryLogCoveredDays: 30,
			RequiredWindowDays:  30,
			APIUserQueries:      1482910,
			RuleEvalQueries:     412000,
		},
		WildcardSources: []metrics.WildcardSource{
			{SourceName: "Cardinality Explorer", MatchPercent: 100, Excluded: true},
		},
		ActiveDrops: []metrics.Recommendation{
			{FamilyName: "checkout_legacy_cart_gauge", MetricType: metrics.MetricTypeGauge, Action: metrics.ActionDropFamily, Series: 42800, Reason: "0 live consumers (observed 45d)"},
			{FamilyName: "orders_deprecated_latency_seconds", MetricType: metrics.MetricTypeHistogram, Action: metrics.ActionDropFamily, Series: 15068, Reason: "dead recording-rule chain (2 rules)"},
		},
		DeadRecordingRules: []metrics.DeadRuleAdvisory{
			{RuleName: "orders:deprecated_latency:p99", File: "rules/orders-legacy.yaml", Group: "orders_legacy"},
			{RuleName: "orders:deprecated_latency:rate5m", File: "rules/orders-legacy.yaml", Group: "orders_legacy", HasOrVectorZero: true},
		},
		LabelCollapseAdvisories: []metrics.LabelCollapseAdvisory{
			{
				FamilyName:   "payments_http_duration_seconds",
				TargetMetric: "payments_http_duration_seconds_bucket",
				UnusedLabels: []string{"instance", "pod", "pod_uid"},
				CombinedEstimate: &metrics.LabelCollapseEstimate{
					DroppedLabels:   []string{"pod", "pod_uid", "instance"},
					CurrentSeries:   42100,
					CollapsedSeries: 5050,
					SavingsPercent:  88.0,
				},
				SingleLabelEstimates: []metrics.LabelCollapseEstimate{
					{DroppedLabels: []string{"pod_uid"}, CurrentSeries: 42100, CollapsedSeries: 42100, SavingsPercent: 0.0, IsSingleLabel: true},
				},
			},
		},
		SynthesizedOutputPath: "./out/trimtab.alloy",
	}

	tableOut, err := Render(rep, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"TRIMTAB AUDIT REPORT — prod-metrics",
		"391,000",
		"1,482,910 api-user queries",
		"checkout_legacy_cart_gauge",
		"ADVISORY: DEAD RECORDING RULES TO DELETE",
		"0.0% (no collapse)",
		"REGRET CHECK (forward-only): 0 regrets",
		"Next tranche (2/4) unlocks after 7d soak with 0 regrets.",
	} {
		if !strings.Contains(tableOut, needle) {
			t.Errorf("table output missing %q:\n%s", needle, tableOut)
		}
	}

	mdOut, err := Render(rep, FormatMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mdOut, "# Trimtab Audit Report") || !strings.Contains(mdOut, "## Advisory: Dead Recording Rules to Delete") {
		t.Errorf("unexpected markdown output:\n%s", mdOut)
	}

	jsonOut, err := Render(rep, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip metrics.AuditReport
	if err := json.Unmarshal([]byte(jsonOut), &roundTrip); err != nil {
		t.Fatalf("invalid JSON report: %v", err)
	}
	if roundTrip.Name != "prod-metrics" {
		t.Errorf("roundTrip.Name = %q", roundTrip.Name)
	}

	promRWRep := *rep
	promRWRep.Target = "promrw"
	promRWRep.SynthesizedOutputPath = "./out/trimtab-remote-write.yaml"
	promRWTable, err := Render(&promRWRep, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(promRWTable, "Synthesized Prometheus remote_write rules -> ./out/trimtab-remote-write.yaml") {
		t.Errorf("promrw table output missing Prometheus remote_write rules footer:\n%s", promRWTable)
	}

	// Observation-only next eligible tranche & pending_confirmation rendering
	obsRep := *rep
	obsRep.ActiveDrops = nil
	obsRep.SynthesizedOutputPath = ""
	obsRep.Summary.ActiveTranche = 0
	obsRep.Summary.SynthesizedFamilies = 0
	obsRep.Summary.SynthesizedSeries = 0
	obsRep.Summary.SynthesizedPercent = 0
	obsRep.Safety.NextTrancheReady = true
	obsRep.Tranches = []metrics.Tranche{{
		Number: 1,
		Series: 57868,
		Items:  rep.ActiveDrops,
	}}
	obsTable, err := Render(&obsRep, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(obsTable, "NEXT ELIGIBLE TRANCHE (Tranche 1/4") || !strings.Contains(obsTable, "trimtab synth --activate") {
		t.Errorf("expected observation-only next eligible tranche guidance:\n%s", obsTable)
	}

	pendingRep := *rep
	pendingRep.Safety.PendingConfirmation = true
	pendingRep.Safety.ActiveTrancheStatus = "pending_confirmation"
	pendingTable, err := Render(&pendingRep, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pendingTable, "[pending_confirmation]") || !strings.Contains(pendingTable, "awaiting confirmation") {
		t.Errorf("expected pending_confirmation status & footer:\n%s", pendingTable)
	}

	// HTML format: standalone, deterministic, zero external URLs, XSS auto-escaped
	htmlOut1, err := Render(rep, FormatHTML)
	if err != nil {
		t.Fatalf("Render(FormatHTML) error: %v", err)
	}
	htmlOut2, err := Render(rep, FormatHTML)
	if err != nil {
		t.Fatalf("second Render(FormatHTML) error: %v", err)
	}
	if htmlOut1 != htmlOut2 {
		t.Fatal("HTML output must be deterministic")
	}
	for _, want := range []string{
		"<!DOCTYPE html>",
		"Trimtab Audit Report &mdash; prod-metrics",
		"391,000",
		"checkout_legacy_cart_gauge",
		"orders:deprecated_latency:rate5m",
		"or vector(0)",
		"0.0% (no collapse)",
		"-88.0%",
	} {
		if !strings.Contains(htmlOut1, want) {
			t.Errorf("HTML output missing %q", want)
		}
	}
	if strings.Contains(htmlOut1, "<script src=") || strings.Contains(htmlOut1, `<link rel="stylesheet" href="http`) {
		t.Fatal("HTML report must not depend on external scripts or stylesheets")
	}

	xssRep := *rep
	xssRep.Name = `<script>alert("xss")</script>`
	xssRep.Regrets = []metrics.RegretEvent{{
		FamilyName:  "bad_metric",
		Query:       `<img src=x onerror=alert(1)>`,
		HitCount:    1,
		ActionTaken: "reverted",
	}}
	xssHTML, err := Render(&xssRep, FormatHTML)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(xssHTML, `<script>alert`) || strings.Contains(xssHTML, `<img src=x`) {
		t.Fatal("HTML report failed to escape untrusted input")
	}
	if !strings.Contains(xssHTML, "&lt;script&gt;alert") {
		t.Fatal("expected escaped script tag in HTML report")
	}
}

func TestPrometheusMetricsAndBundledGrafanaDashboard(t *testing.T) {
	rep := &metrics.AuditReport{
		Name:        "prod-metrics",
		GeneratedAt: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
		WindowDays:  30,
		Target:      "alloy",
		Summary: metrics.AuditSummary{
			TotalFamilies:        200,
			TotalActiveSeries:    100000,
			DroppableFamilies:    25,
			DroppableSeries:      12000,
			DroppablePercent:     12.0,
			Confidence:           metrics.ConfidenceHigh,
			ActiveTranche:        1,
			TotalTranches:        2,
			SynthesizedFamilies:  10,
			SynthesizedSeries:    5000,
			SynthesizedPercent:   5.0,
			ProtectedFamilies:    175,
			ActiveDropWatchCount: 10,
		},
		Safety: metrics.SafetyGateStatus{
			QueryLogConfigured:    true,
			QueryLogCoveredDays:   30,
			RequiredWindowDays:    30,
			APIUserQueries:        1200,
			RuleEvalQueries:       400,
			ExcludedQueries:       50,
			BaselineDriftDetected: true,
			Blocked:               true,
			UnresolvedRegrets:     1,
		},
		DeadRecordingRules:      []metrics.DeadRuleAdvisory{{RuleName: "r1"}},
		LabelCollapseAdvisories: []metrics.LabelCollapseAdvisory{{FamilyName: "f1"}},
		Regrets:                 []metrics.RegretEvent{{FamilyName: "v1"}},
	}

	promText := RenderPrometheusMetrics(rep, `e2e"tenant`)
	for _, want := range []string{
		`# TYPE trimtab_active_series gauge`,
		`trimtab_active_series{name="prod-metrics",tenant="e2e\"tenant"} 100000`,
		`trimtab_query_log_queries{name="prod-metrics",tenant="e2e\"tenant",origin="api-user"} 1200`,
		`trimtab_safety_gate_blocked{name="prod-metrics",tenant="e2e\"tenant"} 1`,
		`trimtab_baseline_drift_detected{name="prod-metrics",tenant="e2e\"tenant"} 1`,
		`trimtab_regret_events_total{name="prod-metrics",tenant="e2e\"tenant"} 1`,
		`trimtab_last_audit_timestamp_seconds{name="prod-metrics",tenant="e2e\"tenant"} 1795262400`,
	} {
		if !strings.Contains(promText, want) {
			t.Errorf("Prometheus exposition missing %q:\n%s", want, promText)
		}
	}

	// Pushgateway mock server check
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := PushPrometheusMetrics(context.Background(), srv.URL, "trimtab", "prod-metrics", promText, config.HTTPAuthConfig{}, 5*time.Second); err != nil {
		t.Fatalf("PushPrometheusMetrics failed: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/metrics/job/trimtab/instance/prod-metrics" || gotBody != promText {
		t.Fatalf("unexpected push request: method=%q path=%q", gotMethod, gotPath)
	}

	// Verify bundled Grafana dashboard JSONs (deploy + chart) are valid, identical, and reference only real metrics.
	deployDashPath := filepath.Join("..", "..", "deploy", "dashboards", "trimtab-overview.json")
	chartDashPath := filepath.Join("..", "..", "charts", "trimtab", "dashboards", "trimtab-overview.json")
	deployBytes, err := os.ReadFile(deployDashPath)
	if err != nil {
		t.Fatalf("read %s: %v", deployDashPath, err)
	}
	chartBytes, err := os.ReadFile(chartDashPath)
	if err != nil {
		t.Fatalf("read %s: %v", chartDashPath, err)
	}
	if !bytes.Equal(deployBytes, chartBytes) {
		t.Fatal("deploy/dashboards/trimtab-overview.json and charts/trimtab/dashboards/trimtab-overview.json must stay identical")
	}

	var dash struct {
		UID    string `json:"uid"`
		Title  string `json:"title"`
		Panels []struct {
			ID      int            `json:"id"`
			Title   string         `json:"title"`
			GridPos map[string]int `json:"gridPos"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(deployBytes, &dash); err != nil {
		t.Fatalf("invalid dashboard JSON: %v", err)
	}
	if dash.UID != "trimtab-control-plane" || len(dash.Panels) < 10 {
		t.Fatalf("unexpected dashboard metadata uid=%q panels=%d", dash.UID, len(dash.Panels))
	}

	metricRe := regexp.MustCompile(`trimtab_[a-z0-9_]+`)
	for _, p := range dash.Panels {
		if p.GridPos["w"] <= 0 || p.GridPos["h"] <= 0 {
			t.Errorf("panel %d (%s) missing valid gridPos: %+v", p.ID, p.Title, p.GridPos)
		}
		for _, tgt := range p.Targets {
			for _, m := range metricRe.FindAllString(tgt.Expr, -1) {
				if !strings.Contains(promText, m+"{") {
					t.Errorf("dashboard panel %q references unknown metric %q not emitted by RenderPrometheusMetrics", p.Title, m)
				}
			}
		}
	}
}
