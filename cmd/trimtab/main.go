package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/engine"
	"trimtab.io/trimtab/pkg/inventory/prometheus"
	"trimtab.io/trimtab/pkg/report"
	"trimtab.io/trimtab/pkg/state"
	"trimtab.io/trimtab/pkg/synthesizer"
	"trimtab.io/trimtab/pkg/synthesizer/alloy"
	"trimtab.io/trimtab/pkg/synthesizer/otel"
	"trimtab.io/trimtab/pkg/synthesizer/promrw"
	"trimtab.io/trimtab/pkg/usage/grafana"
	"trimtab.io/trimtab/pkg/usage/promrules"
	"trimtab.io/trimtab/pkg/usage/querylog"
)

var (
	version = "v0.1.0-dev"
	commit  = "local"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "analyze":
		return runAnalyze(args[1:], stdout, stderr, false)
	case "synth":
		return runAnalyze(args[1:], stdout, stderr, true)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "trimtab %s (%s)\n", version, commit)
		return 0
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "trimtab: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Trimtab — Vendor-Neutral Adaptive Telemetry Control Plane

Usage:
  trimtab analyze --config <trimtab.yaml> [flags]   Audit usage, run safety gates, print report, and optionally synthesize rules
  trimtab synth   --config <trimtab.yaml> [flags]   Synthesize collector drop rules (Alloy, OTel Collector, or Prometheus remote_write)
  trimtab version                                   Print Trimtab version

Common Flags:
  --config <path>                Path to trimtab.yaml (default "trimtab.yaml")
  --target <alloy|otel|promrw>   Override synthesizer target ("alloy", "otel", or "promrw")
  --out <path>                   Output file path for synthesized rules (use "-" for stdout in synth mode)
  --format <fmt>                 Report format for analyze: table | markdown | json (default "table")
  --state <path>                 Override State Ledger (.trimtab-state.json) path
  --accept-baseline-shift        Acknowledge >20% source object count drop and update baseline
  --no-state-write               Do not persist updates to the State Ledger (dry-run)
  --now <RFC3339>                Override evaluation timestamp (for deterministic CI / demo runs)`)
}

func runAnalyze(args []string, stdout, stderr io.Writer, synthOnly bool) int {
	cmdName := "analyze"
	if synthOnly {
		cmdName = "synth"
	}
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath          string
		targetOverride      string
		outOverride         string
		formatStr           string
		stateOverride       string
		acceptBaselineShift bool
		noStateWrite        bool
		nowStr              string
	)
	fs.StringVar(&configPath, "config", "trimtab.yaml", "Path to trimtab.yaml")
	fs.StringVar(&targetOverride, "target", "", "Override synthesizer target (alloy|otel|promrw)")
	fs.StringVar(&outOverride, "out", "", "Output path for synthesized drop rules")
	fs.StringVar(&formatStr, "format", "table", "Report format: table, markdown, json")
	fs.StringVar(&stateOverride, "state", "", "Override State Ledger path")
	fs.BoolVar(&acceptBaselineShift, "accept-baseline-shift", false, "Acknowledge baseline drift and update stored counts")
	fs.BoolVar(&noStateWrite, "no-state-write", false, "Do not persist State Ledger updates")
	fs.StringVar(&nowStr, "now", "", "Override current UTC time (RFC3339)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "trimtab: %v\n", err)
		return 1
	}
	if targetOverride != "" {
		cfg.Synthesizer.Target = strings.ToLower(targetOverride)
	}
	if stateOverride != "" {
		cfg.StatePath = stateOverride
	}
	outputPath := cfg.Synthesizer.OutputPath
	if outOverride != "" {
		outputPath = outOverride
	}

	now := time.Now().UTC()
	if nowStr != "" {
		parsed, err := time.Parse(time.RFC3339, nowStr)
		if err != nil {
			fmt.Fprintf(stderr, "trimtab: invalid --now timestamp %q (expected RFC3339): %v\n", nowStr, err)
			return 2
		}
		now = parsed.UTC()
	}

	syn, err := selectSynthesizer(cfg.Synthesizer)
	if err != nil {
		fmt.Fprintf(stderr, "trimtab: %v\n", err)
		return 1
	}

	ledger, err := state.Load(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(stderr, "trimtab: %v\n", err)
		return 1
	}

	ctx := context.Background()
	window := cfg.Policy.MinObservationWindow.Duration()

	// 1. Inventory (fatal if unreadable)
	inv, collapseEval, err := prometheus.Load(ctx, cfg.Inventory, window, now)
	if err != nil {
		fmt.Fprintf(stderr, "trimtab: load inventory: %v\n", err)
		return 1
	}

	// 2. Usage sources (hard fetch errors fail-closed via HardFetchErrors per Invariant #4)
	var hardErrors []string

	qlRes, err := querylog.ReadSources(ctx, cfg.Usage.QueryLogs, cfg.Policy.ExcludeSources, window, now)
	if err != nil {
		hardErrors = append(hardErrors, fmt.Sprintf("query logs: %v", err))
		qlRes = &querylog.Result{Configured: len(cfg.Usage.QueryLogs) > 0}
	}

	var grafRes *grafana.ScanResult
	if cfg.Usage.Grafana != nil {
		grafRes, err = grafana.Scan(ctx, cfg.Usage.Grafana)
		if err != nil {
			hardErrors = append(hardErrors, fmt.Sprintf("grafana: %v", err))
			grafRes = &grafana.ScanResult{}
		}
	}

	var promRulesRes *promrules.ScanResult
	if len(cfg.Usage.PromRules) > 0 {
		promRulesRes, err = promrules.ScanGlobs(cfg.Usage.PromRules)
		if err != nil {
			hardErrors = append(hardErrors, fmt.Sprintf("prometheus rules: %v", err))
			promRulesRes = &promrules.ScanResult{}
		}
	}

	// 3. Run Core Engine
	auditReport, err := engine.Analyze(ctx, engine.AnalyzeInput{
		Config:              cfg,
		Ledger:              ledger,
		Inventory:           inv,
		CollapseEvaluator:   collapseEval,
		QueryLog:            qlRes,
		Grafana:             grafRes,
		PromRules:           promRulesRes,
		Capabilities:        syn.Capabilities(),
		TargetName:          syn.Name(),
		Now:                 now,
		AcceptBaselineShift: acceptBaselineShift,
		HardFetchErrors:     hardErrors,
	})
	if err != nil {
		fmt.Fprintf(stderr, "trimtab: analyze failed: %v\n", err)
		return 1
	}

	// 4. Synthesize Artifact (always in synth mode, or whenever outputPath is set in analyze mode)
	var artifact *synthesizer.GeneratedArtifact
	if synthOnly || outputPath != "" {
		artifact, err = syn.Synthesize(auditReport)
		if err != nil {
			fmt.Fprintf(stderr, "trimtab: synthesize (%s) failed: %v\n", syn.Name(), err)
			return 1
		}
		auditReport.SynthesizerWarnings = append(auditReport.SynthesizerWarnings, artifact.SkippedAdvisories...)

		if outputPath == "-" || (synthOnly && outputPath == "") {
			fmt.Fprint(stdout, artifact.Content)
		} else {
			if err := writeAtomic(outputPath, []byte(artifact.Content)); err != nil {
				fmt.Fprintf(stderr, "trimtab: write artifact %s: %v\n", outputPath, err)
				return 1
			}
			auditReport.SynthesizedOutputPath = outputPath
		}
	}

	// 5. Persist State Ledger (unless --no-state-write)
	if !noStateWrite && cfg.StatePath != "" {
		if err := ledger.Save(cfg.StatePath); err != nil {
			fmt.Fprintf(stderr, "trimtab: save state ledger %s: %v\n", cfg.StatePath, err)
			return 1
		}
	}

	if synthOnly {
		if outputPath != "" && outputPath != "-" {
			fmt.Fprintf(stdout, "Synthesized %s artifact -> %s (%d families, %d series)\n",
				syn.Name(), outputPath, artifact.DroppedFamilies, artifact.DroppedSeries,
			)
		}
		if len(hardErrors) > 0 {
			return 1
		}
		return 0
	}

	rendered, err := report.Render(auditReport, report.Format(formatStr))
	if err != nil {
		fmt.Fprintf(stderr, "trimtab: render report: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, rendered)

	if len(hardErrors) > 0 {
		return 1
	}
	return 0
}

func selectSynthesizer(sc config.SynthesizerConfig) (synthesizer.Synthesizer, error) {
	switch strings.ToLower(strings.TrimSpace(sc.Target)) {
	case "", "alloy":
		return alloy.New(), nil
	case "otel":
		return otel.New(), nil
	case "promrw", "prometheus-remote-write":
		var opts promrw.Options
		if sc.PromRW != nil {
			opts = promrw.Options{
				Mode:              promrw.Mode(sc.PromRW.Mode),
				RemoteWriteName:   sc.PromRW.RemoteWriteName,
				RemoteWriteURL:    sc.PromRW.RemoteWriteURL,
				OperatorKind:      sc.PromRW.OperatorKind,
				OperatorName:      sc.PromRW.OperatorName,
				OperatorNamespace: sc.PromRW.OperatorNamespace,
				MaxTermsPerRule:   sc.PromRW.MaxTermsPerRule,
			}
		}
		return promrw.New(opts), nil
	default:
		return nil, fmt.Errorf("unsupported synthesizer target %q (expected \"alloy\", \"otel\", or \"promrw\")", sc.Target)
	}
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".trimtab-out-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
