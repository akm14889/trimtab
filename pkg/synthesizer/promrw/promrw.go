package promrw

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/synthesizer"
)

// Mode controls the YAML structure emitted by the Prometheus remote_write synthesizer.
type Mode string

const (
	// ModeRelabelConfigs emits a top-level `write_relabel_configs` block.
	ModeRelabelConfigs Mode = "relabel_configs"
	// ModeRemoteWrite emits a complete `remote_write` entry containing `write_relabel_configs`.
	ModeRemoteWrite Mode = "remote_write"
	// ModePrometheusOperatorPatch emits a camelCase monitoring.coreos.com/v1 strategic-merge patch.
	ModePrometheusOperatorPatch Mode = "prometheus_operator_patch"
)

// Options configures Prometheus remote_write rule synthesis.
type Options struct {
	Mode              Mode
	RemoteWriteName   string
	RemoteWriteURL    string
	OperatorKind      string
	OperatorName      string
	OperatorNamespace string
	MaxTermsPerRule   int
}

// Synthesizer generates Prometheus remote_write[].write_relabel_configs YAML overlays
// (DESIGN §7.3).
type Synthesizer struct {
	opts Options
}

// New constructs a Prometheus remote_write Synthesizer with safe defaults.
func New(opts ...Options) *Synthesizer {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Mode == "" {
		o.Mode = ModeRelabelConfigs
	}
	if o.OperatorKind == "" {
		o.OperatorKind = "Prometheus"
	}
	if o.OperatorName == "" {
		o.OperatorName = "prometheus"
	}
	if o.MaxTermsPerRule <= 0 {
		o.MaxTermsPerRule = synthesizer.DefaultMaxTermsPerRule
	}
	return &Synthesizer{opts: o}
}

func (s *Synthesizer) Name() string {
	return "promrw"
}

func (s *Synthesizer) Capabilities() metrics.Capabilities {
	return metrics.Capabilities{
		CanDropFamily:               true,
		CanDropHistogramBucketsOnly: true,
	}
}

// Synthesize converts a safety-gated AuditReport into Prometheus remote_write YAML drop rules.
func (s *Synthesizer) Synthesize(report *metrics.AuditReport) (*synthesizer.GeneratedArtifact, error) {
	if report == nil {
		return nil, fmt.Errorf("promrw synthesizer requires non-nil AuditReport")
	}
	if err := s.validateOptions(); err != nil {
		return nil, err
	}

	auditDate := report.GeneratedAt.UTC().Format("2006-01-02")
	if report.GeneratedAt.IsZero() {
		auditDate = "unknown"
	}

	regexTerms, droppedFamilies, droppedSeries := synthesizer.CollectRelabelTerms(report.ActiveDrops)
	chunks := synthesizer.ChunkRelabelRegexes(regexTerms, s.opts.MaxTermsPerRule)

	var b strings.Builder
	b.WriteString("# Managed by Trimtab — DO NOT EDIT MANUALLY\n")
	if s.opts.Mode == ModePrometheusOperatorPatch {
		b.WriteString("# Target: Prometheus Operator CRD patch (monitoring.coreos.com/v1)\n")
	} else {
		b.WriteString("# Target: Prometheus remote_write[].write_relabel_configs (DESIGN §7.3)\n")
	}
	if report.Safety.ReportOnly || report.Safety.Blocked {
		fmt.Fprintf(&b,
			"# Audit: %s | Coverage: %.0f/%.0fd | Mode: PASS-THROUGH (0 drops synthesized) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays, len(report.Regrets),
		)
	} else {
		fmt.Fprintf(&b,
			"# Audit: %s | Coverage: %.0f/%.0fd | Tranche %d of %d (%.1f%% series) | Regrets: %d\n",
			auditDate, report.Safety.QueryLogCoveredDays, report.Safety.RequiredWindowDays,
			report.Summary.ActiveTranche, report.Summary.TotalTranches,
			report.Summary.SynthesizedPercent, len(report.Regrets),
		)
	}

	filename := "trimtab-remote-write.yaml"
	switch s.opts.Mode {
	case ModeRelabelConfigs:
		if len(chunks) == 0 {
			b.WriteString("# No active drop rules (valid pass-through).\n")
			b.WriteString("write_relabel_configs: []\n")
		} else {
			b.WriteString("write_relabel_configs:\n")
			writeRelabelRulesYAML(&b, "  ", "source_labels", chunks)
		}

	case ModeRemoteWrite:
		b.WriteString("remote_write:\n")
		prefix := "  - "
		if s.opts.RemoteWriteName != "" {
			fmt.Fprintf(&b, "%sname: %s\n", prefix, marshalYAMLScalar(s.opts.RemoteWriteName))
			prefix = "    "
		}
		if s.opts.RemoteWriteURL != "" {
			fmt.Fprintf(&b, "%surl: %s\n", prefix, marshalYAMLScalar(s.opts.RemoteWriteURL))
		}
		if len(chunks) == 0 {
			b.WriteString("    # No active drop rules (valid pass-through).\n")
			b.WriteString("    write_relabel_configs: []\n")
		} else {
			b.WriteString("    write_relabel_configs:\n")
			writeRelabelRulesYAML(&b, "      ", "source_labels", chunks)
		}

	case ModePrometheusOperatorPatch:
		filename = "trimtab-prometheus-operator-patch.yaml"
		b.WriteString("apiVersion: monitoring.coreos.com/v1\n")
		fmt.Fprintf(&b, "kind: %s\n", marshalYAMLScalar(s.opts.OperatorKind))
		b.WriteString("metadata:\n")
		fmt.Fprintf(&b, "  name: %s\n", marshalYAMLScalar(s.opts.OperatorName))
		if s.opts.OperatorNamespace != "" {
			fmt.Fprintf(&b, "  namespace: %s\n", marshalYAMLScalar(s.opts.OperatorNamespace))
		}
		b.WriteString("spec:\n")
		b.WriteString("  remoteWrite:\n")
		prefix := "    - "
		if s.opts.RemoteWriteName != "" {
			fmt.Fprintf(&b, "%sname: %s\n", prefix, marshalYAMLScalar(s.opts.RemoteWriteName))
			prefix = "      "
		}
		fmt.Fprintf(&b, "%surl: %s\n", prefix, marshalYAMLScalar(s.opts.RemoteWriteURL))
		if len(chunks) == 0 {
			b.WriteString("      # No active drop rules (valid pass-through).\n")
			b.WriteString("      writeRelabelConfigs: []\n")
		} else {
			b.WriteString("      writeRelabelConfigs:\n")
			writeRelabelRulesYAML(&b, "        ", "sourceLabels", chunks)
		}
	}

	return &synthesizer.GeneratedArtifact{
		Filename:        filename,
		Content:         b.String(),
		Format:          "yaml",
		DroppedFamilies: droppedFamilies,
		DroppedSeries:   droppedSeries,
	}, nil
}

func (s *Synthesizer) validateOptions() error {
	switch s.opts.Mode {
	case ModeRelabelConfigs:
		return nil
	case ModeRemoteWrite:
		if s.opts.RemoteWriteURL == "" && s.opts.RemoteWriteName == "" {
			return fmt.Errorf("promrw mode %q requires RemoteWriteURL or RemoteWriteName", s.opts.Mode)
		}
		return nil
	case ModePrometheusOperatorPatch:
		if s.opts.RemoteWriteURL == "" {
			return fmt.Errorf("promrw mode %q requires RemoteWriteURL", s.opts.Mode)
		}
		if s.opts.OperatorKind != "Prometheus" && s.opts.OperatorKind != "PrometheusAgent" {
			return fmt.Errorf("promrw unsupported OperatorKind %q (expected \"Prometheus\" or \"PrometheusAgent\")", s.opts.OperatorKind)
		}
		return nil
	default:
		return fmt.Errorf("promrw unsupported mode %q", s.opts.Mode)
	}
}

func writeRelabelRulesYAML(b *strings.Builder, indent, sourceLabelsKey string, chunks []string) {
	fmt.Fprintf(b, "%s# Prometheus relabel regexes are implicitly anchored.\n", indent)
	for _, chunk := range chunks {
		fmt.Fprintf(b, "%s- %s:\n", indent, sourceLabelsKey)
		fmt.Fprintf(b, "%s    - __name__\n", indent)
		fmt.Fprintf(b, "%s  regex: %s\n", indent, quoteYAMLString(chunk))
		fmt.Fprintf(b, "%s  action: drop\n", indent)
	}
}

func quoteYAMLString(s string) string {
	node := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Style: yaml.DoubleQuotedStyle,
		Value: s,
	}
	out, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimRight(string(out), "\n")
}

func marshalYAMLScalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return quoteYAMLString(s)
	}
	return strings.TrimRight(string(out), "\n")
}
