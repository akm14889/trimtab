package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/inventory/prometheus"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
	"trimtab.io/trimtab/pkg/usage/alertmanager"
	"trimtab.io/trimtab/pkg/usage/grafana"
	"trimtab.io/trimtab/pkg/usage/k8sconsumers"
	"trimtab.io/trimtab/pkg/usage/perses"
	"trimtab.io/trimtab/pkg/usage/promql"
	"trimtab.io/trimtab/pkg/usage/promrules"
	"trimtab.io/trimtab/pkg/usage/querylog"
	"trimtab.io/trimtab/pkg/usage/ruler"
	"trimtab.io/trimtab/pkg/usage/slo"
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
	observed := make(map[string]int, 30)
	for i := 0; i < 30; i++ {
		d := now.Add(-time.Duration(i) * 24 * time.Hour).UTC().Format("2006-01-02")
		observed[d] = 1
	}
	res := &querylog.Result{
		Configured:        true,
		EarliestTimestamp: now.Add(-31 * 24 * time.Hour),
		LatestTimestamp:   now,
		ObservedDates:     observed,
	}
	for _, e := range entries {
		if len(e.Accesses) == 0 && e.Query != "" {
			if ex, err := promql.Extract(e.Query); err == nil {
				e.Accesses = ex.Accesses
			}
		}
		res.Entries = append(res.Entries, e)
		res.TotalEntries++
		if !e.Timestamp.IsZero() {
			res.ObservedDates[e.Timestamp.UTC().Format("2006-01-02")]++
		}
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
					DashboardUID:   e.DashboardUID,
					Query:          e.Query,
					Timestamp:      e.Timestamp,
					ExplicitLabels: acc.ExplicitLabels,
					PreservesAll:   acc.PreservesAll,
					WithoutLabels:  acc.WithoutLabels,
					ClientCaller:   e.ClientCaller,
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
		Config:              cfg,
		Ledger:              state.NewLedger(),
		Inventory:           inv,
		QueryLog:            ql,
		PromRules:           promAlerts,
		Capabilities:        alloyCaps(),
		Now:                 now,
		ActivateNextTranche: true,
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
			Config:              cfg,
			Ledger:              state.NewLedger(),
			Inventory:           inv,
			QueryLog:            ql,
			Capabilities:        alloyCaps(),
			Now:                 now,
			ActivateNextTranche: true,
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
			Config:              cfg,
			Ledger:              state.NewLedger(),
			Inventory:           inv,
			QueryLog:            ql,
			Capabilities:        alloyCaps(),
			Now:                 now,
			ActivateNextTranche: true,
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
		Config:              cfg,
		Ledger:              ledger,
		Inventory:           inv,
		QueryLog:            ql,
		Grafana:             partialGrafana,
		Capabilities:        alloyCaps(),
		Now:                 now,
		ActivateNextTranche: true,
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
		ActivateNextTranche: true,
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
		Config:              cfg,
		Ledger:              state.NewLedger(),
		Inventory:           inv,
		QueryLog:            ql,
		Capabilities:        alloyCaps(),
		Now:                 now,
		ActivateNextTranche: true,
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
		Config:              cfg,
		Ledger:              state.NewLedger(),
		Inventory:           inv,
		QueryLog:            ql,
		Capabilities:        alloyCaps(),
		Now:                 now,
		ActivateNextTranche: true,
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
		Config:              cfg,
		Ledger:              state.NewLedger(),
		Inventory:           inv,
		QueryLog:            ql,
		PromRules:           rules,
		Capabilities:        alloyCaps(),
		Now:                 now,
		ActivateNextTranche: true,
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
	ledger.RecordTrancheActivation(1, now.Add(-10*24*time.Hour), 7*24*time.Hour, true, []metrics.Recommendation{
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
		Config:              cfg,
		Ledger:              ledger,
		Inventory:           inv,
		QueryLog:            ql,
		Capabilities:        alloyCaps(),
		Now:                 now,
		ActivateNextTranche: true,
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
		Capabilities: alloyCaps(), TargetName: "alloy", Now: now, ActivateNextTranche: true,
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
		Capabilities: otelCaps(), TargetName: "otel", Now: now, ActivateNextTranche: true,
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
	cfgEx.Policy.SoakStartMode = "immediate"
	cfgEx.Policy.ExcludeSources = []config.ExcludeSource{{Type: "dashboard_title", Value: "Cardinality Explorer"}}
	ledger := state.NewLedger()

	repRun1, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfgEx, Ledger: ledger, Inventory: inv, QueryLog: ql, Grafana: wildcardGraf,
		Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
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
		Capabilities: alloyCaps(), Now: day2, ActivateNextTranche: true,
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
		Capabilities: alloyCaps(), Now: day8, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repRun3.Safety.SoakBlocked || len(repRun3.ActiveDrops) != 2 || repRun3.Summary.ActiveTranche != 2 {
		t.Fatalf("Run 3 after soak should unlock Tranche 2 (2 active drops), got soakBlocked=%v drops=%d tranche=%d",
			repRun3.Safety.SoakBlocked, len(repRun3.ActiveDrops), repRun3.Summary.ActiveTranche)
	}
}

// Mandatory Safety Test #11 (P2.1b):
// Log-rotation survival via ledger LastQueriedAt & LastBucketQueriedAt + trailing CoveredDates bitmap.
func TestSafetyMatrix_11_LogRotationSurvivalAndTrailingBitmap(t *testing.T) {
	day10 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	day30 := day10.Add(20 * 24 * time.Hour)
	day45 := day10.Add(35 * 24 * time.Hour)
	old := day10.Add(-45 * 24 * time.Hour)

	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"live_core",
			"rotated_family",
			"rotated_hist_bucket", "rotated_hist_sum", "rotated_hist_count",
			"truly_dead_family",
		},
		Metadata: map[string]prometheus.MetricMetadata{
			"rotated_hist": {Type: "histogram"},
		},
		SeriesCounts: map[string]int{
			"live_core":           80000,
			"rotated_family":      5000,
			"rotated_hist_bucket": 4000,
			"rotated_hist_sum":    500,
			"rotated_hist_count":  500,
			"truly_dead_family":   1000,
		},
		FirstSeenHints: map[string]time.Time{
			"live_core": old, "rotated_family": old, "rotated_hist": old, "truly_dead_family": old,
		},
	}

	ledger := state.NewLedger()

	// Day 10: rotated_family and rotated_hist_bucket are queried in current query logs.
	qlDay10 := fullCoverageQueryLog(day10,
		querylog.QueryLogEntry{Query: "live_core", Timestamp: day10.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "rate(rotated_family[5m])", Timestamp: day10.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "rate(rotated_hist_bucket[5m])", Timestamp: day10.Add(-time.Hour), Origin: metrics.OriginAPIUser},
	)
	if _, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: inv, QueryLog: qlDay10,
		Capabilities: alloyCaps(), Now: day10,
	}); err != nil {
		t.Fatal(err)
	}

	// Day 30 (20 days later): query logs rotated! Only live_core and rotated_hist_count appear in current logs.
	// Ledger timestamps must keep rotated_family AND rotated_hist_bucket protected.
	qlDay30 := fullCoverageQueryLog(day30,
		querylog.QueryLogEntry{Query: "live_core", Timestamp: day30.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		querylog.QueryLogEntry{Query: "rate(rotated_hist_count[5m])", Timestamp: day30.Add(-time.Hour), Origin: metrics.OriginAPIUser},
	)
	rep30, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: inv, QueryLog: qlDay30,
		Capabilities: alloyCaps(), Now: day30, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rep30.ActiveDrops {
		if d.FamilyName == "rotated_family" || d.FamilyName == "rotated_hist" {
			t.Fatalf("rotated-off usage should stay protected within 30d window, got drop %+v", d)
		}
	}
	if len(rep30.ActiveDrops) != 1 || rep30.ActiveDrops[0].FamilyName != "truly_dead_family" {
		t.Fatalf("expected only truly_dead_family dropped at Day 30, got %+v", rep30.ActiveDrops)
	}

	// Day 45 (35 days after Day 10 query): rotated_family expires past 30d window and becomes eligible.
	ledgerExpire := state.NewLedger()
	ledgerExpire.RecordSeenFamily("rotated_family", old, day45)
	ledgerExpire.RecordLastQueried("rotated_family", day10) // 35d ago (> 30d window)
	qlDay45 := fullCoverageQueryLog(day45,
		querylog.QueryLogEntry{Query: "live_core", Timestamp: day45.Add(-time.Hour), Origin: metrics.OriginAPIUser},
	)
	rep45, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledgerExpire, Inventory: inv, QueryLog: qlDay45,
		Capabilities: alloyCaps(), Now: day45, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundRotated := false
	for _, d := range rep45.ActiveDrops {
		if d.FamilyName == "rotated_family" {
			foundRotated = true
		}
	}
	if !foundRotated {
		t.Fatalf("expected rotated_family to expire after 35d (> 30d window) and become droppable, got %+v", rep45.ActiveDrops)
	}

	// Trailing-window gap test: 5 early days + 5 recent days across 30d span = 10 covered days => Report-Only!
	gapDates := make(map[string]int)
	for i := 0; i < 5; i++ {
		gapDates[day30.Add(-time.Duration(29-i)*24*time.Hour).Format("2006-01-02")] = 3
		gapDates[day30.Add(-time.Duration(i)*24*time.Hour).Format("2006-01-02")] = 3
	}
	gappedQL := &querylog.Result{
		Configured:        true,
		EarliestTimestamp: day30.Add(-29 * 24 * time.Hour),
		LatestTimestamp:   day30,
		ObservedDates:     gapDates,
		TotalEntries:      30,
		APIUserCount:      30,
	}
	repGap, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: gappedQL,
		Capabilities: alloyCaps(), Now: day30, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repGap.Safety.ReportOnly || len(repGap.ActiveDrops) != 0 {
		t.Fatalf("3-week middle gap in CoveredDates must force Report-Only mode, got reportOnly=%v drops=%d", repGap.Safety.ReportOnly, len(repGap.ActiveDrops))
	}
	if repGap.Safety.QueryLogCoveredDays != 10 {
		t.Errorf("QueryLogCoveredDays = %v, want 10", repGap.Safety.QueryLogCoveredDays)
	}
}

// Mandatory Safety Test #12 (P2.1b):
// Observation-only analyze + explicit synth --activate + confirmed soak lifecycle.
func TestSafetyMatrix_12_ObservationOnlyAnalyzeAndConfirmedSoak(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	invFull := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"live_core",
			"dead_1",
			"dead_hist_bucket", "dead_hist_sum", "dead_hist_count",
			"dead_2",
		},
		Metadata: map[string]prometheus.MetricMetadata{
			"dead_hist": {Type: "histogram"},
		},
		SeriesCounts: map[string]int{
			"live_core":        80000,
			"dead_1":           8000,
			"dead_hist_bucket": 3000,
			"dead_hist_sum":    500,
			"dead_hist_count":  500,
			"dead_2":           8000,
		},
		FirstSeenHints: map[string]time.Time{
			"live_core": old, "dead_1": old, "dead_hist": old, "dead_2": old,
		},
	}

	ql := func(ts time.Time) *querylog.Result {
		return fullCoverageQueryLog(ts,
			querylog.QueryLogEntry{Query: "live_core", Timestamp: ts.Add(-time.Hour), Origin: metrics.OriginAPIUser},
			querylog.QueryLogEntry{Query: "rate(dead_hist_count[5m])", Timestamp: ts.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		)
	}

	ledger := state.NewLedger()

	// 1. Observation-only analyze must NEVER activate tranches.
	repObs, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invFull, QueryLog: ql(now),
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.ActiveDrops) != 0 || len(ledger.Tranches) != 0 || len(repObs.ActiveDrops) != 0 {
		t.Fatalf("observation-only analyze must not activate drops/tranches, got active=%d tranches=%d", len(ledger.ActiveDrops), len(ledger.Tranches))
	}
	if !repObs.Safety.NextTrancheReady || len(repObs.Tranches) == 0 {
		t.Fatalf("expected NextTrancheReady=true with planned tranches")
	}

	// 2. Preview-next must return Tranche 1 drops WITHOUT mutating ledger.
	repPrev, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invFull, QueryLog: ql(now),
		Capabilities: alloyCaps(), Now: now, PreviewNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repPrev.ActiveDrops) == 0 {
		t.Fatal("expected PreviewNextTranche to populate ActiveDrops for preview")
	}
	if len(ledger.ActiveDrops) != 0 || len(ledger.Tranches) != 0 {
		t.Fatalf("PreviewNextTranche must not mutate ledger, got active=%d tranches=%d", len(ledger.ActiveDrops), len(ledger.Tranches))
	}

	// 3. Explicit synth --activate enters pending_confirmation (Tranche 1: dead_1 + dead_hist bucket-only = 11,000 <= 15%).
	repAct, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invFull, QueryLog: ql(now),
		Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repAct.Safety.PendingConfirmation || ledger.LatestTranche().Status != "pending_confirmation" {
		t.Fatalf("expected pending_confirmation after ActivateNextTranche, got pending=%v status=%q", repAct.Safety.PendingConfirmation, ledger.LatestTranche().Status)
	}
	if len(repAct.ActiveDrops) != 2 {
		t.Fatalf("expected 2 drops (dead_1 + dead_hist _bucket) in Tranche 1, got %+v", repAct.ActiveDrops)
	}

	// 4. 72h later with dropped series STILL in inventory => stays pending_confirmation AND warns (>48h)!
	day3 := now.Add(72 * time.Hour)
	repStale, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invFull, QueryLog: ql(day3),
		Capabilities: alloyCaps(), Now: day3, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repStale.Safety.PendingConfirmation || ledger.LatestTranche().Status != "pending_confirmation" {
		t.Fatalf("tranche must stay pending_confirmation while series remain in inventory")
	}
	foundStaleWarn := false
	for _, w := range repStale.Safety.Warnings {
		if strings.Contains(w, "pending_confirmation for >48h") {
			foundStaleWarn = true
		}
	}
	if !foundStaleWarn {
		t.Fatalf("expected >48h pending_confirmation warning, got %v", repStale.Safety.Warnings)
	}

	// 5. Partial drop (dead_1 dropped, dead_hist_bucket still present) => still pending_confirmation!
	invPartial := &prometheus.InventorySnapshot{
		MetricNames: []string{"live_core", "dead_hist_bucket", "dead_hist_sum", "dead_hist_count", "dead_2"},
		Metadata:    invFull.Metadata,
		SeriesCounts: map[string]int{
			"live_core": 80000, "dead_hist_bucket": 3000, "dead_hist_sum": 500, "dead_hist_count": 500, "dead_2": 8000,
		},
		FirstSeenHints: invFull.FirstSeenHints,
	}
	repPartial, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invPartial, QueryLog: ql(day3),
		Capabilities: alloyCaps(), Now: day3, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repPartial.Safety.PendingConfirmation {
		t.Fatal("expected pending_confirmation while dead_hist_bucket is still present")
	}

	// 6. Collector drops dead_1 AND dead_hist_bucket (dead_hist_sum/count remain!) => auto-confirms to soaking!
	invConfirmed := &prometheus.InventorySnapshot{
		MetricNames: []string{"live_core", "dead_hist_sum", "dead_hist_count", "dead_2"},
		Metadata:    invFull.Metadata,
		SeriesCounts: map[string]int{
			"live_core": 80000, "dead_hist_sum": 500, "dead_hist_count": 500, "dead_2": 8000,
		},
		FirstSeenHints: invFull.FirstSeenHints,
	}
	day4 := now.Add(4 * 24 * time.Hour)
	repSoak, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invConfirmed, QueryLog: ql(day4),
		Capabilities: alloyCaps(), Now: day4, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repSoak.Safety.SoakBlocked || ledger.LatestTranche().Status != "soaking" {
		t.Fatalf("expected soaking after dropped series reached 0, got soakBlocked=%v status=%q", repSoak.Safety.SoakBlocked, ledger.LatestTranche().Status)
	}
	// Confirmed active drops from Tranche 1 must still be emitted in ActiveDrops even after disappearing from TSDB!
	if len(repSoak.ActiveDrops) != 2 {
		t.Fatalf("expected disappeared Tranche 1 drops to remain in ActiveDrops, got %+v", repSoak.ActiveDrops)
	}

	// 7. After soakWindow (day4 + 8d): observation-only analyze marks Tranche 1 soaked but does NOT activate Tranche 2.
	day12 := day4.Add(8 * 24 * time.Hour)
	repAfterSoakObs, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invConfirmed, QueryLog: ql(day12),
		Capabilities: alloyCaps(), Now: day12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ledger.LatestTranche().Status != "soaked" || !repAfterSoakObs.Safety.NextTrancheReady {
		t.Fatalf("expected Tranche 1 soaked and NextTrancheReady=true, got status=%q ready=%v", ledger.LatestTranche().Status, repAfterSoakObs.Safety.NextTrancheReady)
	}
	if len(repAfterSoakObs.ActiveDrops) != 2 {
		t.Fatalf("observation-only analyze must not unlock Tranche 2, got %d drops", len(repAfterSoakObs.ActiveDrops))
	}

	// 8. Explicit ActivateNextTranche unlocks Tranche 2 (dead_2).
	repT2, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: invConfirmed, QueryLog: ql(day12),
		Capabilities: alloyCaps(), Now: day12, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repT2.ActiveDrops) != 3 || repT2.Summary.ActiveTranche != 2 {
		t.Fatalf("expected 3 active drops in Tranche 2, got %d (tranche %d)", len(repT2.ActiveDrops), repT2.Summary.ActiveTranche)
	}
}

// Mandatory Safety Test #13 (P2.1b):
// Per-expression query-log wildcard detection (never unioning diverse exact queries by caller IP/User-Agent).
func TestSafetyMatrix_13_PerExpressionQueryLogWildcards(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{"m1", "m2", "m3", "m4", "dead_1"},
		SeriesCounts: map[string]int{
			"m1": 2000, "m2": 2000, "m3": 2000, "m4": 2000, "dead_1": 1000,
		},
		FirstSeenHints: map[string]time.Time{
			"m1": old, "m2": old, "m3": old, "m4": old, "dead_1": old,
		},
	}

	// Shared Grafana server/IP issues 4 distinct exact queries covering 80% (>50%) of inventory!
	// Must NOT be flagged as a wildcard protector.
	qlDiverse := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "m1", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser, ClientCaller: "Grafana/11.3"},
		querylog.QueryLogEntry{Query: "m2", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser, ClientCaller: "Grafana/11.3"},
		querylog.QueryLogEntry{Query: "m3", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser, ClientCaller: "Grafana/11.3"},
		querylog.QueryLogEntry{Query: "m4", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser, ClientCaller: "Grafana/11.3"},
	)
	repDiv, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: qlDiverse,
		Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repDiv.WildcardSources) != 0 {
		t.Fatalf("diverse exact queries from shared caller must NOT trigger wildcard detection, got %+v", repDiv.WildcardSources)
	}
	if len(repDiv.ActiveDrops) != 1 || repDiv.ActiveDrops[0].FamilyName != "dead_1" {
		t.Fatalf("expected dead_1 dropped when shared caller runs diverse exact queries, got %+v", repDiv.ActiveDrops)
	}

	// Single wildcard expression `{__name__=~".+"}` from the same shared caller IS flagged!
	qlWild := fullCoverageQueryLog(now,
		querylog.QueryLogEntry{Query: "m1", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser, ClientCaller: "Grafana/11.3"},
		querylog.QueryLogEntry{Query: `count({__name__=~".+"})`, Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser, ClientCaller: "Grafana/11.3", DashboardUID: "exp-uid"},
	)
	repWild, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: qlWild,
		Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repWild.WildcardSources) != 1 {
		t.Fatalf("expected 1 WildcardSource for {__name__=~\".+\"}, got %+v", repWild.WildcardSources)
	}
	if !strings.Contains(repWild.WildcardSources[0].RecommendedExclude, "exp-uid") {
		t.Errorf("expected RecommendedExclude to include dashboard_uid, got %q", repWild.WildcardSources[0].RecommendedExclude)
	}
	if len(repWild.ActiveDrops) != 0 {
		t.Fatalf("unexcluded wildcard expression must protect all metrics, got %d drops", len(repWild.ActiveDrops))
	}

	// Excluding by query_regex excludes ONLY the wildcard expression while keeping `m1` protected!
	cfgEx := config.DefaultConfig()
	cfgEx.Inventory.FixturePath = "fixture.json"
	cfgEx.Policy.ExcludeSources = []config.ExcludeSource{{Type: "query_regex", Value: `__name__=~"\.\+"`}}
	repEx, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfgEx, Ledger: state.NewLedger(), Inventory: inv, QueryLog: qlWild,
		Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range repEx.ActiveDrops {
		if d.FamilyName == "m1" {
			t.Fatal("excluding wildcard query_regex must NOT exclude Grafana's exact m1 query")
		}
	}
	if len(repEx.ActiveDrops) != 1 || repEx.ActiveDrops[0].FamilyName != "dead_1" {
		t.Fatalf("expected dead_1 dropped after excluding wildcard regex, got %+v", repEx.ActiveDrops)
	}
}

func TestBaselineHWMAndDailyQueryLogRate(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)
	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"
	cfg.Usage.Grafana = &config.GrafanaConfig{URL: "http://grafana.local"}

	inv := &prometheus.InventorySnapshot{
		MetricNames:    []string{"live_m", "dead_m"},
		SeriesCounts:   map[string]int{"live_m": 9000, "dead_m": 1000},
		FirstSeenHints: map[string]time.Time{"live_m": old, "dead_m": old},
	}

	makeDailyQL := func(days, perDay int) *querylog.Result {
		dates := make(map[string]int, days)
		for i := 0; i < days; i++ {
			dates[now.Add(-time.Duration(i)*24*time.Hour).Format("2006-01-02")] = perDay
		}
		ex, _ := promql.Extract("live_m")
		return &querylog.Result{
			Configured:        true,
			EarliestTimestamp: now.Add(-time.Duration(days-1) * 24 * time.Hour),
			LatestTimestamp:   now,
			ObservedDates:     dates,
			TotalEntries:      days * perDay,
			APIUserCount:      days * perDay,
			Refs: []metrics.UsageRef{{
				MetricName:     "live_m",
				Origin:         metrics.OriginAPIUser,
				SourceID:       "querylog:api-user",
				Timestamp:      now.Add(-time.Hour),
				PreservesAll:   ex.Accesses[0].PreservesAll,
			}},
		}
	}

	ledger := state.NewLedger()

	// Run 1: 100 dashboards, 30d @ 100 entries/day.
	_, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: inv,
		QueryLog: makeDailyQL(30, 100), Grafana: &grafana.ScanResult{DashboardCount: 100},
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run 2: 85 dashboards (-15%, within 20% threshold) -> HWM must stay 100 (no bleed-down!),
	// AND query-log rotation to 7d @ 100/day (-77% total entries, 0% daily rate drop) must NOT false-abort!
	rep2, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: inv,
		QueryLog: makeDailyQL(7, 100), Grafana: &grafana.ScanResult{DashboardCount: 85},
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Safety.BaselineDriftDetected {
		t.Fatalf("log rotation keeping 100/d rate must not trip drift guard: %v", rep2.Safety.BaselineDriftDetails)
	}
	if *ledger.BaselineCounts.Dashboards != 100 {
		t.Fatalf("HWM dashboard baseline bled down to %d, want 100", *ledger.BaselineCounts.Dashboards)
	}

	// Run 3: 75 dashboards (-25% vs HWM 100, even though only -11.8% vs 85!) -> MUST trip drift guard!
	// Also weekend dip in query log (65/day = -35% < 50% threshold) does NOT trip query-log drift.
	rep3, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: inv,
		QueryLog: makeDailyQL(30, 65), Grafana: &grafana.ScanResult{DashboardCount: 75},
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep3.Safety.BaselineDriftDetected {
		t.Fatal("expected static HWM bleed-down guard to trip at 75 vs HWM 100")
	}

	// Run 4: Broken query log pipeline (20/day = -80% > 50% threshold) -> MUST trip drift guard!
	rep4, err := Analyze(context.Background(), AnalyzeInput{
		Config: cfg, Ledger: ledger, Inventory: inv,
		QueryLog: makeDailyQL(30, 20), Grafana: &grafana.ScanResult{DashboardCount: 100},
		Capabilities: alloyCaps(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep4.Safety.BaselineDriftDetected {
		t.Fatal("expected broken query-log pipeline (20/d vs 100/d) to trip drift guard")
	}
}

func TestStreamingDedupRegretPreservesHitCountAndLatestTS(t *testing.T) {
	now := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)
	ledger := state.NewLedger()
	ledger.RecordTrancheActivation(1, time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC), 7*24*time.Hour, true, []metrics.Recommendation{
		{FamilyName: "dropped_m", Action: metrics.ActionDropFamily, Series: 100},
	})

	ex, err := promql.Extract("rate(dropped_m[5m])")
	if err != nil {
		t.Fatal(err)
	}
	res := &querylog.Result{
		Configured: true,
		Entries: []querylog.QueryLogEntry{
			{
				Query:             "rate(dropped_m[5m])",
				EarliestTimestamp: time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC),
				Timestamp:         time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
				HitCount:          3,
				Origin:            metrics.OriginAPIUser,
				Accesses:          ex.Accesses,
			},
		},
	}

	cat := GroupFamilies(&prometheus.InventorySnapshot{
		MetricNames:  []string{"dropped_m"},
		SeriesCounts: map[string]int{"dropped_m": 100},
	})

	policy := config.DefaultConfig().Policy
	policy.MinRegretHits = 3
	regrets := DetectRegrets(cat, ledger, res, policy, now)
	if len(regrets) != 1 || regrets[0].HitCount != 3 {
		t.Fatalf("expected regret with HitCount=3 after streaming dedup, got %+v", regrets)
	}
}

// Phase 2.2 Safety Matrix Regression:
// Ruler alerts & KEDA/prometheus-adapter/Argo Rollouts/Flagger protect live families & labels,
// Ruler recording rules join the transitive DAG, static baseline drift aborts safely,
// and static consumers alone without query logs keep Trimtab in Report-Only mode (Invariant #1).
func TestSafetyMatrix_P22_RulersAndK8sConsumers(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"keda_scaled_m",
			"adapter_custom_m",
			"rollout_canary_m",
			"flagger_canary_m",
			"ruler_alerted_m",
			"ruler_live_src_m",
			"ruler:live_rec",
			"ruler_dead_src_m",
			"ruler:dead_rec",
			"truly_unused_m",
		},
		SeriesCounts: map[string]int{
			"keda_scaled_m":    1000,
			"adapter_custom_m": 1000,
			"rollout_canary_m": 1000,
			"flagger_canary_m": 1000,
			"ruler_alerted_m":  1000,
			"ruler_live_src_m": 1000,
			"ruler:live_rec":   200,
			"ruler_dead_src_m": 400,
			"ruler:dead_rec":   100,
			"truly_unused_m":   300,
		},
		MetricLabels: map[string][]string{
			"keda_scaled_m":    {"service", "unused_lbl"},
			"adapter_custom_m": {"namespace", "pod", "unused_lbl"},
			"rollout_canary_m": {"app", "unused_lbl"},
			"flagger_canary_m": {"namespace", "unused_lbl"},
			"ruler_alerted_m":  {"cluster", "unused_lbl"},
		},
		SampleLabelSets: map[string][]map[string]string{
			"keda_scaled_m":    {{"service": "s1", "unused_lbl": "u1"}, {"service": "s1", "unused_lbl": "u2"}},
			"adapter_custom_m": {{"namespace": "ns1", "pod": "p1", "unused_lbl": "u1"}, {"namespace": "ns1", "pod": "p1", "unused_lbl": "u2"}},
			"rollout_canary_m": {{"app": "a1", "unused_lbl": "u1"}, {"app": "a1", "unused_lbl": "u2"}},
			"flagger_canary_m": {{"namespace": "ns1", "unused_lbl": "u1"}, {"namespace": "ns1", "unused_lbl": "u2"}},
			"ruler_alerted_m":  {{"cluster": "c1", "unused_lbl": "u1"}, {"cluster": "c1", "unused_lbl": "u2"}},
		},
		FirstSeenHints: map[string]time.Time{
			"keda_scaled_m": old, "adapter_custom_m": old, "rollout_canary_m": old,
			"flagger_canary_m": old, "ruler_alerted_m": old, "ruler_live_src_m": old,
			"ruler:live_rec": old, "ruler_dead_src_m": old, "ruler:dead_rec": old,
			"truly_unused_m": old,
		},
	}

	mustExtract := func(e string) []promql.MetricAccess {
		ex, err := promql.Extract(e)
		if err != nil {
			t.Fatal(err)
		}
		return ex.Accesses
	}

	rulerRes := &ruler.ScanResult{
		TotalRuleCount:     3,
		AlertRuleCount:     1,
		RecordingRuleCount: 2,
		AlertRefs: []metrics.UsageRef{
			{MetricName: "ruler_alerted_m", Origin: metrics.OriginRulerAlert, SourceID: "ruler-alert:1", ExplicitLabels: []string{"cluster"}},
		},
		RecordingRules: []promrules.RecordingRule{
			{Name: "ruler:live_rec", File: "mimir-ruler/prod", Group: "g1", Expr: "rate(ruler_live_src_m[5m])", Inputs: mustExtract("rate(ruler_live_src_m[5m])")},
			{Name: "ruler:dead_rec", File: "mimir-ruler/prod", Group: "g2", Expr: "rate(ruler_dead_src_m[5m])", Inputs: mustExtract("rate(ruler_dead_src_m[5m])")},
		},
	}

	k8sRes := &k8sconsumers.ScanResult{
		KEDATriggerCount:     1,
		PromAdapterRuleCount: 1,
		RolloutTemplateCount: 1,
		FlaggerTemplateCount: 1,
		Refs: []metrics.UsageRef{
			{MetricName: "keda_scaled_m", Origin: metrics.OriginKEDA, SourceID: "keda:1", ExplicitLabels: []string{"service"}},
			{MetricName: "adapter_custom_m", Origin: metrics.OriginPromAdapter, SourceID: "adapter:1", ExplicitLabels: []string{"namespace", "pod"}},
			{MetricName: "rollout_canary_m", Origin: metrics.OriginArgoRollouts, SourceID: "rollout:1", ExplicitLabels: []string{"app"}},
			{MetricName: "flagger_canary_m", Origin: metrics.OriginFlagger, SourceID: "flagger:1", ExplicitLabels: []string{"namespace"}},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"
	cfg.Usage.Rulers = []config.RulerConfig{{Type: "mimir", URL: "http://mimir.local"}}
	cfg.Usage.K8sConsumers = &config.K8sConsumersConfig{Globs: []string{"k8s/*.yaml"}}

	t.Run("static P2.2 consumers alone stay in Report-Only without query logs", func(t *testing.T) {
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config:              cfg,
			Ledger:              state.NewLedger(),
			Inventory:           inv,
			Ruler:               rulerRes,
			K8sConsumers:        k8sRes,
			Capabilities:        alloyCaps(),
			Now:                 now,
			ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Safety.ReportOnly || len(rep.ActiveDrops) != 0 || rep.Summary.Confidence != metrics.ConfidenceMedium {
			t.Fatalf("expected ReportOnly + 0 active drops + MEDIUM confidence without query logs, got reportOnly=%v drops=%d conf=%s",
				rep.Safety.ReportOnly, len(rep.ActiveDrops), rep.Summary.Confidence)
		}
	})

	t.Run("protects consumers + labels, resolves ruler DAG, and enforces HWM baseline drift", func(t *testing.T) {
		ql := fullCoverageQueryLog(now,
			querylog.QueryLogEntry{Query: "ruler:live_rec", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser},
		)
		ledger := state.NewLedger()
		fixEval := prometheus.NewFixtureCollapseEvaluator(inv)

		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config:              cfg,
			Ledger:              ledger,
			Inventory:           inv,
			CollapseEvaluator:   fixEval,
			QueryLog:            ql,
			Ruler:               rulerRes,
			K8sConsumers:        k8sRes,
			Capabilities:        alloyCaps(),
			Now:                 now,
			ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Only ruler_dead_src_m and truly_unused_m should be dropped; all P2.2 consumer metrics & ruler_live_src_m stay safe!
		droppedSet := map[string]bool{}
		for _, d := range rep.ActiveDrops {
			droppedSet[d.FamilyName] = true
		}
		if !droppedSet["truly_unused_m"] || !droppedSet["ruler_dead_src_m"] || len(droppedSet) != 2 {
			t.Fatalf("expected only truly_unused_m and ruler_dead_src_m dropped, got %+v", rep.ActiveDrops)
		}
		for _, protected := range []string{"keda_scaled_m", "adapter_custom_m", "rollout_canary_m", "flagger_canary_m", "ruler_alerted_m", "ruler_live_src_m", "ruler:live_rec"} {
			if _, ok := rep.ProtectedReasons[protected]; !ok {
				t.Errorf("expected %s in ProtectedReasons", protected)
			}
		}
		if len(rep.DeadRecordingRules) != 1 || rep.DeadRecordingRules[0].RuleName != "ruler:dead_rec" {
			t.Fatalf("expected ruler:dead_rec in DeadRecordingRules, got %+v", rep.DeadRecordingRules)
		}
		// Used labels on P2.2 consumers must NEVER appear in unused label collapse advisories
		for _, adv := range rep.LabelCollapseAdvisories {
			if len(adv.UnusedLabels) != 1 || adv.UnusedLabels[0] != "unused_lbl" {
				t.Errorf("unexpected unused labels on %s: %v", adv.FamilyName, adv.UnusedLabels)
			}
		}

		// Partial KEDA/Ruler drop (3->1 ruler rules, 1->0 keda triggers) triggers baseline drift abort!
		partialRuler := &ruler.ScanResult{TotalRuleCount: 1}
		partialK8s := &k8sconsumers.ScanResult{KEDATriggerCount: 0, PromAdapterRuleCount: 1, RolloutTemplateCount: 1, FlaggerTemplateCount: 1}
		repDrift, err := Analyze(context.Background(), AnalyzeInput{
			Config:              cfg,
			Ledger:              ledger,
			Inventory:           inv,
			QueryLog:            ql,
			Ruler:               partialRuler,
			K8sConsumers:        partialK8s,
			Capabilities:        alloyCaps(),
			Now:                 now,
			ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !repDrift.Safety.BaselineDriftDetected || !repDrift.Safety.Blocked {
			t.Fatalf("expected baseline drift abort when ruler_rules/keda_triggers drop >20%%, got %+v", repDrift.Safety)
		}
	})
}

// Phase 2.3 Safety Matrix Regression:
// Alertmanager protects routing/grouping/inhibit/template labels ONLY on alerted metrics,
// Perses catch-all dashboards trigger WildcardProtector and support dashboard_uid exclusion,
// Sloth/Pyrra SLOs protect metrics & grouping labels, baseline drift guards all three,
// and static P2.3 consumers alone stay in Report-Only mode without query logs (Invariant #1).
func TestSafetyMatrix_P23_AlertmanagerPersesAndSLO(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-45 * 24 * time.Hour)

	inv := &prometheus.InventorySnapshot{
		MetricNames: []string{
			"prom_alerted_m",
			"grafana_alerted_m",
			"ruler_alerted_m",
			"dash_only_m",
			"perses_m",
			"slo_http_duration_seconds_bucket",
			"slo_http_duration_seconds_sum",
			"slo_http_duration_seconds_count",
			"truly_unused_m",
		},
		Metadata: map[string]prometheus.MetricMetadata{
			"slo_http_duration_seconds": {Type: "histogram"},
		},
		SeriesCounts: map[string]int{
			"prom_alerted_m":                   1000,
			"grafana_alerted_m":                1000,
			"ruler_alerted_m":                  1000,
			"dash_only_m":                      1000,
			"perses_m":                         1000,
			"slo_http_duration_seconds_bucket": 1000,
			"slo_http_duration_seconds_sum":    100,
			"slo_http_duration_seconds_count":  100,
			"truly_unused_m":                   300,
		},
		MetricLabels: map[string][]string{
			"prom_alerted_m":                   {"service", "route_lbl", "group_only_lbl", "unused_lbl"},
			"grafana_alerted_m":                {"service", "route_lbl", "unused_lbl"},
			"ruler_alerted_m":                  {"service", "group_only_lbl", "unused_lbl"},
			"dash_only_m":                      {"service", "route_lbl", "group_only_lbl"},
			"perses_m":                         {"cluster", "unused_lbl"},
			"slo_http_duration_seconds_bucket": {"le", "endpoint", "unused_lbl"},
		},
		SampleLabelSets: map[string][]map[string]string{
			"prom_alerted_m":    {{"service": "s1", "route_lbl": "r1", "group_only_lbl": "g1", "unused_lbl": "u1"}, {"service": "s1", "route_lbl": "r1", "group_only_lbl": "g1", "unused_lbl": "u2"}},
			"grafana_alerted_m": {{"service": "s1", "route_lbl": "r1", "unused_lbl": "u1"}, {"service": "s1", "route_lbl": "r1", "unused_lbl": "u2"}},
			"ruler_alerted_m":   {{"service": "s1", "group_only_lbl": "g1", "unused_lbl": "u1"}, {"service": "s1", "group_only_lbl": "g1", "unused_lbl": "u2"}},
			"dash_only_m":       {{"service": "s1", "route_lbl": "r1", "group_only_lbl": "g1"}, {"service": "s1", "route_lbl": "r2", "group_only_lbl": "g2"}},
			"perses_m":          {{"cluster": "c1", "unused_lbl": "u1"}, {"cluster": "c1", "unused_lbl": "u2"}},
			"slo_http_duration_seconds_bucket": {
				{"le": "0.5", "endpoint": "/pay", "unused_lbl": "u1"},
				{"le": "0.5", "endpoint": "/pay", "unused_lbl": "u2"},
			},
		},
		FirstSeenHints: map[string]time.Time{
			"prom_alerted_m": old, "grafana_alerted_m": old, "ruler_alerted_m": old,
			"dash_only_m": old, "perses_m": old, "slo_http_duration_seconds": old, "truly_unused_m": old,
		},
	}

	amRes := &alertmanager.ScanResult{
		ProtectedLabels: []string{"group_only_lbl", "route_lbl"},
		MatcherCount:    4,
	}
	persesRes := &perses.ScanResult{
		DashboardCount: 2,
		Refs: []metrics.UsageRef{
			{
				MetricName:     "perses_m",
				Origin:         metrics.OriginPersesDashboard,
				SourceID:       "perses-dashboard:ops/service",
				SourceName:     "Service Perses",
				DashboardUID:   "ops/service",
				ExplicitLabels: []string{"cluster"},
			},
			{
				MetricRegex:  ".+",
				IsRegex:      true,
				Origin:       metrics.OriginPersesDashboard,
				SourceID:     "perses-dashboard:ops/wildcard",
				SourceName:   "Perses Catchall Explorer",
				DashboardUID: "ops/wildcard",
			},
		},
	}
	sloRes := &slo.ScanResult{
		SLOCount: 2,
		Refs: []metrics.UsageRef{
			{
				MetricName:     "slo_http_duration_seconds_bucket",
				Origin:         metrics.OriginSLO,
				SourceID:       "slo:pyrra:latency",
				SourceName:     "Pyrra Latency",
				ExplicitLabels: []string{"le", "endpoint"},
			},
		},
	}
	promAlerts := &promrules.ScanResult{
		AlertRuleCount: 1,
		TotalRuleCount: 1,
		AlertRefs: []metrics.UsageRef{
			{MetricName: "prom_alerted_m", Origin: metrics.OriginPromAlert, SourceID: "prom-alert:a", ExplicitLabels: []string{"service"}},
		},
	}
	grafRes := &grafana.ScanResult{
		DashboardCount: 1,
		AlertRuleCount: 1,
		Refs: []metrics.UsageRef{
			{MetricName: "grafana_alerted_m", Origin: metrics.OriginGrafanaAlert, SourceID: "grafana-alert:a", ExplicitLabels: []string{"service"}},
			{MetricName: "dash_only_m", Origin: metrics.OriginGrafanaDashboard, SourceID: "grafana-dashboard:d1", ExplicitLabels: []string{"service"}},
		},
	}
	rulerRes := &ruler.ScanResult{
		TotalRuleCount: 1,
		AlertRuleCount: 1,
		AlertRefs: []metrics.UsageRef{
			{MetricName: "ruler_alerted_m", Origin: metrics.OriginRulerAlert, SourceID: "ruler-alert:r", ExplicitLabels: []string{"service"}},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Inventory.FixturePath = "fixture.json"
	cfg.Usage.Alertmanager = &config.AlertmanagerConfig{URL: "http://am.local"}
	cfg.Usage.Perses = &config.PersesConfig{URL: "http://perses.local"}
	cfg.Usage.SLO = &config.SLOConfig{Globs: []string{"slos/*.yaml"}}
	cfg.Usage.Grafana = &config.GrafanaConfig{URL: "http://grafana.local"}
	cfg.Usage.PromRules = []string{"rules/*.yaml"}
	cfg.Usage.Rulers = []config.RulerConfig{{Type: "mimir", URL: "http://mimir.local"}}

	t.Run("static P2.3 sources alone stay Report-Only without query logs", func(t *testing.T) {
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: state.NewLedger(), Inventory: inv,
			Alertmanager: amRes, Perses: persesRes, SLO: sloRes,
			Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Safety.ReportOnly || len(rep.ActiveDrops) != 0 || rep.Summary.Confidence != metrics.ConfidenceMedium {
			t.Fatalf("expected ReportOnly with 0 drops and MEDIUM confidence, got reportOnly=%v drops=%d conf=%s",
				rep.Safety.ReportOnly, len(rep.ActiveDrops), rep.Summary.Confidence)
		}
	})

	t.Run("Perses catch-all wildcard flags RecommendedExclude dashboard_uid", func(t *testing.T) {
		ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "prom_alerted_m", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})
		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: ql,
			Perses: persesRes, Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.WildcardSources) != 1 || !strings.Contains(rep.WildcardSources[0].RecommendedExclude, `dashboard_uid="ops/wildcard"`) {
			t.Fatalf("expected Perses wildcard source with RecommendedExclude, got %+v", rep.WildcardSources)
		}
		if len(rep.ActiveDrops) != 0 {
			t.Fatalf("unexcluded Perses catch-all should protect all metrics, got %d drops", len(rep.ActiveDrops))
		}
	})

	t.Run("Alertmanager protects alerted metric labels only, SLO/Perses protect metrics, HWM drift aborts", func(t *testing.T) {
		exCfg := *cfg
		exCfg.Policy.ExcludeSources = []config.ExcludeSource{{Type: "dashboard_uid", Value: "ops/wildcard"}}
		ql := fullCoverageQueryLog(now, querylog.QueryLogEntry{Query: "sum by (service) (prom_alerted_m)", Timestamp: now.Add(-time.Hour), Origin: metrics.OriginAPIUser})
		ledger := state.NewLedger()
		fixEval := prometheus.NewFixtureCollapseEvaluator(inv)

		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config: &exCfg, Ledger: ledger, Inventory: inv, CollapseEvaluator: fixEval,
			QueryLog: ql, Grafana: grafRes, PromRules: promAlerts, Ruler: rulerRes,
			Alertmanager: amRes, Perses: persesRes, SLO: sloRes,
			Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.ActiveDrops) != 1 || rep.ActiveDrops[0].FamilyName != "truly_unused_m" {
			t.Fatalf("expected ONLY truly_unused_m dropped, got %+v", rep.ActiveDrops)
		}

		byAdv := map[string][]string{}
		for _, adv := range rep.LabelCollapseAdvisories {
			byAdv[adv.FamilyName] = adv.UnusedLabels
		}
		for _, alertedFam := range []string{"prom_alerted_m", "grafana_alerted_m", "ruler_alerted_m"} {
			if unused := byAdv[alertedFam]; !slices.Equal(unused, []string{"unused_lbl"}) {
				t.Errorf("%s unused labels = %v, want [unused_lbl] (Alertmanager labels must be protected)", alertedFam, unused)
			}
		}
		// dash_only_m has no alert refs, so Alertmanager labels must NOT falsely protect its unused labels!
		if dashUnused := byAdv["dash_only_m"]; !slices.Contains(dashUnused, "route_lbl") || !slices.Contains(dashUnused, "group_only_lbl") {
			t.Errorf("dash_only_m should not receive Alertmanager label protection, got unused=%v", dashUnused)
		}
		if sloUnused := byAdv["slo_http_duration_seconds"]; !slices.Equal(sloUnused, []string{"unused_lbl"}) {
			t.Errorf("slo_http_duration_seconds unused labels = %v, want [unused_lbl]", sloUnused)
		}
		if persesUnused := byAdv["perses_m"]; !slices.Equal(persesUnused, []string{"unused_lbl"}) {
			t.Errorf("perses_m unused labels = %v, want [unused_lbl]", persesUnused)
		}

		// Baseline drift (>20% drop across AlertmanagerMatchers, PersesDashboards, SLOs) aborts synthesis!
		partialAM := &alertmanager.ScanResult{ProtectedLabels: amRes.ProtectedLabels, MatcherCount: 1}
		partialPerses := &perses.ScanResult{DashboardCount: 1, Refs: persesRes.Refs}
		partialSLO := &slo.ScanResult{SLOCount: 1, Refs: sloRes.Refs}
		repDrift, err := Analyze(context.Background(), AnalyzeInput{
			Config: &exCfg, Ledger: ledger, Inventory: inv, QueryLog: ql,
			Grafana: grafRes, PromRules: promAlerts, Ruler: rulerRes,
			Alertmanager: partialAM, Perses: partialPerses, SLO: partialSLO,
			Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !repDrift.Safety.BaselineDriftDetected || !repDrift.Safety.Blocked {
			t.Fatalf("expected P2.3 baseline drift abort, got %+v", repDrift.Safety)
		}
	})
}

func TestE2ERegressionFindings(t *testing.T) {
	t.Run("F2 sub-day 20m and 1d cold-start stay ReportOnly with ProjectedDrops", func(t *testing.T) {
		now := time.Date(2026, 11, 25, 18, 1, 0, 0, time.UTC)
		cfg := config.DefaultConfig()
		cfg.Inventory.FixturePath = "fixture.json"
		cfg.Policy.MinObservationWindow = config.Duration(20 * time.Minute)

		inv := &prometheus.InventorySnapshot{
			MetricNames:  []string{"live_m", "unused_m"},
			SeriesCounts: map[string]int{"live_m": 800, "unused_m": 200},
			FirstSeenHints: map[string]time.Time{
				"live_m":   now.Add(-time.Minute),
				"unused_m": now.Add(-time.Minute),
			},
		}
		ex, _ := promql.Extract("live_m")
		shortQL := &querylog.Result{
			Configured:        true,
			RequestedWindow:   20 * time.Minute,
			EarliestTimestamp: now.Add(-time.Minute),
			LatestTimestamp:   now,
			ObservedDates:     map[string]int{"2026-11-25": 50},
			TotalEntries:      50,
			APIUserCount:      50,
			Refs: []metrics.UsageRef{{
				MetricName:   "live_m",
				Origin:       metrics.OriginAPIUser,
				SourceID:     "querylog:api-user",
				Timestamp:    now,
				PreservesAll: ex.Accesses[0].PreservesAll,
			}},
		}

		rep, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: state.NewLedger(), Inventory: inv, QueryLog: shortQL,
			Capabilities: alloyCaps(), Now: now, ActivateNextTranche: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Safety.ReportOnly || rep.Summary.Confidence != metrics.ConfidenceMedium {
			t.Fatalf("expected ReportOnly=true & MEDIUM confidence on 1m/20m cold start, got %+v %s", rep.Safety, rep.Summary.Confidence)
		}
		if rep.Summary.SynthesizedFamilies != 0 || len(rep.ActiveDrops) != 0 {
			t.Fatalf("expected 0 active drops on cold start, got %d", len(rep.ActiveDrops))
		}
		if len(rep.ProjectedDrops) != 1 || rep.ProjectedDrops[0].FamilyName != "unused_m" {
			t.Fatalf("expected ProjectedDrops=[unused_m] during cold start, got %+v", rep.ProjectedDrops)
		}
	})

	t.Run("F5 narrowing Loki window keeps constant daily rate without false drift", func(t *testing.T) {
		now := time.Date(2026, 11, 25, 18, 30, 0, 0, time.UTC)
		old := now.Add(-time.Hour)
		cfg := config.DefaultConfig()
		cfg.Inventory.FixturePath = "fixture.json"
		cfg.Policy.MinObservationWindow = config.Duration(20 * time.Minute)

		inv := &prometheus.InventorySnapshot{
			MetricNames:    []string{"live_m", "dead_m"},
			SeriesCounts:   map[string]int{"live_m": 800, "dead_m": 200},
			FirstSeenHints: map[string]time.Time{"live_m": old, "dead_m": old},
		}
		ex, _ := promql.Extract("live_m")
		mkQL := func(win time.Duration, apiEntries, exclEntries int) *querylog.Result {
			return &querylog.Result{
				Configured:        true,
				RequestedWindow:   win,
				EarliestTimestamp: now.Add(-win),
				LatestTimestamp:   now,
				ObservedDates:     map[string]int{"2026-11-25": apiEntries + exclEntries},
				TotalEntries:      apiEntries + exclEntries,
				APIUserCount:      apiEntries,
				ExcludedCount:     exclEntries,
				Refs: []metrics.UsageRef{{
					MetricName:   "live_m",
					Origin:       metrics.OriginAPIUser,
					SourceID:     "querylog:api-user",
					Timestamp:    now,
					PreservesAll: ex.Accesses[0].PreservesAll,
				}},
			}
		}

		ledger := state.NewLedger()
		// 20m window with heavy Trimtab ExcludedCount burst (800 self-queries + 400 api-user)
		if _, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: inv, QueryLog: mkQL(20*time.Minute, 400, 800),
			Capabilities: alloyCaps(), Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		// 5m window at steady API-user QPS (100 api-user + 2 excluded) MUST NOT trip baseline drift!
		rep5m, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: inv, QueryLog: mkQL(5*time.Minute, 100, 2),
			Capabilities: alloyCaps(), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if rep5m.Safety.BaselineDriftDetected {
			t.Fatalf("narrowing window 20m -> 5m at steady QPS (with earlier ExcludedCount burst) must not trip baseline drift: %v", rep5m.Safety.BaselineDriftDetails)
		}

		// Real API queries dropping to 0 (even if self/excluded queries exist) MUST still trip -100% baseline drift!
		repExclOnly, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: inv,
			QueryLog: &querylog.Result{
				Configured:      true,
				RequestedWindow: 20 * time.Minute,
				ObservedDates:   map[string]int{"2026-11-25": 200},
				TotalEntries:    200,
				ExcludedCount:   200,
			},
			Capabilities: alloyCaps(), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !repExclOnly.Safety.BaselineDriftDetected || !repExclOnly.Safety.Blocked {
			t.Fatalf("expected 0 api/rule queries (excluded-only) to trip drift guard, got %+v", repExclOnly.Safety)
		}

		// Outage (0 entries) MUST still trip -100% baseline drift!
		repOutage, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: inv,
			QueryLog: &querylog.Result{Configured: true, RequestedWindow: 20 * time.Minute},
			Capabilities: alloyCaps(), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !repOutage.Safety.BaselineDriftDetected || !repOutage.Safety.Blocked {
			t.Fatalf("expected 0-entry query-log outage to trip drift guard, got %+v", repOutage.Safety)
		}
	})

	t.Run("F4 and F7.5 regret revert lifecycle and 0-series exclusion", func(t *testing.T) {
		now := time.Date(2026, 11, 25, 19, 0, 0, 0, time.UTC)
		old := now.Add(-time.Hour)
		cfg := config.DefaultConfig()
		cfg.Inventory.FixturePath = "fixture.json"
		cfg.Policy.MinObservationWindow = config.Duration(20 * time.Minute)
		cfg.Policy.SoakWindow = config.Duration(time.Minute)

		inv := &prometheus.InventorySnapshot{
			MetricNames:  []string{"live_m", "dead_m", "ghost_zero_series"},
			SeriesCounts: map[string]int{"live_m": 800, "dead_m": 100, "ghost_zero_series": 0},
			FirstSeenHints: map[string]time.Time{
				"live_m": old, "dead_m": old, "ghost_zero_series": old,
			},
		}
		ledger := state.NewLedger()
		ledger.RecordTrancheActivation(1, now.Add(-2*time.Minute), time.Minute, true, []metrics.Recommendation{
			{FamilyName: "victim_m", Action: metrics.ActionDropFamily, Series: 50},
		})

		exLive, _ := promql.Extract("live_m")
		exVic, _ := promql.Extract("victim_m")
		ql := &querylog.Result{
			Configured:        true,
			RequestedWindow:   20 * time.Minute,
			EarliestTimestamp: now.Add(-20 * time.Minute),
			LatestTimestamp:   now,
			ObservedDates:     map[string]int{"2026-11-25": 20},
			TotalEntries:      20,
			APIUserCount:      20,
			Entries: []querylog.QueryLogEntry{{
				Query:     "victim_m",
				Timestamp: now.Add(-30 * time.Second),
				Origin:    metrics.OriginAPIUser,
				Accesses:  exVic.Accesses,
			}},
			Refs: []metrics.UsageRef{{
				MetricName: "live_m", Origin: metrics.OriginAPIUser, SourceID: "querylog:api-user",
				Timestamp: now, PreservesAll: exLive.Accesses[0].PreservesAll,
			}},
		}

		// Regret fires: sets PendingRevertFamilies=[victim_m]; ghost_zero_series must NOT enter tranches!
		repReg, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: inv, QueryLog: ql, Capabilities: alloyCaps(), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(repReg.Regrets) != 1 || len(repReg.Safety.PendingRevertFamilies) != 1 || repReg.Safety.PendingRevertFamilies[0] != "victim_m" {
			t.Fatalf("expected regret + PendingRevertFamilies=[victim_m], got regrets=%+v pending=%v", repReg.Regrets, repReg.Safety.PendingRevertFamilies)
		}
		for _, tr := range repReg.Tranches {
			for _, it := range tr.Items {
				if it.FamilyName == "ghost_zero_series" {
					t.Fatal("0-series historical family must never enter drop tranches")
				}
			}
		}

		// Telemetry returns in inventory -> PendingRevertFamilies clears automatically, while blocked_regret awaits --resolve-regret.
		invRestored := &prometheus.InventorySnapshot{
			MetricNames:    []string{"live_m", "dead_m", "victim_m"},
			SeriesCounts:   map[string]int{"live_m": 800, "dead_m": 100, "victim_m": 50},
			FirstSeenHints: map[string]time.Time{"live_m": old, "dead_m": old, "victim_m": old},
		}
		qlClean := &querylog.Result{
			Configured:        true,
			RequestedWindow:   20 * time.Minute,
			EarliestTimestamp: now.Add(-20 * time.Minute),
			LatestTimestamp:   now,
			ObservedDates:     map[string]int{"2026-11-25": 20},
			TotalEntries:      20,
			APIUserCount:      20,
			Refs: []metrics.UsageRef{{
				MetricName: "live_m", Origin: metrics.OriginAPIUser, SourceID: "querylog:api-user",
				Timestamp: now, PreservesAll: exLive.Accesses[0].PreservesAll,
			}},
		}
		repPostRevert, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: invRestored, QueryLog: qlClean, Capabilities: alloyCaps(), Now: now.Add(30 * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(repPostRevert.Safety.PendingRevertFamilies) != 0 || repPostRevert.Safety.ActiveTrancheStatus != "blocked_regret" {
			t.Fatalf("expected PendingRevertFamilies cleared while still blocked_regret, got %+v", repPostRevert.Safety)
		}

		// --resolve-regret clears UnresolvedRegrets and unblocks status while victim_m stays pinned.
		repResolved, err := Analyze(context.Background(), AnalyzeInput{
			Config: cfg, Ledger: ledger, Inventory: invRestored, QueryLog: qlClean, Capabilities: alloyCaps(),
			Now: now.Add(time.Minute), ResolveRegret: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if repResolved.Safety.UnresolvedRegrets != 0 || repResolved.Safety.ActiveTrancheStatus == "blocked_regret" {
			t.Fatalf("expected regret resolved, got %+v", repResolved.Safety)
		}
		if !ledger.IsPinnedNeverDrop("victim_m") {
			t.Fatal("victim_m must remain pinned to neverDrop")
		}
	})
}
