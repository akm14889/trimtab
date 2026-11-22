package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
}
