package otel

import (
	"fmt"
	"sort"
	"strings"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/synthesizer"
)

// Synthesizer generates an OpenTelemetry Collector `--config` overlay (`trimtab-rules.yaml`)
// defining only `processors.filter/trimtab` (NEVER a `service:` block) per DESIGN §7.2.
type Synthesizer struct{}

func New() *Synthesizer {
	return &Synthesizer{}
}

func (s *Synthesizer) Name() string {
	return "otel"
}

func (s *Synthesizer) Capabilities() metrics.Capabilities {
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

// Synthesize emits `trimtab-rules.yaml`:
//   - NEVER defines a `service:` block (OTel --config merge overwrites pipeline lists).
//   - NEVER heuristically reverse-translates Prometheus names to OTLP names.
//   - Skips bucket-only drops and ambiguously translated names with advisory warnings.
//   - Emits a valid non-matching OTTL predicate ('name == "__trimtab_noop__"' ) when 0 metrics are dropped.
func (s *Synthesizer) Synthesize(report *metrics.AuditReport) (*synthesizer.GeneratedArtifact, error) {
	if report == nil {
		return nil, fmt.Errorf("otel synthesizer requires non-nil AuditReport")
	}

	auditDate := report.GeneratedAt.UTC().Format("2006-01-02")
	if report.GeneratedAt.IsZero() {
		auditDate = "unknown"
	}

	drops := append([]metrics.Recommendation(nil), report.ActiveDrops...)
	sort.Slice(drops, func(i, j int) bool {
		if drops[i].Series != drops[j].Series {
			return drops[i].Series > drops[j].Series
		}
		return drops[i].FamilyName < drops[j].FamilyName
	})

	type verifiedDrop struct {
		otlpName string
		rec      metrics.Recommendation
	}
	var verified []verifiedDrop
	var skipped []string
	droppedSeries := 0

	for _, rec := range drops {
		if rec.AdvisoryOnly {
			continue
		}
		if rec.Action == metrics.ActionDropHistogramBuckets {
			skipped = append(skipped, fmt.Sprintf(
				"skipped %s: OTel filter processor drops whole instruments and cannot drop classic _bucket series independently",
				rec.FamilyName,
			))
			continue
		}
		if rec.Action != metrics.ActionDropFamily {
			continue
		}

		otlpName, ok := resolveDeterministicOTLPName(rec)
		if !ok {
			skipped = append(skipped, fmt.Sprintf(
				"skipped %s: metric name cannot be deterministically verified as an OTLP instrument name (namingMode=%q); refusing lossy reverse-translation — provide inventory.otlpNameMap or use prometheus_receiver/utf8_no_translation",
				rec.FamilyName, rec.NamingMode,
			))
			continue
		}

		verified = append(verified, verifiedDrop{otlpName: otlpName, rec: rec})
		droppedSeries += rec.Series
	}

	var b strings.Builder
	b.WriteString("# Managed by Trimtab — DO NOT EDIT MANUALLY\n")
	b.WriteString("# Never defines `service:` so user pipeline lists are never overwritten.\n")
	if report.Safety.ReportOnly || report.Safety.Blocked {
		b.WriteString(fmt.Sprintf(
			"# Audit: %s | Coverage: %.0f/%.0fd | Mode: NO-OP (0 drops synthesized) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays, len(report.Regrets),
		))
	} else {
		b.WriteString(fmt.Sprintf(
			"# Audit: %s | Coverage: %.0f/%.0fd | Tranche %d of %d (%.1f%% series) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays,
			report.Summary.ActiveTranche, report.Summary.TotalTranches,
			report.Summary.SynthesizedPercent, len(report.Regrets),
		))
	}

	b.WriteString("processors:\n")
	b.WriteString("  filter/trimtab:\n")
	b.WriteString("    error_mode: ignore\n")
	b.WriteString("    metrics:\n")
	b.WriteString("      metric:\n")

	if len(verified) == 0 {
		b.WriteString("        # No active drops — valid non-matching OTTL predicate (never empty {})\n")
		b.WriteString("        - 'name == \"__trimtab_noop__\"'\n")
	} else {
		for _, v := range verified {
			b.WriteString(fmt.Sprintf(
				"        # %s | %d series | Tranche %d of %d\n",
				v.rec.Reason, v.rec.Series,
				report.Summary.ActiveTranche, report.Summary.TotalTranches,
			))
			escaped := strings.ReplaceAll(v.otlpName, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			b.WriteString(fmt.Sprintf("        - 'name == \"%s\"'\n", escaped))
		}
	}

	return &synthesizer.GeneratedArtifact{
		Filename:          "trimtab-rules.yaml",
		Content:           b.String(),
		Format:            "yaml",
		DroppedFamilies:   len(verified),
		DroppedSeries:     droppedSeries,
		SkippedAdvisories: skipped,
	}, nil
}

func resolveDeterministicOTLPName(rec metrics.Recommendation) (string, bool) {
	switch rec.NamingMode {
	case metrics.NamingExplicitOTLP:
		if rec.OTLPName != "" {
			return rec.OTLPName, true
		}
		return "", false
	case metrics.NamingPrometheusReceiver, metrics.NamingUTF8NoTranslation:
		if rec.FamilyName != "" {
			return rec.FamilyName, true
		}
		return "", false
	default:
		// NamingTranslatedAmbiguous or unspecified: NEVER reverse-guess OTLP names!
		return "", false
	}
}
