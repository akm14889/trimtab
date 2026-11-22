package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/inventory/prometheus"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
	"trimtab.io/trimtab/pkg/usage/grafana"
	"trimtab.io/trimtab/pkg/usage/promql"
	"trimtab.io/trimtab/pkg/usage/promrules"
	"trimtab.io/trimtab/pkg/usage/querylog"
)

func alloyCaps() metrics.Capabilities {
	return metrics.Capabilities{
		CanDropFamily:               true,
		CanDropHistogramBucketsOnly: true,
	}
}

func otelCaps() metrics.Capabilities {
	return metrics.Capabilities{
		CanDropFamily:               true,
		CanDropHistogramBucketsOnly: false,
		SupportedNamingModes: []metrics.NamingMode{
			metrics.NamingPrometheusReceiver,
			metrics.NamingUTF8NoTranslation,
			metrics.NamingExplicitOTLP,
		},
	}
}

func fullCoverageQueryLog(now time.Time, entries ...querylog.QueryLogEntry) *querylog.Result {
	res := &querylog.Result{
		Configured:        true,
		EarliestTimestamp: now.Add(-31 * 24 * time.Hour),
		LatestTimestamp:   now,
	}
	for _, e := range entries {
		if len(e.Accesses) == 0 && e.Query != "" {
			if ex, err := promql.Extract(e.Query); err == nil {
				e.Accesses = ex.Accesses
			}
		}
		res.Entries = append(res.Entries, e)
		res.TotalEntries++
		switch e.Origin {
		case metrics.OriginAPIUser:
			res.APIUserCount++
		case metrics.OriginRuleEval:
			res.RuleEvalCount++
		case metrics.OriginExcludedExplorer:
			res.ExcludedCount++
		}
		if e.Origin != metrics.OriginExcludedExplorer {
			for _, acc := range e.Accesses {
				res.Refs = append(res.Refs, metrics.UsageRef{
					MetricName:     acc.MetricName,
					MetricRegex:    acc.MetricRegex,
					IsRegex:        acc.IsRegex,
					Origin:         e.Origin,
					SourceID:       "querylog:" + string(e.Origin),
					SourceName:     "querylog",
					Query:          e.Query,
					Timestamp:      e.Timestamp,
					ExplicitLabels: acc.ExplicitLabels,
					PreservesAll:   acc.PreservesAll,
					WithoutLabels:  acc.WithoutLabels,
				})
			}
		}
	}
	return res
}

// Mandatory Safety Test #1:
// Missing query log OR query_log_coverage < minObservationWindow => 0 synthesized drops (report-only with projected savings).
func TestSafetyMatrix_01_MissingOrShortQueryLogReportOnly(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"
	old := now.Add(-45 * 24 * time.Hour)

	inv := &prometheus.InventorySnapshot{
		MetricNames:  []string{"used_metric", "unused_legacy_metric"},
		SeriesCounts: map[string]int{"used_metric": 9000, "unused_legacy_metric": 1000},
		FirstSeenHints: map[string]time.Time{
			"used_metric":          old,
			"unused_legacy_metric": old,
		},
	}

	t.Run("missing query log", func(t *testing.T) {
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config:       cfg,
			Ledger:       state.NewLedger(),
			Inventory:    inv,
			QueryLog:     &querylog.Result{Configured: false},
			Capabilities: alloyCaps(),
			Now:          now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Safety.ReportOnly {
			t.Fatal("expected ReportOnly = true when query log is missing")
		}
		if len(rep.ActiveDrops) != 0 || rep.Summary.SynthesizedFamilies != 0 {
			t.Fatalf("expected 0 synthesized drops, got %d", len(rep.ActiveDrops))
		}
		if len(rep.ProjectedDrops) == 0 {
			t.Fatal("expected projected drops to still be reported in Report-Only mode")
		}
		if rep.Summary.Confidence != metrics.ConfidenceMedium {
			t.Errorf("expected MEDIUM confidence, got %s", rep.Summary.Confidence)
		}
	})

	t.Run("query log coverage 5/30 days", func(t *testing.T) {
		shortQL := &querylog.Result{
			Configured:        true,
			EarliestTimestamp: now.Add(-5 * 24 * time.Hour),
			LatestTimestamp:   now,
			TotalEntries:      10,
			APIUserCount:      10,
		}
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config:       cfg,
			Ledger:       state.NewLedger(),
			Inventory:    inv,
			QueryLog:     shortQL,
			Capabilities: alloyCaps(),
			Now:          now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Safety.ReportOnly || len(rep.ActiveDrops) != 0 {
			t.Fatalf("expected ReportOnly with 0 active drops for 5/30d coverage, got reportOnly=%v drops=%d", rep.Safety.ReportOnly, len(rep.ActiveDrops))
		}
		if rep.Safety.QueryLogCoveredDays < 4.9 || rep.Safety.QueryLogCoveredDays > 5.1 {
			t.Errorf("QueryLogCoveredDays = %v, want 5.0", rep.Safety.QueryLogCoveredDays)
		}
	})
}

// Mandatory Safety Test #2:
// rule-eval entries in query log do NOT mark recording-rule inputs as live-used; api-user entries and alerting rules DO.
func TestSafetyMatrix_02_RuleEvalDoesNotMarkLive(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{"only_rule_eval_metric", "api_user_metric", "alerted_metric", "filler_used"},
		SeriesCounts: map[string]int{
			"only_rule_eval_metric": 100,
			"api_user_metric":       300,
			"alerted_metric":        300,
			"filler_used":           300,
		},
		FirstSeenHints: map[string]time.Time{
			"only_rule_eval_metric": old,
			"api_user_metric":       old,
			"alerted_metric":        old,
			"filler_used":           old,
		},
	}

	ql := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "rate(only_rule_eval_metric[5m])", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginRuleEval, RuleGroup: "dead_grp"},
		querylog.QueryLogEntry{Query: "rate(api_user_metric[5m])", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "filler_used", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
	)
	promAlerts := &promrules.ScanResult{
		AlertRuleCount: 1,
		TotalRuleCount: 1,
		AlertRefs: []metrics.UsageRef{
			{MetricName: "alerted_metric", Origin: metrics.OriginPromAlert, SourceID: "prom-alert:a", SourceName: "HighAlert"},
		},
	}

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:       cfg,
		Ledger:       state.NewLedger(),
		Inventory:    inv,
		QueryLog:     ql,
		PromRules:    promAlerts,
		Capabilities: alloyCaps(),
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ActiveDrops) != 1 || rep.ActiveDrops[0].FamilyName != "only_rule_eval_metric" {
		t.Fatalf("expected ONLY only_rule_eval_metric to be dropped, got %+v", rep.ActiveDrops)
	}
}

// Mandatory Safety Test #3:
// Large droppable estate (47%) => split into tranches <= 15%, sorted descending by series savings,
// never splitting a histogram family; >80% => sanity-abort.
func TestSafetyMatrix_03_TranchesAndSanityCeiling(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	t.Run("47% droppable splits into <=15% tranches without splitting histogram", func(t *testing.T) {
		// Total = 100,000 series. Live = 53,000. Droppable = 47,000 (47%).
		// Hist family A = 12,000 (bucket 10,000 + sum 1,000 + count 1,000) -> must stay intact!
		// Gauge B = 11,000, Gauge C = 10,000, Gauge D = 8,000, Gauge E = 4,000, Gauge F = 2,000.
		inv := &prometheus.InventorySnapshot{
			MetricNames: []string{
				"live_core",
				"dead_hist_bucket", "dead_hist_sum", "dead_hist_count",
				"dead_b", "dead_c", "dead_d", "dead_e", "dead_f",
			},
			Metadata: map[string]prometheus.MetricMetadata{
				"dead_hist": {Type: "histogram"},
			},
			SeriesCounts: map[string]int{
				"live_core":        53000,
				"dead_hist_bucket": 10000,
				"dead_hist_sum":    1000,
				"dead_hist_count":  1000,
				"dead_b":           11000,
				"dead_c":           10000,
				"dead_d":           8000,
				"dead_e":           4000,
				"dead_f":           2000,
			},
			FirstSeenHints: map[string]time.Time{
				"live_core": old, "dead_hist": old, "dead_b": old, "dead_c": old, "dead_d": old, "dead_e": old, "dead_f": old,
			},
		}
		ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "live_core", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config:       cfg,
			Ledger:       state.NewLedger(),
			Inventory:    inv,
			QueryLog:     ql,
			Capabilities: alloyCaps(),
			Now:          now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Summary.DroppablePercent != 47.0 {
			t.Fatalf("DroppablePercent = %.1f, want 47.0", rep.Summary.DroppablePercent)
		}
		if len(rep.Tranches) < 4 {
			t.Fatalf("expected >=4 tranches for 47%% at 15%% cap, got %d", len(rep.Tranches))
		}
		for _, tr := range rep.Tranches {
			if tr.PercentEstate > 15.0 {
				t.Errorf("tranche %d exceeded 15%% cap: %.1f%% (%d series)", tr.Number, tr.PercentEstate, tr.Series)
			}
		}
		// Tranche 1 starts with largest family (dead_hist = 12,000 intact + dead_f = 2,000 => 14,000 = 14%)
		if rep.ActiveDrops[0].FamilyName != "dead_hist" || rep.ActiveDrops[0].Series != 12000 {
			t.Fatalf("expected dead_hist (12,000 intact) first in Tranche 1, got %+v", rep.ActiveDrops[0])
		}
		if len(rep.ActiveDrops[0].Members) != 3 {
			t.Fatalf("histogram family was split: members=%v", rep.ActiveDrops[0].Members)
		}
	})

	t.Run(">80% droppable triggers sanity-abort ceiling", func(t *testing.T) {
		inv := &prometheus.InventorySnapshot{
			MetricNames:  []string{"live_tiny", "dead_huge_1", "dead_huge_2"},
			SeriesCounts: map[string]int{"live_tiny": 1000, "dead_huge_1": 5000, "dead_huge_2": 4000}, // 90% droppable
			FirstSeenHints: map[string]time.Time{
				"live_tiny": old, "dead_huge_1": old, "dead_huge_2": old,
			},
		}
		ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "live_tiny", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config:       cfg,
			Ledger:       state.NewLedger(),
			Inventory:    inv,
			QueryLog:     ql,
			Capabilities: alloyCaps(),
			Now:          now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Safety.SanityCeilingTripped || !rep.Safety.Blocked {
			t.Fatalf("expected sanity ceiling to trip & block synthesis, got %+v", rep.Safety)
		}
		if len(rep.ActiveDrops) != 0 {
			t.Fatalf("expected 0 synthesized drops on sanity abort, got %d", len(rep.ActiveDrops))
		}
	})
}

// Mandatory Safety Test #4:
// Simulated Grafana partial response (100 -> 20 dashboards) => synthesis aborted by baseline-drift guard.
func TestSafetyMatrix_04_BaselineDriftGuard(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"
	cfg.Usage.Grafana = &config.GrafanaConfig{URL: "http://grafana.local"}

	ledger := state.NewLedger()
	ledger.BaselineCounts.Dashboards = state.IntPtr(100)

	inv := &prometheus.InventorySnapshot{
		MetricNames:    []string{"live_m", "dead_m"},
		SeriesCounts:   map[string]int{"live_m": 900, "dead_m": 100},
		FirstSeenHints: map[string]time.Time{"live_m": old, "dead_m": old},
	}
	ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "live_m", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})
	partialGrafana := &grafana.ScanResult{DashboardCount: 20}

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:       cfg,
		Ledger:       ledger,
		Inventory:    inv,
		QueryLog:     ql,
		Grafana:      partialGrafana,
		Capabilities: alloyCaps(),
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safety.BaselineDriftDetected || !rep.Safety.Blocked {
		t.Fatalf("expected BaselineDriftDetected=true and Blocked=true, got %+v", rep.Safety)
	}
	if len(rep.ActiveDrops) != 0 {
		t.Fatalf("expected 0 active drops on baseline drift, got %d", len(rep.ActiveDrops))
	}
	// Baseline must NOT be overwritten when blocked
	if *ledger.BaselineCounts.Dashboards != 100 {
		t.Fatalf("baseline should stay 100 when drift blocked, got %d", *ledger.BaselineCounts.Dashboards)
	}

	// Passing AcceptBaselineShift allows proceeding and updates baseline
	rep2, err := Analyze(context.Background(), AnalyzeInput{
		Config:              cfg,
		Ledger:              ledger,
		Inventory:           inv,
		QueryLog:            ql,
		Grafana:             partialGrafana,
		Capabilities:        alloyCaps(),
		Now:                 now,
		AcceptBaselineShift: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Safety.Blocked || len(rep2.ActiveDrops) != 1 {
		t.Fatalf("expected AcceptBaselineShift to unblock synthesis, got blocked=%v drops=%d", rep2.Safety.Blocked, len(rep2.ActiveDrops))
	}
	if *ledger.BaselineCounts.Dashboards != 20 {
		t.Fatalf("baseline should update to 20 after AcceptBaselineShift, got %d", *ledger.BaselineCounts.Dashboards)
	}
}

// Mandatory Safety Test #5:
// Metric family seen < minObservationWindow => protected from drop.
func TestSafetyMatrix_05_ObservationWindowProtection(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames:  []string{"live_m", "new_cron_job_metric", "old_unused_metric"},
		SeriesCounts: map[string]int{"live_m": 800, "new_cron_job_metric": 100, "old_unused_metric": 100},
		FirstSeenHints: map[string]time.Time{
			"live_m":              now.Add(-45 * 24 * time.Hour),
			"new_cron_job_metric": now.Add(-5 * 24 * time.Hour), // < 30d!
			"old_unused_metric":   now.Add(-45 * 24 * time.Hour),
		},
	}
	ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "live_m", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:       cfg,
		Ledger:       state.NewLedger(),
		Inventory:    inv,
		QueryLog:     ql,
		Capabilities: alloyCaps(),
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ActiveDrops) != 1 || rep.ActiveDrops[0].FamilyName != "old_unused_metric" {
		t.Fatalf("expected only old_unused_metric dropped, got %+v", rep.ActiveDrops)
	}
	if reason := rep.ProtectedReasons["new_cron_job_metric"]; !strings.Contains(reason, "minObservationWindow") {
		t.Errorf("expected new_cron_job_metric protected by minObservationWindow, got %q", reason)
	}
}

// Mandatory Safety Test #6:
// Default neverDrop (up, ALERTS, ALERTS_FOR_STATE, scrape_*, prometheus_*, alloy_*, otelcol_*) => always protected.
func TestSafetyMatrix_06_DefaultNeverDropProtected(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	metaMetrics := []string{
		"up", "ALERTS", "ALERTS_FOR_STATE",
		"scrape_duration_seconds", "prometheus_tsdb_head_series",
		"alloy_component_controller_running_components", "otelcol_process_uptime",
	}
	inv := &prometheus.InventorySnapshot{
		MetricNames:    append([]string{"live_m", "droppable_m"}, metaMetrics...),
		SeriesCounts:   map[string]int{"live_m": 8000, "droppable_m": 500},
		FirstSeenHints: map[string]time.Time{"live_m": old, "droppable_m": old},
	}
	for _, m := range metaMetrics {
		inv.SeriesCounts[m] = 100
		inv.FirstSeenHints[m] = old
	}
	ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "live_m", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:       cfg,
		Ledger:       state.NewLedger(),
		Inventory:    inv,
		QueryLog:     ql,
		Capabilities: alloyCaps(),
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ActiveDrops) != 1 || rep.ActiveDrops[0].FamilyName != "droppable_m" {
		t.Fatalf("expected only droppable_m dropped (all meta-monitoring protected), got %+v", rep.ActiveDrops)
	}
	for _, m := range metaMetrics {
		if !strings.Contains(rep.ProtectedReasons[m], "neverDrop") {
			t.Errorf("expected %s to be protected by neverDrop, got %q", m, rep.ProtectedReasons[m])
		}
	}
}

// Mandatory Safety Test #7:
// Dead recording-rule chain (M -> R1 -> R2) => M dropped at collector + R1/R2 emitted in Dead Recording Rules advisory;
// Live chain => all protected.
func TestSafetyMatrix_07_RecordingRuleDAGDeadAndLiveChains(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"dead_src_m", "dead:r1", "dead:r2",
			"live_src_m", "live:r1", "live:r2",
		},
		SeriesCounts: map[string]int{
			"dead_src_m": 500, "dead:r1": 100, "dead:r2": 50,
			"live_src_m": 4000, "live:r1": 500, "live:r2": 100,
		},
		FirstSeenHints: map[string]time.Time{
			"dead_src_m": old, "dead:r1": old, "dead:r2": old,
			"live_src_m": old, "live:r1": old, "live:r2": old,
		},
	}

	mustExtract := func(e string) []promql.MetricAccess {
		r, err := promql.Extract(e)
		if err != nil {
			t.Fatal(err)
		}
		return r.Accesses
	}
	rules := &promrules.ScanResult{
		RecordingRuleCount: 4,
		TotalRuleCount:     4,
		RecordingRules: []promrules.RecordingRule{
			{Name: "dead:r1", File: "rules/dead.yaml", Group: "dead_grp", Expr: "rate(dead_src_m[5m]) or vector(0)", Inputs: mustExtract("rate(dead_src_m[5m]) or vector(0)"), HasOrVectorZero: true},
			{Name: "dead:r2", File: "rules/dead.yaml", Group: "dead_grp", Expr: "sum(dead:r1)", Inputs: mustExtract("sum(dead:r1)")},
			{Name: "live:r1", File: "rules/live.yaml", Group: "live_grp", Expr: "rate(live_src_m[5m])", Inputs: mustExtract("rate(live_src_m[5m])")},
			{Name: "live:r2", File: "rules/live.yaml", Group: "live_grp", Expr: "sum(live:r1)", Inputs: mustExtract("sum(live:r1)")},
		},
	}
	// Only live:r2 has an api-user consumer; dead chain has rule-evals only!
	ql := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "live:r2", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "rate(dead_src_m[5m])", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginRuleEval},
		querylog.QueryLogEntry{Query: "sum(dead:r1)", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginRuleEval},
	)

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:       cfg,
		Ledger:       state.NewLedger(),
		Inventory:    inv,
		QueryLog:     ql,
		PromRules:    rules,
		Capabilities: alloyCaps(),
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// dead_src_m MUST be dropped at collector; live_src_m, live:r1, live:r2 MUST be protected.
	if len(rep.ActiveDrops) != 1 || rep.ActiveDrops[0].FamilyName != "dead_src_m" {
		t.Fatalf("expected only dead_src_m dropped at collector, got %+v", rep.ActiveDrops)
	}
	if !strings.Contains(rep.ActiveDrops[0].Reason, "dead recording-rule chain") {
		t.Errorf("expected dead recording-rule chain reason, got %q", rep.ActiveDrops[0].Reason)
	}
	if len(rep.DeadRecordingRules) != 2 {
		t.Fatalf("expected 2 dead recording rule advisories (dead:r1, dead:r2), got %+v", rep.DeadRecordingRules)
	}
	for _, liveName := range []string{"live_src_m", "live:r1", "live:r2"} {
		if _, protected := rep.ProtectedReasons[liveName]; !protected {
			t.Errorf("expected live chain metric %s to be protected", liveName)
		}
	}
}

// Mandatory Safety Test #8 & #9:
// - sum without (pod), bare rate(x[5m]), label_replace, Grafana legendFormat, alert {{ $labels.x }} => labels marked used.
// - le and quantile => never suggested in advisory label drops; single-label without (pod_uid) honestly shows 0% collapse when pod remains.
func TestSafetyMatrix_08_09_LabelSemanticsAndHonestWithoutCollapse(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"bare_metric",
			"without_metric",
			"payments_http_duration_seconds_bucket",
			"payments_http_duration_seconds_sum",
			"payments_http_duration_seconds_count",
		},
		Metadata: map[string]prometheus.MetricMetadata{
			"payments_http_duration_seconds": {Type: "histogram"},
		},
		SeriesCounts: map[string]int{
			"bare_metric":                           100,
			"without_metric":                        100,
			"payments_http_duration_seconds_bucket": 8,
			"payments_http_duration_seconds_sum":    2,
			"payments_http_duration_seconds_count":  2,
		},
		MetricLabels: map[string][]string{
			"bare_metric":                           {"service", "pod", "instance"},
			"without_metric":                        {"service", "pod", "env"},
			"payments_http_duration_seconds_bucket": {"__name__", "le", "quantile", "service", "status", "cluster", "pod", "pod_uid", "instance"},
		},
		SampleLabelSets: map[string][]map[string]string{
			"bare_metric":    {{"service": "a", "pod": "p1", "instance": "i1"}},
			"without_metric": {{"service": "a", "pod": "p1", "env": "prod"}, {"service": "a", "pod": "p2", "env": "prod"}},
			"payments_http_duration_seconds_bucket": {
				{"service": "pay", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "p1", "pod_uid": "u1", "instance": "i1"},
				{"service": "pay", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "p2", "pod_uid": "u2", "instance": "i2"},
				{"service": "pay", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "p3", "pod_uid": "u3", "instance": "i3"},
				{"service": "pay", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "p4", "pod_uid": "u4", "instance": "i4"},
				{"service": "chk", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "c1", "pod_uid": "u5", "instance": "i5"},
				{"service": "chk", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "c2", "pod_uid": "u6", "instance": "i6"},
				{"service": "chk", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "c3", "pod_uid": "u7", "instance": "i7"},
				{"service": "chk", "status": "200", "cluster": "c1", "le": "0.1", "quantile": "0.9", "pod": "c4", "pod_uid": "u8", "instance": "i8"},
			},
		},
		FirstSeenHints: map[string]time.Time{
			"bare_metric": old, "without_metric": old, "payments_http_duration_seconds": old,
		},
	}

	ql := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "rate(bare_metric[5m])", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "sum without (pod) (without_metric)", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{
			Query:     `histogram_quantile(0.95, sum by (le, service) (label_replace(rate(payments_http_duration_seconds_bucket[5m]), "svc", "$1", "service", "(.*)")))`,
			Timestamp: now.Add(-time.Hour),
			Origin:    metrics.OriginAPIUser,
		},
	)
	// Grafana legendFormat marks `status` used; alert template marks `cluster` used.
	graf := &grafana.ScanResult{
		DashboardCount: 1,
		Refs: []metrics.UsageRef{
			{MetricName: "payments_http_duration_seconds_bucket", Origin: metrics.OriginGrafanaDashboard, SourceID: "dash:1", ExplicitLabels: []string{"status"}},
		},
	}
	alerts := &promrules.ScanResult{
		AlertRuleCount: 1,
		TotalRuleCount: 1,
		AlertRefs: []metrics.UsageRef{
			{MetricName: "payments_http_duration_seconds_bucket", Origin: metrics.OriginPromAlert, SourceID: "alert:1", ExplicitLabels: []string{"cluster"}},
		},
	}

	fixEval := prometheus.NewFixtureCollapseEvaluator(inv)

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:            cfg,
		Ledger:            state.NewLedger(),
		Inventory:         inv,
		CollapseEvaluator: fixEval,
		QueryLog:          ql,
		Grafana:           graf,
		PromRules:         alerts,
		Capabilities:      alloyCaps(),
		Now:               now,
	})
	if err != nil {
		t.Fatal(err)
	}

	byFam := map[string]metrics.LabelCollapseAdvisory{}
	for _, adv := range rep.LabelCollapseAdvisories {
		byFam[adv.FamilyName] = adv
	}

	// bare_metric preserves all labels -> 0 unused labels -> NO advisory!
	if _, exists := byFam["bare_metric"]; exists {
		t.Fatalf("bare_metric preserves all labels and should have no unused label advisory")
	}

	// without_metric used `sum without (pod)` -> ONLY `pod` is unused (service & env preserved!).
	wm, ok := byFam["without_metric"]
	if !ok || len(wm.UnusedLabels) != 1 || wm.UnusedLabels[0] != "pod" {
		t.Fatalf("without_metric unused labels = %v, want [pod]", wm.UnusedLabels)
	}

	// payments_http_duration_seconds: le, quantile, __name__ NEVER unused; service/status/cluster used;
	// only [instance, pod, pod_uid] unused; single-label pod_uid shows 0.0% collapse!
	pm, ok := byFam["payments_http_duration_seconds"]
	if !ok {
		t.Fatalf("missing advisory for payments_http_duration_seconds: %+v", rep.LabelCollapseAdvisories)
	}
	for _, lbl := range pm.UnusedLabels {
		if lbl == "le" || lbl == "quantile" || lbl == "__name__" || lbl == "service" || lbl == "status" || lbl == "cluster" {
			t.Errorf("protected/used label %q wrongly listed as unused: %v", lbl, pm.UnusedLabels)
		}
	}
	if pm.CombinedEstimate == nil || pm.CombinedEstimate.SavingsPercent != 75.0 {
		t.Errorf("expected combined collapse savings 75.0%%, got %+v", pm.CombinedEstimate)
	}
	foundPodUID := false
	for _, s := range pm.SingleLabelEstimates {
		if len(s.DroppedLabels) == 1 && s.DroppedLabels[0] == "pod_uid" {
			foundPodUID = true
			if s.SavingsPercent != 0.0 {
				t.Errorf("single-label without(pod_uid) should show 0.0%% collapse, got %.1f%%", s.SavingsPercent)
			}
		}
	}
	if !foundPodUID {
		t.Fatal("missing single-label collapse estimate for pod_uid")
	}
}

// Mandatory Safety Test #10:
// api-user query hits a metric in active_drops => RegretEvent emitted, metric removed from drops &
// pinned to neverDrop, next tranche blocked.
func TestSafetyMatrix_10_RegretDetectionAndTrancheBlocking(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	// Ledger already has Tranche 1 active from 10 days ago (soakWindow 7d WOULD have completed if 0 regrets!)
	ledger := state.NewLedger()
	ledger.RecordTrancheActivation(1, now.Add(-10*24*time.Hour), 7*24*time.Hour, []metrics.Recommendation{
		{FamilyName: "dropped_a", Action: metrics.ActionDropFamily, Series: 1000},
		{FamilyName: "dropped_b", Action: metrics.ActionDropFamily, Series: 1000},
	})

	inv := &prometheus.InventorySnapshot{
		MetricNames:  []string{"live_core", "dropped_a", "dropped_b", "next_tranche_c"},
		SeriesCounts: map[string]int{"live_core": 20000, "dropped_a": 1000, "dropped_b": 1000, "next_tranche_c": 1000},
		FirstSeenHints: map[string]time.Time{
			"live_core": old, "dropped_a": old, "dropped_b": old, "next_tranche_c": old,
		},
	}

	// Single on-call SRE runs 1 api-user query on `dropped_a` AFTER it was dropped!
	ql := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "live_core", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "rate(dropped_a[5m])", Timestamp: now.Add(-30 * time.Minute), Origin: metrics.OriginAPIUser},
	)

	rep, err := Analyze(context.Background(), AnalyzeInput{
		Config:       cfg,
		Ledger:       ledger,
		Inventory:    inv,
		QueryLog:     ql,
		Capabilities: alloyCaps(),
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Regrets) != 1 || rep.Regrets[0].FamilyName != "dropped_a" {
		t.Fatalf("expected 1 RegretEvent for dropped_a, got %+v", rep.Regrets)
	}
	if !ledger.IsPinnedNeverDrop("dropped_a") {
		t.Fatal("dropped_a should be pinned to ledger.PinnedNeverDrop")
	}
	// dropped_a MUST NOT be in ActiveDrops; next_tranche_c MUST NOT unlock because regret blocks next tranche!
	for _, d := range rep.ActiveDrops {
		if d.FamilyName == "dropped_a" {
			t.Fatal("regretted metric dropped_a should be removed from ActiveDrops immediately")
		}
		if d.FamilyName == "next_tranche_c" {
			t.Fatal("next tranche should be blocked when unresolved regrets > 0")
		}
	}
	if rep.Safety.UnresolvedRegrets == 0 {
		t.Fatal("expected UnresolvedRegrets > 0")
	}
}

// Capability Negotiation Test (Invariant #7):
// Classic histogram where only _count/_sum are queried => Alloy drops _bucket only; OTel keeps as advisory!
// Native histogram => NEVER recommended for bucket-only drop!
func TestCapabilityNegotiationAndNativeHistogram(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"live_core",
			"classic_hist_bucket", "classic_hist_sum", "classic_hist_count",
			"native_hist",
		},
		Metadata: map[string]prometheus.MetricMetadata{
			"classic_hist": {Type: "histogram"},
			"native_hist":  {Type: "histogram"},
		},
		NativeHistograms: map[string]bool{"native_hist": true},
		SeriesCounts: map[string]int{
			"live_core":           50000,
			"classic_hist_bucket": 5000,
			"classic_hist_sum":    500,
			"classic_hist_count":  500,
			"native_hist":         500,
		},
		FirstSeenHints: map[string]time.Time{
			"live_core": old, "classic_hist": old, "native_hist": old,
		},
	}
	ql := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "live_core", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "rate(classic_hist_count[5m])", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "histogram_count(native_hist)", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
	)

	// Alloy supports CanDropHistogramBucketsOnly
	repAlloy, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: ql,
		Capabilities: alloyCaps(), TargetName: "alloy", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repAlloy.ActiveDrops) != 1 || repAlloy.ActiveDrops[0].Action != metrics.ActionDropHistogramBuckets || repAlloy.ActiveDrops[0].FamilyName != "classic_hist" {
		t.Fatalf("Alloy should synthesize DROP_HISTOGRAM_BUCKETS for classic_hist only, got %+v", repAlloy.ActiveDrops)
	}

	// OTel does NOT support CanDropHistogramBucketsOnly -> advisory only!
	repOTel, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: ql,
		Capabilities: otelCaps(), TargetName: "otel", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repOTel.ActiveDrops) != 0 {
		t.Fatalf("OTel must NOT synthesize bucket-only drop, got %+v", repOTel.ActiveDrops)
	}
	if len(repOTel.CapabilityAdvisories) != 1 || repOTel.CapabilityAdvisories[0].FamilyName != "classic_hist" {
		t.Fatalf("expected 1 CapabilityAdvisory for classic_hist on OTel, got %+v", repOTel.CapabilityAdvisories)
	}
}

// Wildcard Protector & Soak Progression tests (Invariants #3 & #6).
func TestWildcardProtectorAndSoakProgression(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)

	inv := &prometheus.InventorySnapshot{
		MetricNames:  []string{"live_a", "dead_1", "dead_2", "dead_3"},
		SeriesCounts: map[string]int{"live_a": 7000, "dead_1": 1000, "dead_2": 1000, "dead_3": 1000},
		FirstSeenHints: map[string]time.Time{
			"live_a": old, "dead_1": old, "dead_2": old, "dead_3": old,
		},
	}
	ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "live_a", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})
	wildcardGraf := &grafana.ScanResult{
		DashboardCount: 1,
		Refs: []metrics.UsageRef{
			{
				MetricRegex: ".+",
				IsRegex:     true,
				Origin:      metrics.OriginGrafanaDashboard,
				SourceID:    "grafana-dashboard:card-exp",
				SourceName:  "Cardinality Explorer",
			},
		},
	}

	// Without excludeSources: Wildcard dashboard is flagged AND safely protects everything!
	cfgNoEx := config.DefaultConfig()
	cfgNoEx.Inventory.FixturePath = "fixture.json"
	rep1, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfgNoEx, Ledger: state.NewLedger(), Inventory: inv, QueryLog: ql, Grafana: wildcardGraf,
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep1.WildcardSources) != 1 || rep1.WildcardSources[0].Excluded {
		t.Fatalf("expected 1 unexcluded WildcardSource, got %+v", rep1.WildcardSources)
	}
	if len(rep1.ActiveDrops) != 0 {
		t.Fatalf("unexcluded wildcard source must fail-safe protect all metrics, got %d drops", len(rep1.ActiveDrops))
	}

	// With excludeSources: Wildcard dashboard excluded, Tranche 1 drops dead_1 (10% <= 15%),
	// Run 2 during 7d soak keeps dead_1 without unlocking dead_2,
	// Run 3 after 7d soak unlocks Tranche 2 (dead_2)!
	cfgEx := config.DefaultConfig()
	cfgEx.Inventory.FixturePath = "fixture.json"
	cfgEx.Policy.ExcludeSources = []config.ExcludeSource{{Type: "dashboard_title", Value: "Cardinality Explorer"}}
	ledger := state.NewLedger()

	repRun1, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfgEx, Ledger: ledger, Inventory: inv, QueryLog: ql, Grafana: wildcardGraf,
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repRun1.ActiveDrops) != 1 || repRun1.Summary.ActiveTranche != 1 {
		t.Fatalf("Run 1 expected 1 family in Tranche 1, got %+v (tranche %d)", repRun1.ActiveDrops, repRun1.Summary.ActiveTranche)
	}

	// Run 2: 2 days later (still within 7d soak) -> holds at Tranche 1
	day2 := now.Add(2 * 24 * time.Hour)
	ql2 := fullCoverageQueryLog(day2, querylog.QueryLogEntry{Query: "live_a", Timestamp: day2.Add(-time.Hour), Origin: metrics.OriginAPIUser})
	repRun2, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfgEx, Ledger: ledger, Inventory: inv, QueryLog: ql2, Grafana: wildcardGraf,
		Capabilities: alloyCaps(), Now: day2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repRun2.Safety.SoakBlocked || len(repRun2.ActiveDrops) != 1 {
		t.Fatalf("Run 2 during soak should hold at 1 active drop, got soakBlocked=%v drops=%d", repRun2.Safety.SoakBlocked, len(repRun2.ActiveDrops))
	}

	// Run 3: 8 days later (7d soak complete, 0 regrets) -> unlocks Tranche 2!
	day8 := now.Add(8 * 24 * time.Hour)
	ql3 := fullCoverageQueryLog(day8, querylog.QueryLogEntry{Query: "live_a", Timestamp: day8.Add(-time.Hour), Origin: metrics.OriginAPIUser})
	repRun3, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfgEx, Ledger: ledger, Inventory: inv, QueryLog: ql3, Grafana: wildcardGraf,
		Capabilities: alloyCaps(), Now: day8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repRun3.Safety.SoakBlocked || len(repRun3.ActiveDrops) != 2 || repRun3.Summary.ActiveTranche != 2 {
		t.Fatalf("Run 3 after soak should unlock Tranche 2 (2 active drops), got soakBlocked=%v drops=%d tranche=%d",
			repRun3.Safety.SoakBlocked, len(repRun3.ActiveDrops), repRun3.Summary.ActiveTranche)
	}
}
