package slo

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promql"
)

// ScanResult holds extracted Sloth, Pyrra, and OpenSLO live usage refs and SLO counts.
type ScanResult struct {
	Refs         []metrics.UsageRef
	SLOCount     int
	FilesScanned int
	Warnings     []string
}

var (
	tplBracketDurationRe = regexp.MustCompile(`\[\s*\{\{[^}]+\}\}\s*\]`)
	tplOffsetRe          = regexp.MustCompile(`(?i)\boffset\s+\{\{[^}]+\}\}`)
	tplAnyRe             = regexp.MustCompile(`\{\{[^}]+\}\}`)
)

func sanitizeSLOPromQL(expr string) string {
	s := tplBracketDurationRe.ReplaceAllString(expr, "[5m]")
	s = tplOffsetRe.ReplaceAllString(s, "offset 5m")
	s = tplAnyRe.ReplaceAllString(s, "1")
	return s
}

// Scan scans configured SLO specification globs/directories.
func Scan(cfg *config.SLOConfig) (*ScanResult, error) {
	if cfg == nil || len(cfg.Globs) == 0 {
		return &ScanResult{}, nil
	}
	return ScanGlobs(cfg.Globs)
}

// ScanGlobs expands file globs and directories recursively and extracts Sloth, Pyrra, and OpenSLO specs.
func ScanGlobs(patterns []string) (*ScanResult, error) {
	res := &ScanResult{}
	if len(patterns) == 0 {
		return res, nil
	}
	files, err := expandFiles(patterns)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read slo file %s: %w", f, err)
		}
		res.FilesScanned++
		if err := scanBytes(data, filepath.ToSlash(f), res); err != nil {
			return nil, fmt.Errorf("parse slo YAML %s: %w", f, err)
		}
	}
	return res, nil
}

func expandFiles(patterns []string) ([]string, error) {
	seen := make(map[string]struct{})
	var files []string
	add := func(p string) {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			files = append(files, p)
		}
	}
	for _, pat := range patterns {
		if strings.TrimSpace(pat) == "" {
			continue
		}
		info, err := os.Stat(pat)
		if err == nil && info.IsDir() {
			err := filepath.WalkDir(pat, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				lower := strings.ToLower(d.Name())
				if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".json") {
					add(path)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("walk slo dir %s: %w", pat, err)
			}
			continue
		}
		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, fmt.Errorf("invalid slo glob %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("slo path matched no files: %q", pat)
		}
		for _, m := range matches {
			if minfo, sErr := os.Stat(m); sErr == nil && minfo.IsDir() {
				continue
			}
			add(m)
		}
	}
	sort.Strings(files)
	return files, nil
}

type k8sHead struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Version    string `yaml:"version"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

func scanBytes(data []byte, source string, res *ScanResult) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	docIdx := 0
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		docIdx++
		if node.Kind == 0 || len(node.Content) == 0 {
			continue
		}
		scanNode(&node, source, fmt.Sprintf("doc%d", docIdx), res)
	}
	return nil
}

func scanNode(node *yaml.Node, source, docPath string, res *ScanResult) {
	var head k8sHead
	if err := node.Decode(&head); err != nil {
		return
	}
	if head.Kind == "List" || strings.HasSuffix(head.Kind, "List") {
		var list struct {
			Items []yaml.Node `yaml:"items"`
		}
		if err := node.Decode(&list); err == nil && len(list.Items) > 0 {
			for i := range list.Items {
				scanNode(&list.Items[i], source, fmt.Sprintf("%s.item%d", docPath, i), res)
			}
			return
		}
	}

	switch {
	case (strings.HasPrefix(head.APIVersion, "sloth.slok.dev/") && head.Kind == "PrometheusServiceLevel") ||
		(head.Kind == "" && strings.HasPrefix(head.Version, "prometheus/")):
		scanSloth(node, head, source, docPath, res)
	case strings.HasPrefix(head.APIVersion, "pyrra.dev/") && head.Kind == "ServiceLevelObjective":
		scanPyrra(node, head, source, docPath, res)
	case strings.HasPrefix(head.APIVersion, "openslo") && head.Kind == "SLO":
		scanOpenSLO(node, head, source, docPath, res)
	}
}

type slothSLIItem struct {
	Name string `yaml:"name"`
	SLI  struct {
		Events struct {
			ErrorQuery string `yaml:"errorQuery"`
			TotalQuery string `yaml:"totalQuery"`
		} `yaml:"events"`
		Raw struct {
			ErrorRatioQuery string `yaml:"errorRatioQuery"`
		} `yaml:"raw"`
	} `yaml:"sli"`
}

func scanSloth(node *yaml.Node, head k8sHead, source, docPath string, res *ScanResult) {
	var doc struct {
		Service string         `yaml:"service"`
		SLOs    []slothSLIItem `yaml:"slos"`
		Spec    struct {
			Service string         `yaml:"service"`
			SLOs    []slothSLIItem `yaml:"slos"`
		} `yaml:"spec"`
	}
	if err := node.Decode(&doc); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("sloth %s (%s): decode failed: %v", source, docPath, err))
		return
	}
	service := doc.Spec.Service
	slos := doc.Spec.SLOs
	if len(slos) == 0 {
		service = doc.Service
		slos = doc.SLOs
	}
	if len(slos) == 0 {
		return
	}
	if service == "" {
		service = head.Metadata.Name
	}
	if service == "" {
		service = docPath
	}

	for i, slo := range slos {
		sloName := slo.Name
		if sloName == "" {
			sloName = fmt.Sprintf("slo-%d", i)
		}
		queries := []string{
			strings.TrimSpace(slo.SLI.Events.ErrorQuery),
			strings.TrimSpace(slo.SLI.Events.TotalQuery),
			strings.TrimSpace(slo.SLI.Raw.ErrorRatioQuery),
		}
		hasAny := false
		for _, q := range queries {
			if q != "" {
				hasAny = true
				break
			}
		}
		if !hasAny {
			continue
		}
		res.SLOCount++
		sourceID := fmt.Sprintf("slo:sloth:%s:%s:%s", source, service, sloName)
		sourceName := fmt.Sprintf("Sloth %s/%s", service, sloName)
		for _, rawQ := range queries {
			if rawQ == "" {
				continue
			}
			extracted, err := promql.Extract(sanitizeSLOPromQL(rawQ))
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("sloth %s %s/%s: parse PromQL: %v", source, service, sloName, err))
				continue
			}
			appendAccesses(res, extracted.Accesses, sourceID, sourceName, source, rawQ, nil, true)
		}
	}
}

type pyrraMetricSelector struct {
	Metric string `yaml:"metric"`
}

func scanPyrra(node *yaml.Node, head k8sHead, source, docPath string, res *ScanResult) {
	var doc struct {
		Spec struct {
			Indicator struct {
				Ratio *struct {
					Errors   pyrraMetricSelector `yaml:"errors"`
					Total    pyrraMetricSelector `yaml:"total"`
					Grouping []string            `yaml:"grouping"`
				} `yaml:"ratio"`
				Latency *struct {
					Success  pyrraMetricSelector `yaml:"success"`
					Total    pyrraMetricSelector `yaml:"total"`
					Grouping []string            `yaml:"grouping"`
				} `yaml:"latency"`
				LatencyNative *struct {
					Total    pyrraMetricSelector `yaml:"total"`
					Grouping []string            `yaml:"grouping"`
				} `yaml:"latencyNative"`
				BoolGauge *struct {
					Metric   string   `yaml:"metric"`
					Grouping []string `yaml:"grouping"`
				} `yaml:"bool_gauge"`
				BoolGaugeCamel *struct {
					Metric   string   `yaml:"metric"`
					Grouping []string `yaml:"grouping"`
				} `yaml:"boolGauge"`
			} `yaml:"indicator"`
		} `yaml:"spec"`
	}
	if err := node.Decode(&doc); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("pyrra %s (%s): decode failed: %v", source, docPath, err))
		return
	}

	var selectors []string
	var grouping []string
	ind := doc.Spec.Indicator
	if ind.Ratio != nil {
		selectors = append(selectors, ind.Ratio.Errors.Metric, ind.Ratio.Total.Metric)
		grouping = append(grouping, ind.Ratio.Grouping...)
	}
	if ind.Latency != nil {
		selectors = append(selectors, ind.Latency.Success.Metric, ind.Latency.Total.Metric)
		grouping = append(grouping, ind.Latency.Grouping...)
	}
	if ind.LatencyNative != nil {
		selectors = append(selectors, ind.LatencyNative.Total.Metric)
		grouping = append(grouping, ind.LatencyNative.Grouping...)
	}
	if ind.BoolGauge != nil {
		selectors = append(selectors, ind.BoolGauge.Metric)
		grouping = append(grouping, ind.BoolGauge.Grouping...)
	}
	if ind.BoolGaugeCamel != nil {
		selectors = append(selectors, ind.BoolGaugeCamel.Metric)
		grouping = append(grouping, ind.BoolGaugeCamel.Grouping...)
	}

	var validSelectors []string
	for _, s := range selectors {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			validSelectors = append(validSelectors, trimmed)
		}
	}
	if len(validSelectors) == 0 {
		return
	}
	res.SLOCount++

	name := head.Metadata.Name
	if head.Metadata.Namespace != "" && name != "" {
		name = head.Metadata.Namespace + "/" + name
	} else if name == "" {
		name = docPath
	}
	sourceID := fmt.Sprintf("slo:pyrra:%s:%s", source, name)
	sourceName := fmt.Sprintf("Pyrra %s", name)

	for _, sel := range validSelectors {
		extracted, err := promql.Extract(sanitizeSLOPromQL(sel))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("pyrra %s %s: parse selector %q: %v", source, name, sel, err))
			continue
		}
		// Pyrra wraps bare selectors inside sum by (<grouping>), so only matchers + grouping labels are preserved.
		appendAccesses(res, extracted.Accesses, sourceID, sourceName, source, sel, grouping, false)
	}
}

type openSLOMetricSource struct {
	Type string `yaml:"type"`
	Spec struct {
		Query string `yaml:"query"`
	} `yaml:"spec"`
}

func scanOpenSLO(node *yaml.Node, head k8sHead, source, docPath string, res *ScanResult) {
	var doc struct {
		Spec struct {
			Indicator struct {
				Spec struct {
					ThresholdMetric struct {
						MetricSource openSLOMetricSource `yaml:"metricSource"`
					} `yaml:"thresholdMetric"`
					RatioMetric struct {
						Good struct {
							MetricSource openSLOMetricSource `yaml:"metricSource"`
						} `yaml:"good"`
						Bad struct {
							MetricSource openSLOMetricSource `yaml:"metricSource"`
						} `yaml:"bad"`
						Total struct {
							MetricSource openSLOMetricSource `yaml:"metricSource"`
						} `yaml:"total"`
					} `yaml:"ratioMetric"`
				} `yaml:"spec"`
			} `yaml:"indicator"`
		} `yaml:"spec"`
	}
	if err := node.Decode(&doc); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("openslo %s (%s): decode failed: %v", source, docPath, err))
		return
	}

	sources := []openSLOMetricSource{
		doc.Spec.Indicator.Spec.ThresholdMetric.MetricSource,
		doc.Spec.Indicator.Spec.RatioMetric.Good.MetricSource,
		doc.Spec.Indicator.Spec.RatioMetric.Bad.MetricSource,
		doc.Spec.Indicator.Spec.RatioMetric.Total.MetricSource,
	}
	var queries []string
	for _, ms := range sources {
		if ms.Type != "" && !strings.EqualFold(strings.TrimSpace(ms.Type), "Prometheus") {
			continue
		}
		if q := strings.TrimSpace(ms.Spec.Query); q != "" {
			queries = append(queries, q)
		}
	}
	if len(queries) == 0 {
		return
	}
	res.SLOCount++

	name := head.Metadata.Name
	if name == "" {
		name = docPath
	}
	sourceID := fmt.Sprintf("slo:openslo:%s:%s", source, name)
	sourceName := fmt.Sprintf("OpenSLO %s", name)
	for _, q := range queries {
		extracted, err := promql.Extract(sanitizeSLOPromQL(q))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("openslo %s %s: parse PromQL: %v", source, name, err))
			continue
		}
		appendAccesses(res, extracted.Accesses, sourceID, sourceName, source, q, nil, true)
	}
}

func appendAccesses(
	res *ScanResult,
	accesses []promql.MetricAccess,
	sourceID, sourceName, sourceFolder, query string,
	extraLabels []string,
	preserveAllAllowed bool,
) {
	for _, acc := range accesses {
		preservesAll := acc.PreservesAll
		if !preserveAllAllowed {
			preservesAll = false
		}
		res.Refs = append(res.Refs, metrics.UsageRef{
			MetricName:     acc.MetricName,
			MetricRegex:    acc.MetricRegex,
			IsRegex:        acc.IsRegex,
			Origin:         metrics.OriginSLO,
			SourceID:       sourceID,
			SourceName:     sourceName,
			SourceFolder:   sourceFolder,
			Query:          query,
			ExplicitLabels: mergeStrings(acc.ExplicitLabels, extraLabels),
			PreservesAll:   preservesAll,
			WithoutLabels:  acc.WithoutLabels,
		})
	}
}

func mergeStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		if s != "" && s != "__name__" {
			seen[s] = struct{}{}
		}
	}
	for _, s := range b {
		if s != "" && s != "__name__" {
			seen[s] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
