package promrw

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
)

type rawRelabelRule struct {
	SourceLabels []string `yaml:"source_labels"`
	Regex        string   `yaml:"regex"`
	Action       string   `yaml:"action"`
}

type rawOperatorRelabelRule struct {
	SourceLabels []string `yaml:"sourceLabels"`
	Regex        string   `yaml:"regex"`
	Action       string   `yaml:"action"`
}

func toPromRelabelConfigs(t *testing.T, rules []rawRelabelRule) []*relabel.Config {
	t.Helper()
	cfgs := make([]*relabel.Config, 0, len(rules))
	for _, r := range rules {
		re, err := relabel.NewRegexp(r.Regex)
		if err != nil {
			t.Fatalf("compile relabel regex %q: %v", r.Regex, err)
		}
		srcLabels := make(model.LabelNames, len(r.SourceLabels))
		for i, l := range r.SourceLabels {
			srcLabels[i] = model.LabelName(l)
		}
		cfg := relabel.DefaultRelabelConfig
		cfg.SourceLabels = srcLabels
		cfg.Regex = re
		cfg.Action = relabel.Action(r.Action)
		if err := cfg.Validate(model.UTF8Validation); err != nil {
			t.Fatalf("validate relabel config (%+v): %v", r, err)
		}
		cfgs = append(cfgs, &cfg)
	}
	return cfgs
}

func assertKeep(t *testing.T, cfgs []*relabel.Config, metricName string, wantKeep bool) {
	t.Helper()
	lb := labels.NewBuilder(labels.FromStrings("__name__", metricName, "job", "app"))
	got := relabel.ProcessBuilder(lb, cfgs...)
	if got != wantKeep {
		t.Fatalf("relabel.ProcessBuilder(%q) = %v, want %v", metricName, got, wantKeep)
	}
}

func TestSynthesizePromRWGoldenAndExecution(t *testing.T) {
	auditTime := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	syn := New()

	if syn.Name() != "promrw" {
		t.Fatalf("Name() = %q, want %q", syn.Name(), "promrw")
	}
	caps := syn.Capabilities()
	if !caps.CanDropFamily || !caps.CanDropHistogramBucketsOnly {
		t.Fatalf("unexpected promrw capabilities: %+v", caps)
	}

	normalReport := &metrics.AuditReport{
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

	t.Run("normal family drops (relabel_configs)", func(t *testing.T) {
		art, err := syn.Synthesize(normalReport)
		if err != nil {
			t.Fatal(err)
		}
		if art.Filename != "trimtab-remote-write.yaml" || art.DroppedFamilies != 2 || art.DroppedSeries != 57868 {
			t.Fatalf("unexpected artifact metadata: %+v", art)
		}
		assertGolden(t, "normal.promrw.yaml.golden", art.Content)

		var doc struct {
			WriteRelabelConfigs []rawRelabelRule `yaml:"write_relabel_configs"`
		}
		if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
			t.Fatalf("unmarshal YAML: %v", err)
		}
		cfgs := toPromRelabelConfigs(t, doc.WriteRelabelConfigs)
		assertKeep(t, cfgs, "checkout_legacy_cart_gauge", false)
		assertKeep(t, cfgs, "orders_deprecated_latency_seconds_bucket", false)
		assertKeep(t, cfgs, "orders_deprecated_latency_seconds_sum", false)
		assertKeep(t, cfgs, "orders_deprecated_latency_seconds_count", false)
		assertKeep(t, cfgs, "orders_deprecated_latency_seconds_created", false)
		assertKeep(t, cfgs, "checkout_legacy_cart_gauge_v2", true)
		assertKeep(t, cfgs, "up", true)
		assertKeep(t, cfgs, "payments_http_duration_seconds_bucket", true)
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
		assertGolden(t, "noop.promrw.yaml.golden", art.Content)

		var doc struct {
			WriteRelabelConfigs []rawRelabelRule `yaml:"write_relabel_configs"`
		}
		if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
			t.Fatalf("unmarshal YAML: %v", err)
		}
		if len(doc.WriteRelabelConfigs) != 0 {
			t.Fatalf("expected 0 relabel rules in pass-through mode, got %d", len(doc.WriteRelabelConfigs))
		}
		assertKeep(t, nil, "up", true)
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
		assertGolden(t, "bucket_only.promrw.yaml.golden", art.Content)

		var doc struct {
			WriteRelabelConfigs []rawRelabelRule `yaml:"write_relabel_configs"`
		}
		if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
			t.Fatalf("unmarshal YAML: %v", err)
		}
		cfgs := toPromRelabelConfigs(t, doc.WriteRelabelConfigs)
		assertKeep(t, cfgs, "api_request_duration_seconds_bucket", false)
		assertKeep(t, cfgs, "api_request_duration_seconds_sum", true)
		assertKeep(t, cfgs, "api_request_duration_seconds_count", true)
	})

	t.Run("remote_write mode", func(t *testing.T) {
		rwSyn := New(Options{
			Mode:            ModeRemoteWrite,
			RemoteWriteName: "prod-mimir",
			RemoteWriteURL:  "https://mimir.example.com/api/v1/push",
		})
		art, err := rwSyn.Synthesize(normalReport)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "remote_write.promrw.yaml.golden", art.Content)

		var doc struct {
			RemoteWrite []struct {
				Name                string           `yaml:"name"`
				URL                 string           `yaml:"url"`
				WriteRelabelConfigs []rawRelabelRule `yaml:"write_relabel_configs"`
			} `yaml:"remote_write"`
		}
		if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
			t.Fatalf("unmarshal remote_write YAML: %v", err)
		}
		if len(doc.RemoteWrite) != 1 || doc.RemoteWrite[0].Name != "prod-mimir" {
			t.Fatalf("unexpected remote_write doc: %+v", doc)
		}
		cfgs := toPromRelabelConfigs(t, doc.RemoteWrite[0].WriteRelabelConfigs)
		assertKeep(t, cfgs, "checkout_legacy_cart_gauge", false)
		assertKeep(t, cfgs, "up", true)
	})

	t.Run("prometheus_operator_patch mode", func(t *testing.T) {
		opSyn := New(Options{
			Mode:              ModePrometheusOperatorPatch,
			RemoteWriteName:   "prod-mimir",
			RemoteWriteURL:    "https://mimir.example.com/api/v1/push",
			OperatorKind:      "Prometheus",
			OperatorName:      "k8s",
			OperatorNamespace: "monitoring",
		})
		art, err := opSyn.Synthesize(normalReport)
		if err != nil {
			t.Fatal(err)
		}
		if art.Filename != "trimtab-prometheus-operator-patch.yaml" {
			t.Fatalf("unexpected filename %q", art.Filename)
		}
		assertGolden(t, "operator_patch.promrw.yaml.golden", art.Content)

		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec struct {
				RemoteWrite []struct {
					Name                string                   `yaml:"name"`
					URL                 string                   `yaml:"url"`
					WriteRelabelConfigs []rawOperatorRelabelRule `yaml:"writeRelabelConfigs"`
				} `yaml:"remoteWrite"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
			t.Fatalf("unmarshal operator patch YAML: %v", err)
		}
		if doc.APIVersion != "monitoring.coreos.com/v1" || doc.Kind != "Prometheus" ||
			doc.Metadata.Name != "k8s" || doc.Metadata.Namespace != "monitoring" ||
			len(doc.Spec.RemoteWrite) != 1 {
			t.Fatalf("unexpected operator patch structure: %+v", doc)
		}
		rules := make([]rawRelabelRule, 0, len(doc.Spec.RemoteWrite[0].WriteRelabelConfigs))
		for _, r := range doc.Spec.RemoteWrite[0].WriteRelabelConfigs {
			rules = append(rules, rawRelabelRule(r))
		}
		cfgs := toPromRelabelConfigs(t, rules)
		assertKeep(t, cfgs, "checkout_legacy_cart_gauge", false)
		assertKeep(t, cfgs, "orders_deprecated_latency_seconds_bucket", false)
		assertKeep(t, cfgs, "up", true)
	})
}

func TestSynthesizePromRW_SummaryNativeUTF8AndChunking(t *testing.T) {
	syn := New()
	rep := &metrics.AuditReport{
		GeneratedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		Summary:     metrics.AuditSummary{ActiveTranche: 1, TotalTranches: 1, SynthesizedPercent: 5.0},
		Safety:      metrics.SafetyGateStatus{QueryLogCoveredDays: 30, RequiredWindowDays: 30},
		ActiveDrops: []metrics.Recommendation{
			{
				FamilyName: "summary_metric",
				MetricType: metrics.MetricTypeSummary,
				Action:     metrics.ActionDropFamily,
				Series:     300,
			},
			{
				FamilyName: "native_hist_metric",
				MetricType: metrics.MetricTypeNativeHistogram,
				Action:     metrics.ActionDropFamily,
				Series:     200,
			},
			{
				FamilyName: "http.server.legacy.duration",
				MetricType: metrics.MetricTypeGauge,
				Action:     metrics.ActionDropFamily,
				Series:     150,
			},
			{
				FamilyName:   "advisory_only_skip",
				MetricType:   metrics.MetricTypeGauge,
				Action:       metrics.ActionDropFamily,
				Series:       999,
				AdvisoryOnly: true,
			},
		},
	}

	art, err := syn.Synthesize(rep)
	if err != nil {
		t.Fatal(err)
	}
	if art.DroppedFamilies != 3 || art.DroppedSeries != 650 {
		t.Fatalf("unexpected drop stats (advisory should be skipped): %+v", art)
	}

	var doc struct {
		WriteRelabelConfigs []rawRelabelRule `yaml:"write_relabel_configs"`
	}
	if err := yaml.Unmarshal([]byte(art.Content), &doc); err != nil {
		t.Fatalf("unmarshal YAML: %v\n%s", err, art.Content)
	}
	cfgs := toPromRelabelConfigs(t, doc.WriteRelabelConfigs)

	// Summary family drops base + _sum + _count + _created without touching _other
	assertKeep(t, cfgs, "summary_metric", false)
	assertKeep(t, cfgs, "summary_metric_sum", false)
	assertKeep(t, cfgs, "summary_metric_count", false)
	assertKeep(t, cfgs, "summary_metric_created", false)
	assertKeep(t, cfgs, "summary_metric_other", true)

	// Native histogram
	assertKeep(t, cfgs, "native_hist_metric", false)

	// UTF-8 metric name with dots escaped accurately via regexp.QuoteMeta + YAML double-quoting
	assertKeep(t, cfgs, "http.server.legacy.duration", false)
	assertKeep(t, cfgs, "httpXserverXlegacyXduration", true)

	// AdvisoryOnly skipped
	assertKeep(t, cfgs, "advisory_only_skip", true)

	// Multi-rule chunking test (85 families with default MaxTermsPerRule=40 -> 3 rules)
	var drops85 []metrics.Recommendation
	for i := range 85 {
		drops85 = append(drops85, metrics.Recommendation{
			FamilyName: fmt.Sprintf("unused_metric_%03d", i),
			MetricType: metrics.MetricTypeCounter,
			Action:     metrics.ActionDropFamily,
			Series:     100 - i,
		})
	}
	art85, err := syn.Synthesize(&metrics.AuditReport{
		GeneratedAt: rep.GeneratedAt,
		Summary:     rep.Summary,
		Safety:      rep.Safety,
		ActiveDrops: drops85,
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.WriteRelabelConfigs = nil
	if err := yaml.Unmarshal([]byte(art85.Content), &doc); err != nil {
		t.Fatalf("unmarshal 85-family YAML: %v", err)
	}
	if len(doc.WriteRelabelConfigs) != 3 {
		t.Fatalf("expected 3 chunked relabel rules for 85 metrics, got %d", len(doc.WriteRelabelConfigs))
	}
	cfgs85 := toPromRelabelConfigs(t, doc.WriteRelabelConfigs)
	for i := range 85 {
		assertKeep(t, cfgs85, fmt.Sprintf("unused_metric_%03d", i), false)
	}
	assertKeep(t, cfgs85, "unrelated_live_metric", true)
}

func TestSynthesizePromRW_PassThroughAllModesAndErrors(t *testing.T) {
	noopReport := &metrics.AuditReport{
		GeneratedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		Safety: metrics.SafetyGateStatus{
			ReportOnly:          true,
			QueryLogCoveredDays: 0,
			RequiredWindowDays:  30,
		},
	}

	// Pass-through for remote_write mode
	rwArt, err := New(Options{
		Mode:            ModeRemoteWrite,
		RemoteWriteName: "prod-mimir",
	}).Synthesize(noopReport)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rwArt.Content, "write_relabel_configs: []") {
		t.Fatalf("expected empty write_relabel_configs in remote_write pass-through:\n%s", rwArt.Content)
	}

	// Pass-through for operator patch mode (PrometheusAgent, no namespace)
	opArt, err := New(Options{
		Mode:           ModePrometheusOperatorPatch,
		OperatorKind:   "PrometheusAgent",
		OperatorName:   "edge-agent",
		RemoteWriteURL: "https://mimir.example.com/api/v1/push",
	}).Synthesize(noopReport)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opArt.Content, "kind: PrometheusAgent") || !strings.Contains(opArt.Content, "writeRelabelConfigs: []") {
		t.Fatalf("unexpected PrometheusAgent pass-through output:\n%s", opArt.Content)
	}

	// Error cases
	if _, err := New().Synthesize(nil); err == nil {
		t.Fatal("expected error for nil AuditReport")
	}
	if _, err := New(Options{Mode: "invalid"}).Synthesize(noopReport); err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if _, err := New(Options{Mode: ModeRemoteWrite}).Synthesize(noopReport); err == nil {
		t.Fatal("expected error for ModeRemoteWrite missing URL and Name")
	}
	if _, err := New(Options{Mode: ModePrometheusOperatorPatch}).Synthesize(noopReport); err == nil {
		t.Fatal("expected error for ModePrometheusOperatorPatch missing RemoteWriteURL")
	}
	if _, err := New(Options{
		Mode:           ModePrometheusOperatorPatch,
		OperatorKind:   "Deployment",
		RemoteWriteURL: "https://example.com/push",
	}).Synthesize(noopReport); err == nil {
		t.Fatal("expected error for invalid OperatorKind")
	}
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
