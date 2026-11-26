package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"trimtab.io/trimtab/pkg/metrics"
)

// Format controls how an AuditReport is rendered.
type Format string

const (
	FormatTable    Format = "table"
	FormatMarkdown Format = "markdown"
	FormatJSON     Format = "json"
	FormatHTML     Format = "html"
)

// Render formats an AuditReport as a screenshot-able terminal table, Markdown, JSON, or standalone HTML.
func Render(rep *metrics.AuditReport, format Format) (string, error) {
	if rep == nil {
		return "", fmt.Errorf("cannot render nil AuditReport")
	}
	switch Format(strings.ToLower(string(format))) {
	case "", FormatTable:
		return renderTable(rep), nil
	case FormatMarkdown:
		return renderMarkdown(rep), nil
	case FormatJSON:
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case FormatHTML:
		return renderHTML(rep)
	default:
		return "", fmt.Errorf("unsupported report format %q (expected table, markdown, json, or html)", format)
	}
}

func renderTable(rep *metrics.AuditReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TRIMTAB AUDIT REPORT — %s (single-tenant | window: %s)\n", rep.Name, formatWindowLabel(rep.WindowDays))
	if rep.Safety.QueryLogConfigured {
		fmt.Fprintf(&b, "Query Log Coverage: %s (%s api-user queries | %s rule-evals filtered",
			formatCoverageRatio(rep.Safety.QueryLogCoveredDays, rep.Safety.RequiredWindowDays),
			formatInt(rep.Safety.APIUserQueries),
			formatInt(rep.Safety.RuleEvalQueries),
		)
		if rep.Safety.ExcludedQueries > 0 {
			fmt.Fprintf(&b, " | %s excluded", formatInt(rep.Safety.ExcludedQueries))
		}
		b.WriteString(")\n")
	} else {
		b.WriteString("Query Log Coverage: NONE CONFIGURED (Report-Only mode — no drops synthesized)\n")
	}
	b.WriteString(strings.Repeat("=", 89) + "\n")

	fmt.Fprintf(&b, "Metric Families: %6s   | Active Series: %s\n",
		formatInt(rep.Summary.TotalFamilies),
		formatInt(rep.Summary.TotalActiveSeries),
	)
	fmt.Fprintf(&b, "Droppable Total: %6s   | Droppable Series: %s (%.1f%%)   [%s confidence]\n",
		formatInt(rep.Summary.DroppableFamilies),
		formatInt(rep.Summary.DroppableSeries),
		rep.Summary.DroppablePercent,
		rep.Summary.Confidence,
	)

	if rep.Safety.ReportOnly || rep.Safety.Blocked {
		fmt.Fprintf(&b, "Active Tranche:  HOLD     | Synthesized Now:  0 (0.0%%)       [cap: %.1f%%/tranche]\n",
			rep.Summary.MaxDropPercentPerTranche,
		)
	} else {
		fmt.Fprintf(&b, "Active Tranche:  %d/%-5d | Synthesized Now:  %s (%.1f%%)   [cap: %.1f%%/tranche]\n",
			rep.Summary.ActiveTranche,
			maxInt(rep.Summary.TotalTranches, rep.Summary.ActiveTranche),
			formatInt(rep.Summary.SynthesizedSeries),
			rep.Summary.SynthesizedPercent,
			rep.Summary.MaxDropPercentPerTranche,
		)
	}

	wildcardSummary := formatWildcardSummary(rep.WildcardSources)
	fmt.Fprintf(&b, "Protected (Safe): %5s   | Wildcard Exclusions: %s\n",
		formatInt(rep.Summary.ProtectedFamilies),
		wildcardSummary,
	)

	// Safety Gate Alerts
	if len(rep.Safety.BlockReasons) > 0 {
		b.WriteString("\nSAFETY GATE (SYNTHESIS HELD — PASS-THROUGH / NO-OP ONLY)\n")
		for _, r := range rep.Safety.BlockReasons {
			fmt.Fprintf(&b, "  ! %s\n", r)
		}
	}
	if len(rep.Safety.Warnings) > 0 {
		b.WriteString("\nWARNINGS\n")
		for _, w := range rep.Safety.Warnings {
			fmt.Fprintf(&b, "  * %s\n", w)
		}
	}

	// Active Tranche Drops OR Next Eligible Tranche (observation-only) OR Projected Drops (Report-Only)
	if len(rep.ActiveDrops) > 0 {
		statusSuffix := ""
		if rep.Safety.ActiveTrancheStatus != "" {
			statusSuffix = fmt.Sprintf(" [%s]", rep.Safety.ActiveTrancheStatus)
		}
		fmt.Fprintf(&b, "\nTRANCHE %d DROPS%s (0 live consumers in %.0fd — sorted by largest series savings)\n",
			rep.Summary.ActiveTranche, statusSuffix, rep.WindowDays,
		)
		writeDropsTable(&b, rep.ActiveDrops)
	} else if len(rep.ProjectedDrops) > 0 {
		fmt.Fprintf(&b, "\nPROJECTED DROPS (Report-Only — 0 synthesized until safety gate clears)\n")
		writeDropsTable(&b, rep.ProjectedDrops)
	} else if !rep.Safety.ReportOnly && !rep.Safety.Blocked && len(rep.Tranches) > 0 && rep.Safety.NextTrancheReady {
		nextT := rep.Tranches[0]
		fmt.Fprintf(&b, "\nNEXT ELIGIBLE TRANCHE (Tranche %d/%d — observation-only; run 'trimtab synth --activate' to activate)\n",
			nextT.Number, rep.Summary.TotalTranches,
		)
		writeDropsTable(&b, nextT.Items)
	}

	// Capability Advisories (e.g., OTel bucket-only or skipped unverified names)
	if len(rep.CapabilityAdvisories) > 0 || len(rep.SynthesizerWarnings) > 0 {
		fmt.Fprintf(&b, "\nADVISORY: TARGET CAPABILITY NOTES (%s)\n", rep.Target)
		for _, adv := range rep.CapabilityAdvisories {
			fmt.Fprintf(&b, "  - %s (%s series): %s\n", adv.FamilyName, formatInt(adv.Series), adv.AdvisoryNote)
		}
		for _, sw := range rep.SynthesizerWarnings {
			fmt.Fprintf(&b, "  - %s\n", sw)
		}
	}

	// Dead Recording Rules Advisory
	if len(rep.DeadRecordingRules) > 0 {
		b.WriteString("\nADVISORY: DEAD RECORDING RULES TO DELETE (ruler bypasses collector — delete in rule files)\n")
		var tb strings.Builder
		tw := tabwriter.NewWriter(&tb, 0, 2, 3, ' ', 0)
		fmt.Fprintln(tw, "  RULE NAME\tFILE\tGROUP\tNOTE")
		for _, dr := range rep.DeadRecordingRules {
			note := "delete in rule file"
			if dr.HasOrVectorZero {
				note = "has `or vector(0)` — keeps emitting zeros until deleted"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", dr.RuleName, dr.File, dr.Group, note)
		}
		_ = tw.Flush()
		b.WriteString(tb.String())
	}

	// High-Cardinality Unused Labels Advisory (honest without collapse)
	if len(rep.LabelCollapseAdvisories) > 0 {
		b.WriteString("\nADVISORY: HIGH-CARDINALITY UNUSED LABELS (read-only — requires shard-safe aggregation)\n")
		var tb strings.Builder
		tw := tabwriter.NewWriter(&tb, 0, 2, 3, ' ', 0)
		fmt.Fprintln(tw, "  METRIC FAMILY\tUNUSED LABELS\tTODAY -> COLLAPSED\tSAVINGS")
		for _, adv := range rep.LabelCollapseAdvisories {
			if adv.CombinedEstimate != nil {
				est := adv.CombinedEstimate
				savingsStr := fmt.Sprintf("-%.1f%%", est.SavingsPercent)
				if est.SavingsPercent == 0 {
					savingsStr = "0.0% (no collapse)"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s -> %s\t%s\n",
					adv.TargetMetric,
					strings.Join(est.DroppedLabels, ", "),
					formatInt(est.CurrentSeries),
					formatInt(est.CollapsedSeries),
					savingsStr,
				)
			}
			// Show honest single-label no-collapse demonstrations (or notable single labels)
			for _, s := range adv.SingleLabelEstimates {
				if adv.CombinedEstimate != nil && len(adv.CombinedEstimate.DroppedLabels) == 1 {
					continue
				}
				// Highlight single labels that produce 0% collapse so operators see why single-label drops fail!
				if s.SavingsPercent == 0 {
					fmt.Fprintf(tw, "  %s\t%s (alone)\t%s -> %s\t0.0%% (no collapse)\n",
						adv.TargetMetric,
						strings.Join(s.DroppedLabels, ", "),
						formatInt(s.CurrentSeries),
						formatInt(s.CollapsedSeries),
					)
					break // show one representative 0%-collapse single label per family to keep table crisp
				}
			}
		}
		_ = tw.Flush()
		b.WriteString(tb.String())
	}

	// Regret Check (Forward-Only)
	b.WriteString("\n")
	if len(rep.Regrets) == 0 {
		fmt.Fprintf(&b, "REGRET CHECK (forward-only): 0 regrets since last tranche (watching %d active drop families)\n",
			rep.Summary.ActiveDropWatchCount,
		)
	} else {
		fmt.Fprintf(&b, "REGRET CHECK (forward-only): %d REGRET(S) DETECTED — un-dropped & pinned to neverDrop, next tranche BLOCKED\n",
			len(rep.Regrets),
		)
		for _, reg := range rep.Regrets {
			fmt.Fprintf(&b, "  ! %s (%d api-user hit(s), query=%q) -> %s\n",
				reg.FamilyName, reg.HitCount, reg.Query, reg.ActionTaken,
			)
		}
	}

	// Synthesizer Footer
	if rep.SynthesizedOutputPath != "" {
		b.WriteString("\n")
		label := "Alloy module"
		switch strings.ToLower(rep.Target) {
		case "otel":
			label = "OTel Collector rules"
		case "promrw", "prometheus-remote-write":
			label = "Prometheus remote_write rules"
		}
		if rep.Safety.ReportOnly || rep.Safety.Blocked {
			fmt.Fprintf(&b, "Synthesized %s -> %s (pass-through / no-op: 0 families dropped)\n",
				label, rep.SynthesizedOutputPath,
			)
		} else {
			fmt.Fprintf(&b, "Synthesized %s -> %s (tranche %d/%d: %d families, %s series)\n",
				label, rep.SynthesizedOutputPath,
				rep.Summary.ActiveTranche,
				maxInt(rep.Summary.TotalTranches, rep.Summary.ActiveTranche),
				rep.Summary.SynthesizedFamilies,
				formatInt(rep.Summary.SynthesizedSeries),
			)
		}
	}

	if !rep.Safety.ReportOnly && !rep.Safety.Blocked {
		if rep.Safety.PendingConfirmation {
			fmt.Fprintf(&b, "Tranche %d/%d is awaiting confirmation (waiting for dropped series to reach 0 in inventory, or pass --confirm-tranche).\n",
				rep.Summary.ActiveTranche, maxInt(rep.Summary.TotalTranches, rep.Summary.ActiveTranche),
			)
		} else if rep.Safety.NextTrancheReady && len(rep.Tranches) > 0 {
			nextT := rep.Tranches[0]
			fmt.Fprintf(&b, "Next eligible tranche (%d/%d) is ready (observation-only; run 'trimtab synth --activate' to activate).\n",
				nextT.Number, maxInt(rep.Summary.TotalTranches, nextT.Number),
			)
		} else if rep.Summary.TotalTranches > rep.Summary.ActiveTranche {
			nextTranche := rep.Summary.ActiveTranche + 1
			if rep.Safety.UnresolvedRegrets > 0 {
				if len(rep.Safety.PendingRevertFamilies) > 0 {
					fmt.Fprintf(&b, "Next tranche (%d/%d) BLOCKED until regrets are reviewed (awaiting collector revert for %v; apply reverted rules then pass --resolve-regret).\n",
						nextTranche, rep.Summary.TotalTranches, rep.Safety.PendingRevertFamilies)
				} else {
					fmt.Fprintf(&b, "Next tranche (%d/%d) BLOCKED until regrets are reviewed (revert confirmed; run 'trimtab analyze --resolve-regret' to unblock).\n",
						nextTranche, rep.Summary.TotalTranches)
				}
			} else if rep.Safety.SoakBlocked {
				fmt.Fprintf(&b, "Next tranche (%d/%d) soaking (%s remaining with 0 regrets).\n",
					nextTranche, rep.Summary.TotalTranches, formatDurationShort(rep.Safety.SoakRemaining),
				)
			} else {
				fmt.Fprintf(&b, "Next tranche (%d/%d) unlocks after %s soak with 0 regrets.\n",
					nextTranche, rep.Summary.TotalTranches, formatDurationShort(rep.SoakWindow),
				)
			}
		}
	}

	return b.String()
}

func writeDropsTable(b *strings.Builder, drops []metrics.Recommendation) {
	var tb strings.Builder
	tw := tabwriter.NewWriter(&tb, 0, 2, 3, ' ', 0)
	fmt.Fprintln(tw, "  METRIC FAMILY\tTYPE\tACTION\tSERIES\tREASON")
	for _, d := range drops {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			d.FamilyName,
			d.MetricType,
			d.Action,
			formatInt(d.Series),
			d.Reason,
		)
	}
	_ = tw.Flush()
	b.WriteString(tb.String())
}

func renderMarkdown(rep *metrics.AuditReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Trimtab Audit Report — `%s`\n\n", rep.Name)
	fmt.Fprintf(&b, "- **Scope:** Single-tenant, global per metric family | **Window:** `%s` | **Target:** `%s`\n", formatWindowLabel(rep.WindowDays), rep.Target)
	if rep.Safety.QueryLogConfigured {
		fmt.Fprintf(&b, "- **Query Log Coverage:** `%s` (%s `api-user` queries, %s `rule-eval` filtered)\n",
			formatCoverageRatio(rep.Safety.QueryLogCoveredDays, rep.Safety.RequiredWindowDays),
			formatInt(rep.Safety.APIUserQueries), formatInt(rep.Safety.RuleEvalQueries),
		)
	} else {
		b.WriteString("- **Query Log Coverage:** None configured (**Report-Only mode**)\n")
	}

	b.WriteString("\n## Summary\n\n")
	b.WriteString("| Metric | Value |\n| :--- | :--- |\n")
	fmt.Fprintf(&b, "| Metric Families | %s |\n", formatInt(rep.Summary.TotalFamilies))
	fmt.Fprintf(&b, "| Active Series | %s |\n", formatInt(rep.Summary.TotalActiveSeries))
	fmt.Fprintf(&b, "| Droppable Total | %s families / %s series (%.1f%%) [%s] |\n",
		formatInt(rep.Summary.DroppableFamilies), formatInt(rep.Summary.DroppableSeries), rep.Summary.DroppablePercent, rep.Summary.Confidence)
	fmt.Fprintf(&b, "| Active Tranche | %d/%d (%s series, %.1f%%; cap %.1f%%) |\n",
		rep.Summary.ActiveTranche, maxInt(rep.Summary.TotalTranches, rep.Summary.ActiveTranche),
		formatInt(rep.Summary.SynthesizedSeries), rep.Summary.SynthesizedPercent, rep.Summary.MaxDropPercentPerTranche)
	fmt.Fprintf(&b, "| Protected (Safe) | %s families |\n", formatInt(rep.Summary.ProtectedFamilies))
	fmt.Fprintf(&b, "| Wildcard Exclusions | %s |\n", formatWildcardSummary(rep.WildcardSources))

	if len(rep.Safety.BlockReasons) > 0 {
		b.WriteString("\n## Safety Gate Blocks\n\n")
		for _, r := range rep.Safety.BlockReasons {
			fmt.Fprintf(&b, "- **BLOCKED:** %s\n", r)
		}
	}

	drops := rep.ActiveDrops
	heading := fmt.Sprintf("Tranche %d Drops", rep.Summary.ActiveTranche)
	if rep.Safety.ActiveTrancheStatus != "" && len(drops) > 0 {
		heading = fmt.Sprintf("Tranche %d Drops (%s)", rep.Summary.ActiveTranche, rep.Safety.ActiveTrancheStatus)
	}
	if len(drops) == 0 && len(rep.ProjectedDrops) > 0 {
		drops = rep.ProjectedDrops
		heading = "Projected Drops (Report-Only)"
	} else if len(drops) == 0 && !rep.Safety.ReportOnly && !rep.Safety.Blocked && len(rep.Tranches) > 0 && rep.Safety.NextTrancheReady {
		drops = rep.Tranches[0].Items
		heading = fmt.Sprintf("Next Eligible Tranche (%d/%d — Observation-Only)", rep.Tranches[0].Number, rep.Summary.TotalTranches)
	}
	if len(drops) > 0 {
		fmt.Fprintf(&b, "\n## %s\n\n", heading)
		b.WriteString("| Metric Family | Type | Action | Series | Reason |\n| :--- | :--- | :--- | ---: | :--- |\n")
		for _, d := range drops {
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s | %s |\n", d.FamilyName, d.MetricType, d.Action, formatInt(d.Series), d.Reason)
		}
	}

	if len(rep.DeadRecordingRules) > 0 {
		b.WriteString("\n## Advisory: Dead Recording Rules to Delete\n\n")
		b.WriteString("| Rule Name | File | Group | Note |\n| :--- | :--- | :--- | :--- |\n")
		for _, dr := range rep.DeadRecordingRules {
			fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n", dr.RuleName, dr.File, dr.Group, dr.Note)
		}
	}

	if len(rep.LabelCollapseAdvisories) > 0 {
		b.WriteString("\n## Advisory: High-Cardinality Unused Labels (Read-Only)\n\n")
		b.WriteString("| Metric | Unused Labels | Today -> Collapsed | Savings |\n| :--- | :--- | ---: | ---: |\n")
		for _, adv := range rep.LabelCollapseAdvisories {
			if adv.CombinedEstimate != nil {
				e := adv.CombinedEstimate
				fmt.Fprintf(&b, "| `%s` | `%s` | %s -> %s | -%.1f%% |\n",
					adv.TargetMetric, strings.Join(e.DroppedLabels, ", "), formatInt(e.CurrentSeries), formatInt(e.CollapsedSeries), e.SavingsPercent)
			}
			for _, s := range adv.SingleLabelEstimates {
				if adv.CombinedEstimate != nil && len(adv.CombinedEstimate.DroppedLabels) == 1 {
					continue
				}
				if s.SavingsPercent == 0 {
					fmt.Fprintf(&b, "| `%s` | `%s` (alone) | %s -> %s | 0.0%% (no collapse) |\n",
						adv.TargetMetric, strings.Join(s.DroppedLabels, ", "), formatInt(s.CurrentSeries), formatInt(s.CollapsedSeries))
					break
				}
			}
		}
	}

	b.WriteString("\n## Regret Check (Forward-Only)\n\n")
	if len(rep.Regrets) == 0 {
		fmt.Fprintf(&b, "0 regrets detected (watching %d active drop families).\n", rep.Summary.ActiveDropWatchCount)
	} else {
		for _, r := range rep.Regrets {
			fmt.Fprintf(&b, "- **REGRET:** `%s` (%d hits, query `%s`) — %s\n", r.FamilyName, r.HitCount, r.Query, r.ActionTaken)
		}
	}
	return b.String()
}

func formatWildcardSummary(wcs []metrics.WildcardSource) string {
	var excluded []string
	var active []string
	for _, w := range wcs {
		if w.Excluded {
			excluded = append(excluded, fmt.Sprintf("%q", w.SourceName))
		} else {
			active = append(active, fmt.Sprintf("%q (UNEXCLUDED %.0f%%)", w.SourceName, w.MatchPercent))
		}
	}
	if len(excluded) == 0 && len(active) == 0 {
		return "0"
	}
	var parts []string
	if len(excluded) > 0 {
		parts = append(parts, fmt.Sprintf("%d excluded (%s)", len(excluded), strings.Join(excluded, ", ")))
	}
	if len(active) > 0 {
		parts = append(parts, fmt.Sprintf("%d flagged (%s)", len(active), strings.Join(active, ", ")))
	}
	return strings.Join(parts, "; ")
}

func formatInt(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func formatDurationShort(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d >= 24*time.Hour {
		days := d.Hours() / 24.0
		if days == float64(int(days)) {
			return fmt.Sprintf("%dd", int(days))
		}
		return fmt.Sprintf("%.1fd", days)
	}
	return d.Round(time.Minute).String()
}

func formatWindowLabel(days float64) string {
	if days > 0 && days < 1 {
		return time.Duration(days * float64(24*time.Hour)).Round(time.Second).String()
	}
	return fmt.Sprintf("%.0fd", days)
}

func formatCoverageRatio(coveredDays, requiredDays float64) string {
	if requiredDays > 0 && requiredDays < 1 {
		covDur := time.Duration(coveredDays * float64(24*time.Hour)).Round(time.Second)
		reqDur := time.Duration(requiredDays * float64(24*time.Hour)).Round(time.Second)
		return fmt.Sprintf("%s/%s", covDur, reqDur)
	}
	return fmt.Sprintf("%.0f/%.0f days", coveredDays, requiredDays)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
