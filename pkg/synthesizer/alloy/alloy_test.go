package alloy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
)

func TestSynthesizeAlloyGolden(t *testing.T) {
	auditTime := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	syn := New()

	caps := syn.Capabilities()
	if !caps.CanDropFamily || !caps.CanDropHistogramBucketsOnly {
		t.Fatalf("unexpected Alloy capabilities: %+v", caps)
	}

	t.Run("normal family drops", func(t *testing.T) {
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
				},
				{
					FamilyName: "orders_deprecated_latency_seconds",
					MetricType: metrics.MetricTypeHistogram,
					Action:     metrics.ActionDropFamily,
					Series:     15068,
				},
			},
		}
		art, err := syn.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "normal.alloy.golden", art.Content)
		if !strings.Contains(art.Content, "forward_to = argument.forward_to.value") {
			t.Fatal("Alloy output must reference argument.forward_to.value")
		}
		if strings.Contains(art.Content, "^") || strings.Contains(art.Content, "$") {
			t.Fatal("Alloy relabel regex should be implicitly anchored without ^/$")
		}
	})

	t.Run("zero drop pass-through", func(t *testing.T) {
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
		assertGolden(t, "noop.alloy.golden", art.Content)
		if strings.Contains(art.Content, "rule {") {
			t.Fatal("0-drop Alloy module must omit rule blocks for valid pass-through")
		}
		if !strings.Contains(art.Content, "forward_to = argument.forward_to.value") {
			t.Fatal("Alloy output must reference argument.forward_to.value")
		}
	})

	t.Run("histogram bucket-only drop", func(t *testing.T) {
		rep := &metrics.AuditReport{
			GeneratedAt: auditTime,
			Summary: metrics.AuditSummary{
				ActiveTranche:      1,
				TotalTranches:      1,
				SynthesizedPercent: 8.5,
			},
			Safety: metrics.SafetyGateStatus{
				QueryLogCoveredDays: 30,
				RequiredWindowDays:  30,
			},
			ActiveDrops: []metrics.Recommendation{
				{
					FamilyName: "api_request_duration_seconds",
					MetricType: metrics.MetricTypeHistogram,
					Action:     metrics.ActionDropHistogramBuckets,
					Series:     9200,
				},
			},
		}
		art, err := syn.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "bucket_only.alloy.golden", art.Content)
	})

	t.Run("relabel_rules mode normal and noop", func(t *testing.T) {
		synRules := New(Options{Mode: ModeRelabelRules})
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
				},
				{
					FamilyName: "orders_deprecated_latency_seconds",
					MetricType: metrics.MetricTypeHistogram,
					Action:     metrics.ActionDropFamily,
					Series:     15068,
				},
			},
		}
		art, err := synRules.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "relabel_rules.alloy.golden", art.Content)

		noopRep := &metrics.AuditReport{
			GeneratedAt: auditTime,
			Safety: metrics.SafetyGateStatus{
				ReportOnly:          true,
				QueryLogCoveredDays: 3,
				RequiredWindowDays:  30,
			},
		}
		artNoop, err := synRules.Synthesize(noopRep)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "relabel_rules_noop.alloy.golden", artNoop.Content)
	})

	t.Run("configmap mode normal and noop", func(t *testing.T) {
		synCM := New(Options{Mode: ModeConfigMap, ConfigMapNamespace: "monitoring"})
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
				},
				{
					FamilyName: "orders_deprecated_latency_seconds",
					MetricType: metrics.MetricTypeHistogram,
					Action:     metrics.ActionDropFamily,
					Series:     15068,
				},
			},
		}
		art, err := synCM.Synthesize(rep)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "configmap.yaml.golden", art.Content)

		var doc struct {
			Kind     string            `yaml:"kind"`
			Metadata map[string]any    `yaml:"metadata"`
			Data     map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
			t.Fatalf("invalid configmap YAML: %v", err)
		}
		if doc.Kind != "ConfigMap" || !strings.Contains(doc.Data["trimtab.alloy"], "forward_to = argument.forward_to.value") {
			t.Fatalf("unexpected configmap parsed doc: %+v", doc)
		}

		synCMDefaultNS := New(Options{Mode: ModeConfigMap})
		noopRep := &metrics.AuditReport{
			GeneratedAt: auditTime,
			Safety: metrics.SafetyGateStatus{
				ReportOnly:          true,
				QueryLogCoveredDays: 3,
				RequiredWindowDays:  30,
			},
		}
		artNoop, err := synCMDefaultNS.Synthesize(noopRep)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "configmap_noop.yaml.golden", artNoop.Content)
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
