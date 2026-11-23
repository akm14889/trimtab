package alloy

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/synthesizer"
)

// Mode controls the output format emitted by the Grafana Alloy synthesizer.
type Mode string

const (
	// ModeModule emits the standalone `declare "drop_unused"` import.file module (default).
	ModeModule Mode = "module"
	// ModeRelabelRules emits bare Alloy `rule { ... }` blocks for Helm values.yaml embedding.
	ModeRelabelRules Mode = "relabel_rules"
	// ModeConfigMap emits a Kubernetes v1/ConfigMap manifest wrapping the module.
	ModeConfigMap Mode = "configmap"
)

// Options configures Grafana Alloy synthesis output modes.
type Options struct {
	Mode               Mode
	ConfigMapName      string
	ConfigMapNamespace string
	ConfigMapKey       string
	MaxTermsPerRule    int
}

// Synthesizer generates a Grafana Alloy `import.file` module (`trimtab.alloy`),
// bare `relabel_rules`, or a Kubernetes `ConfigMap` implementing DESIGN §7.1.
type Synthesizer struct {
	opts Options
}

// New constructs an Alloy Synthesizer with safe defaults (ModeModule).
func New(opts ...Options) *Synthesizer {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Mode == "" {
		o.Mode = ModeModule
	}
	if o.ConfigMapName == "" {
		o.ConfigMapName = "trimtab-alloy-rules"
	}
	if o.ConfigMapKey == "" {
		o.ConfigMapKey = "trimtab.alloy"
	}
	if o.MaxTermsPerRule <= 0 {
		o.MaxTermsPerRule = synthesizer.DefaultMaxTermsPerRule
	}
	return &Synthesizer{opts: o}
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

// Synthesize produces `trimtab.alloy` (or `relabel_rules` / `configmap`).
//
// When 0 metrics are actively dropped (including Report-Only or blocked states),
// `rule { ... }` blocks are omitted so the module/rules remain a valid pass-through.
func (s *Synthesizer) Synthesize(report *metrics.AuditReport) (*synthesizer.GeneratedArtifact, error) {
	if report == nil {
		return nil, fmt.Errorf("alloy synthesizer requires non-nil AuditReport")
	}
	switch s.opts.Mode {
	case ModeModule, ModeRelabelRules, ModeConfigMap:
	default:
		return nil, fmt.Errorf("alloy unsupported mode %q", s.opts.Mode)
	}

	auditDate := report.GeneratedAt.UTC().Format("2006-01-02")
	if report.GeneratedAt.IsZero() {
		auditDate = "unknown"
	}

	regexTerms, droppedFamilies, droppedSeries := synthesizer.CollectRelabelTerms(report.ActiveDrops)
	chunks := synthesizer.ChunkRelabelRegexes(regexTerms, s.opts.MaxTermsPerRule)

	headerComment := buildHeaderComment(report, auditDate)

	switch s.opts.Mode {
	case ModeRelabelRules:
		var b strings.Builder
		b.WriteString("// Managed by Trimtab — DO NOT EDIT MANUALLY\n")
		b.WriteString(headerComment)
		if len(chunks) == 0 {
			b.WriteString("// No active drop rules (valid pass-through).\n")
		} else {
			b.WriteString("// Prometheus/Alloy relabel regexes are implicitly anchored.\n")
			writeAlloyRuleBlocks(&b, "", chunks)
		}
		return &synthesizer.GeneratedArtifact{
			Filename:        "trimtab-relabel-rules.alloy",
			Content:         b.String(),
			Format:          "alloy",
			DroppedFamilies: droppedFamilies,
			DroppedSeries:   droppedSeries,
		}, nil

	case ModeConfigMap:
		moduleContent := renderModule(headerComment, chunks)
		cm := renderConfigMapYAML(s.opts, moduleContent)
		return &synthesizer.GeneratedArtifact{
			Filename:        "trimtab-alloy-configmap.yaml",
			Content:         cm,
			Format:          "yaml",
			DroppedFamilies: droppedFamilies,
			DroppedSeries:   droppedSeries,
		}, nil

	default: // ModeModule
		moduleContent := renderModule(headerComment, chunks)
		return &synthesizer.GeneratedArtifact{
			Filename:        "trimtab.alloy",
			Content:         moduleContent,
			Format:          "alloy",
			DroppedFamilies: droppedFamilies,
			DroppedSeries:   droppedSeries,
		}, nil
	}
}

func buildHeaderComment(report *metrics.AuditReport, auditDate string) string {
	if report.Safety.ReportOnly || report.Safety.Blocked {
		return fmt.Sprintf(
			"// Audit: %s | Coverage: %.0f/%.0fd | Mode: PASS-THROUGH (0 drops synthesized) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays, len(report.Regrets),
		)
	}
	return fmt.Sprintf(
		"// Audit: %s | Coverage: %.0f/%.0fd | Tranche %d of %d (%.1f%% series) | Regrets: %d\n",
		auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays,
		report.Summary.ActiveTranche, report.Summary.TotalTranches,
		report.Summary.SynthesizedPercent, len(report.Regrets),
	)
}

func renderModule(headerComment string, chunks []string) string {
	var b strings.Builder
	b.WriteString("// Managed by Trimtab — DO NOT EDIT MANUALLY\n")
	b.WriteString(headerComment)
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
		writeAlloyRuleBlocks(&b, "    ", chunks)
	}

	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

func writeAlloyRuleBlocks(b *strings.Builder, indent string, chunks []string) {
	for _, chunk := range chunks {
		fmt.Fprintf(b, "%srule {\n", indent)
		fmt.Fprintf(b, "%s  source_labels = [\"__name__\"]\n", indent)
		fmt.Fprintf(b, "%s  regex         = %q\n", indent, chunk)
		fmt.Fprintf(b, "%s  action        = \"drop\"\n", indent)
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

func renderConfigMapYAML(opts Options, moduleContent string) string {
	var b strings.Builder
	b.WriteString("# Managed by Trimtab — DO NOT EDIT MANUALLY\n")
	b.WriteString("apiVersion: v1\n")
	b.WriteString("kind: ConfigMap\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", marshalYAMLScalar(opts.ConfigMapName))
	if opts.ConfigMapNamespace != "" {
		fmt.Fprintf(&b, "  namespace: %s\n", marshalYAMLScalar(opts.ConfigMapNamespace))
	}
	b.WriteString("  labels:\n")
	b.WriteString("    app.kubernetes.io/managed-by: trimtab\n")
	b.WriteString("data:\n")
	fmt.Fprintf(&b, "  %s: |\n", marshalYAMLScalar(opts.ConfigMapKey))
	for _, line := range strings.Split(strings.TrimRight(moduleContent, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
		} else {
			b.WriteString("    " + line + "\n")
		}
	}
	return b.String()
}

func marshalYAMLScalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimRight(string(out), "\n")
}
