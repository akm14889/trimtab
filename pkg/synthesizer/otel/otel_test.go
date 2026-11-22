package otel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
)

func TestSynthesizeOTelGoldenAndContracts(t *testing.T) {
	auditTime := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	syn := New()

	caps := syn.Capabilities()
	if !caps.CanDropFamily || caps.CanDropHistogramBucketsOnly {
		t.Fatalf("unexpected OTel capabilities: %+v", caps)
	}

	validateYAMLNoService := func(t *testing.T, content string) map[string]any {
		t.Helper()
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			t.Fatalf("invalid OTel YAML: %v\n%s", err, content)
		}
		if _, hasService := doc["service"]; hasService {
			t.Fatal("OTel overlay MUST NEVER contain a `service:` block")
		}
		return doc
	}

	t.Run("normal verified family drops", func(t *testing.T) {
		rep := &metrics.AuditReport{
			GeneratedAt: auditTime,
			Summary: metrics.AuditSummary{
				ActiveTranche:      1,
				TotalTranches:      4,
				SynthesizedPercent: 14.8,
			},
			Safety: metrics.SafetyGateStatus{
				QueryLogCoveredDays: 30,
				RequiredWindowDays:  30,
			},
			ActiveDrops: []metrics.Recommendation{
				{
					FamilyName: "checkout_legacy_cart_gauge",
					MetricType: metrics.MetricTypeGauge,
					Action:     metrics.ActionDropFamily,
					Series:     42800,
					Reason:     "0 live consumers in 30d",
					NamingMode: metrics.NamingPrometheusReceiver,
				},
				{
					FamilyName: "orders_deprecated_latency_seconds",
					MetricType: metrics.MetricTypeHistogram,
					Action:     metrics.ActionDropFamily,
					Series:     15068,
					Reason:     "dead recording-rule chain (2 rules)",
					NamingMode: metrics.NamingExplicitOTLP,
					OTLPName:   "orders.deprecated.latency",
				},
			},
		}
		art, err := syn.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		validateYAMLNoService(t, art.Content)
		assertGolden(t, "normal.otel.yaml.golden", art.Content)
		if art.DroppedFamilies != 2 || len(art.SkippedAdvisories) != 0 {
			t.Fatalf("unexpected artifact stats: %+v", art)
		}
	})

	t.Run("zero drop emits valid non-matching OTTL predicate", func(t *testing.T) {
		rep := &metrics.AuditReport{
			GeneratedAt: auditTime,
			Safety: metrics.SafetyGateStatus{
				ReportOnly:          true,
				QueryLogCoveredDays: 3,
				RequiredWindowDays:  30,
			},
		}
		art, err := syn.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		validateYAMLNoService(t, art.Content)
		assertGolden(t, "noop.otel.yaml.golden", art.Content)
		if !strings.Contains(art.Content, `'name == "__trimtab_noop__"'`) {
			t.Fatal("0-drop OTel config must emit valid non-matching OTTL predicate")
		}
	})

	t.Run("skips ambiguous reverse-translation and bucket-only drops with advisories", func(t *testing.T) {
		rep := &metrics.AuditReport{
			GeneratedAt: auditTime,
			Summary:     metrics.AuditSummary{ActiveTranche: 1, TotalTranches: 1, SynthesizedPercent: 5.0},
			Safety:      metrics.SafetyGateStatus{QueryLogCoveredDays: 30, RequiredWindowDays: 30},
			ActiveDrops: []metrics.Recommendation{
				{
					FamilyName: "ambiguous_otlp_metric_bytes",
					MetricType: metrics.MetricTypeGauge,
					Action:     metrics.ActionDropFamily,
					Series:     5000,
					NamingMode: metrics.NamingTranslatedAmbiguous,
				},
				{
					FamilyName: "classic_hist",
					MetricType: metrics.MetricTypeHistogram,
					Action:     metrics.ActionDropHistogramBuckets,
					Series:     3000,
					NamingMode: metrics.NamingPrometheusReceiver,
				},
			},
		}
		art, err := syn.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		validateYAMLNoService(t, art.Content)
		if art.DroppedFamilies != 0 {
			t.Fatalf("expected 0 drops when all items are ambiguous or bucket-only, got %d", art.DroppedFamilies)
		}
		if len(art.SkippedAdvisories) != 2 {
			t.Fatalf("expected 2 skipped advisories, got %v", art.SkippedAdvisories)
		}
		if !strings.Contains(art.Content, `'name == "__trimtab_noop__"'`) {
			t.Fatal("when all drops are skipped, OTel output must fall back to __trimtab_noop__")
		}
	})
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	wantBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	want := string(wantBytes)
	if got != want {
		t.Fatalf("golden mismatch for %s:\n--- GOT ---\n%s\n--- WANT ---\n%s", name, got, want)
	}
}
