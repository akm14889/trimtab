package alloy

import (
	"fmt"
	"strings"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/synthesizer"
)

// Synthesizer generates a Grafana Alloy `import.file` module (`trimtab.alloy`)
// implementing the one-time insertion point contract in DESIGN §7.1.
type Synthesizer struct{}

func New() *Synthesizer {
	return &Synthesizer{}
}

func (s *Synthesizer) Name() string {
	return "alloy"
}

func (s *Synthesizer) Capabilities() metrics.Capabilities {
	return metrics.Capabilities{
		CanDropFamily:               true,
		CanDropHistogramBucketsOnly: true,
	}
}

// Synthesize produces `trimtab.alloy` defining `declare "drop_unused"` with
// `argument "forward_to"`, `export "receiver"`, and `prometheus.relabel "trimtab_drop"`
// using `forward_to = argument.forward_to.value`.
//
// When 0 metrics are actively dropped (including Report-Only or blocked states),
// `rule { ... }` blocks are omitted so the module is a valid pass-through.
func (s *Synthesizer) Synthesize(report *metrics.AuditReport) (*synthesizer.GeneratedArtifact, error) {
	if report == nil {
		return nil, fmt.Errorf("alloy synthesizer requires non-nil AuditReport")
	}

	auditDate := report.GeneratedAt.UTC().Format("2006-01-02")
	if report.GeneratedAt.IsZero() {
		auditDate = "unknown"
	}

	regexTerms, droppedFamilies, droppedSeries := synthesizer.CollectRelabelTerms(report.ActiveDrops)
	chunks := synthesizer.ChunkRelabelRegexes(regexTerms, synthesizer.DefaultMaxTermsPerRule)

	var b strings.Builder
	b.WriteString("// Managed by Trimtab — DO NOT EDIT MANUALLY\n")
	if report.Safety.ReportOnly || report.Safety.Blocked {
		fmt.Fprintf(&b,
			"// Audit: %s | Coverage: %.0f/%.0fd | Mode: PASS-THROUGH (0 drops synthesized) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays, len(report.Regrets),
		)
	} else {
		fmt.Fprintf(&b,
			"// Audit: %s | Coverage: %.0f/%.0fd | Tranche %d of %d (%.1f%% series) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays,
			report.Summary.ActiveTranche, report.Summary.TotalTranches,
			report.Summary.SynthesizedPercent, len(report.Regrets),
		)
	}

	b.WriteString("declare \"drop_unused\" {\n")
	b.WriteString("  argument \"forward_to\" {\n")
	b.WriteString("    optional = false\n")
	b.WriteString("  }\n")
	b.WriteString("  export \"receiver\" {\n")
	b.WriteString("    value = prometheus.relabel.trimtab_drop.receiver\n")
	b.WriteString("  }\n\n")
	b.WriteString("  prometheus.relabel \"trimtab_drop\" {\n")
	b.WriteString("    forward_to = argument.forward_to.value\n")

	if len(chunks) == 0 {
		b.WriteString("\n    // No active drop rules (valid pass-through).\n")
	} else {
		b.WriteString("\n    // Prometheus/Alloy relabel regexes are implicitly anchored.\n")
		for _, chunk := range chunks {
			b.WriteString("    rule {\n")
			b.WriteString("      source_labels = [\"__name__\"]\n")
			fmt.Fprintf(&b, "      regex         = %q\n", chunk)
			b.WriteString("      action        = \"drop\"\n")
			b.WriteString("    }\n")
		}
	}

	b.WriteString("  }\n")
	b.WriteString("}\n")

	return &synthesizer.GeneratedArtifact{
		Filename:        "trimtab.alloy",
		Content:         b.String(),
		Format:          "alloy",
		DroppedFamilies: droppedFamilies,
		DroppedSeries:   droppedSeries,
	}, nil
}
