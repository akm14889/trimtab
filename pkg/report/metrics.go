package report

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
	"trimtab.io/trimtab/pkg/metrics"
)

// RenderPrometheusMetrics renders Trimtab's operational control-plane metrics in
// Prometheus text exposition format (version 0.0.4).
func RenderPrometheusMetrics(rep *metrics.AuditReport, tenant string) string {
	if rep == nil {
		return ""
	}
	auditName := strings.TrimSpace(rep.Name)
	if auditName == "" {
		auditName = "trimtab"
	}
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		tenant = "default"
	}

	baseLabels := fmt.Sprintf(`name="%s",tenant="%s"`, escapePromLabelValue(auditName), escapePromLabelValue(tenant))
	var b strings.Builder

	writeGauge(&b, "trimtab_active_series", "Total active time series discovered in TSDB inventory.", baseLabels, fmt.Sprintf("%d", rep.Summary.TotalActiveSeries))
	writeGauge(&b, "trimtab_total_families", "Total metric families discovered in TSDB inventory.", baseLabels, fmt.Sprintf("%d", rep.Summary.TotalFamilies))
	writeGauge(&b, "trimtab_protected_families", "Metric families protected from dropping.", baseLabels, fmt.Sprintf("%d", rep.Summary.ProtectedFamilies))
	writeGauge(&b, "trimtab_droppable_families", "Metric families currently eligible for dropping.", baseLabels, fmt.Sprintf("%d", rep.Summary.DroppableFamilies))
	writeGauge(&b, "trimtab_droppable_series", "Total series eligible for dropping across tranches.", baseLabels, fmt.Sprintf("%d", rep.Summary.DroppableSeries))
	writeGauge(&b, "trimtab_droppable_percent", "Percentage of active series eligible for dropping.", baseLabels, fmt.Sprintf("%.2f", rep.Summary.DroppablePercent))
	writeGauge(&b, "trimtab_synthesized_drop_families", "Metric families synthesized into active drop rules.", baseLabels, fmt.Sprintf("%d", rep.Summary.SynthesizedFamilies))
	writeGauge(&b, "trimtab_synthesized_drop_series", "Series synthesized into active drop rules.", baseLabels, fmt.Sprintf("%d", rep.Summary.SynthesizedSeries))
	writeGauge(&b, "trimtab_synthesized_drop_percent", "Percentage of active series synthesized into drop rules.", baseLabels, fmt.Sprintf("%.2f", rep.Summary.SynthesizedPercent))
	writeGauge(&b, "trimtab_active_drop_watch_count", "Active dropped families monitored for forward-only regret.", baseLabels, fmt.Sprintf("%d", rep.Summary.ActiveDropWatchCount))
	writeGauge(&b, "trimtab_active_tranche", "Current active tranche number.", baseLabels, fmt.Sprintf("%d", rep.Summary.ActiveTranche))
	writeGauge(&b, "trimtab_total_tranches", "Total planned and active tranches.", baseLabels, fmt.Sprintf("%d", maxInt(rep.Summary.TotalTranches, rep.Summary.ActiveTranche)))
	writeGauge(&b, "trimtab_query_log_coverage_days", "Observed query-log coverage in days.", baseLabels, fmt.Sprintf("%.6f", rep.Safety.QueryLogCoveredDays))
	writeGauge(&b, "trimtab_required_window_days", "Configured minObservationWindow in days.", baseLabels, fmt.Sprintf("%.6f", rep.Safety.RequiredWindowDays))

	b.WriteString("# HELP trimtab_query_log_queries Query log entries inspected by origin.\n")
	b.WriteString("# TYPE trimtab_query_log_queries gauge\n")
	fmt.Fprintf(&b, "trimtab_query_log_queries{%s,origin=\"api-user\"} %d\n", baseLabels, rep.Safety.APIUserQueries)
	fmt.Fprintf(&b, "trimtab_query_log_queries{%s,origin=\"rule-eval\"} %d\n", baseLabels, rep.Safety.RuleEvalQueries)
	fmt.Fprintf(&b, "trimtab_query_log_queries{%s,origin=\"excluded\"} %d\n", baseLabels, rep.Safety.ExcludedQueries)

	writeGauge(&b, "trimtab_safety_gate_report_only", "1 when Trimtab is holding synthesis in Report-Only mode.", baseLabels, boolGauge(rep.Safety.ReportOnly))
	writeGauge(&b, "trimtab_safety_gate_blocked", "1 when drop synthesis is blocked by a safety gate.", baseLabels, boolGauge(rep.Safety.Blocked))
	writeGauge(&b, "trimtab_baseline_drift_detected", "1 when baseline drift circuit breaker tripped.", baseLabels, boolGauge(rep.Safety.BaselineDriftDetected))
	writeGauge(&b, "trimtab_sanity_ceiling_tripped", "1 when maxTotalDroppablePercent sanity ceiling tripped.", baseLabels, boolGauge(rep.Safety.SanityCeilingTripped))
	writeGauge(&b, "trimtab_soak_blocked", "1 when the next tranche is gated by an active soak window.", baseLabels, boolGauge(rep.Safety.SoakBlocked))
	writeGauge(&b, "trimtab_pending_confirmation", "1 when the active tranche awaits drop confirmation in inventory.", baseLabels, boolGauge(rep.Safety.PendingConfirmation))
	writeGauge(&b, "trimtab_unresolved_regrets", "Count of unresolved regrets blocking tranche progression.", baseLabels, fmt.Sprintf("%d", rep.Safety.UnresolvedRegrets))

	b.WriteString("# HELP trimtab_regret_events_total Forward-only regret events detected during the audit run.\n")
	b.WriteString("# TYPE trimtab_regret_events_total counter\n")
	fmt.Fprintf(&b, "trimtab_regret_events_total{%s} %d\n", baseLabels, len(rep.Regrets))

	writeGauge(&b, "trimtab_dead_recording_rules", "Dead recording rules flagged for deletion.", baseLabels, fmt.Sprintf("%d", len(rep.DeadRecordingRules)))
	writeGauge(&b, "trimtab_label_collapse_advisories", "High-cardinality unused-label advisories emitted.", baseLabels, fmt.Sprintf("%d", len(rep.LabelCollapseAdvisories)))

	tsSec := int64(0)
	if !rep.GeneratedAt.IsZero() {
		tsSec = rep.GeneratedAt.UTC().Unix()
	}
	writeGauge(&b, "trimtab_last_audit_timestamp_seconds", "Unix timestamp when the audit completed.", baseLabels, fmt.Sprintf("%d", tsSec))

	return b.String()
}

// PushPrometheusMetrics pushes Prometheus text exposition metrics to a Prometheus Pushgateway
// via HTTP PUT (/metrics/job/<job>[/instance/<instance>]).
func PushPrometheusMetrics(
	ctx context.Context,
	pushgatewayURL, job, instance, payload string,
	auth config.HTTPAuthConfig,
	timeout time.Duration,
) error {
	base := strings.TrimSpace(pushgatewayURL)
	if base == "" {
		return fmt.Errorf("pushgateway URL must not be empty")
	}
	job = strings.TrimSpace(job)
	if job == "" {
		job = "trimtab"
	}
	endpoint := "/metrics/job/" + url.PathEscape(job)
	if inst := strings.TrimSpace(instance); inst != "" {
		endpoint += "/instance/" + url.PathEscape(inst)
	}

	client, err := httpclient.New(httpclient.Options{
		BaseURL: base,
		Timeout: timeout,
		Auth:    auth,
	})
	if err != nil {
		return fmt.Errorf("pushgateway client: %w", err)
	}
	if _, err := client.Put(ctx, endpoint, "text/plain; version=0.0.4; charset=utf-8", []byte(payload)); err != nil {
		return fmt.Errorf("push metrics to %s: %w", endpoint, err)
	}
	return nil
}

func writeGauge(b *strings.Builder, name, help, labels, val string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s{%s} %s\n", name, labels, val)
}

func boolGauge(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func escapePromLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
