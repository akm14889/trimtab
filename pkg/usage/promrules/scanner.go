package promrules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promql"
)

// RecordingRule captures a Prometheus recording rule for DAG dependency analysis.
type RecordingRule struct {
	Name            string                `json:"name"`
	Expr            string                `json:"expr"`
	File            string                `json:"file"`
	Group           string                `json:"group"`
	Inputs          []promql.MetricAccess `json:"inputs"`
	TemplateLabels  []string              `json:"templateLabels,omitempty"`
	HasOrVectorZero bool                  `json:"hasOrVectorZero"`
}

// ScanResult holds all alerting-rule live refs, recording rules, and object counts for baseline tracking.
type ScanResult struct {
	AlertRefs          []metrics.UsageRef
	RecordingRules     []RecordingRule
	TotalRuleCount     int
	AlertRuleCount     int
	RecordingRuleCount int
	FilesScanned       int
	Warnings           []string
}

type ruleFile struct {
	Groups []ruleGroup `yaml:"groups"`
}

type ruleGroup struct {
	Name  string     `yaml:"name"`
	Rules []ruleItem `yaml:"rules"`
}

type ruleItem struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

var (
	// Matches {{ $labels.foo }}, {{$labels.foo_bar}}, {{ .Labels.foo }}
	goTplDotLabelRe = regexp.MustCompile(`(?:\$labels|\.Labels)\.([a-zA-Z_][a-zA-Z0-9_]*)`)
	// Matches {{ index $labels "foo" }} or {{ $labels["foo"] }}
	goTplIndexLabelRe = regexp.MustCompile(`(?:index\s+\$labels\s+|(?:\$labels|\.Labels)\s*\[\s*)["']([a-zA-Z_][a-zA-Z0-9_]*)["']`)
	// Detects `or vector(0)` / `or on(...) vector(0)` rules that keep emitting zeros after source drop
	orVectorZeroRe = regexp.MustCompile(`(?i)\bor\b[^\n]*\bvector\s*\(\s*0(?:\.0+)?\s*\)`)
)

// HasOrVectorZero reports whether expr contains `or vector(0)` (Invariant #8).
func HasOrVectorZero(expr string) bool {
	return orVectorZeroRe.MatchString(expr)
}

// ExtractTemplateLabels extracts Prometheus Go template label references (Invariant #8).
func ExtractTemplateLabels(texts ...string) []string {
	seen := make(map[string]struct{})
	for _, txt := range texts {
		for _, m := range goTplDotLabelRe.FindAllStringSubmatch(txt, -1) {
			if len(m) > 1 {
				seen[m[1]] = struct{}{}
			}
		}
		for _, m := range goTplIndexLabelRe.FindAllStringSubmatch(txt, -1) {
			if len(m) > 1 {
				seen[m[1]] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// ScanGlobs expands file/directory/glob paths and parses every Prometheus rules YAML file.
func ScanGlobs(patterns []string) (*ScanResult, error) {
	res := &ScanResult{}
	if len(patterns) == 0 {
		return res, nil
	}
	files, err := expandFiles(patterns)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if err := scanFile(file, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func expandFiles(patterns []string) ([]string, error) {
	seen := make(map[string]struct{})
	var files []string
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		info, err := os.Stat(pat)
		if err == nil && info.IsDir() {
			entries, err := os.ReadDir(pat)
			if err != nil {
				return nil, fmt.Errorf("read prom rules dir %s: %w", pat, err)
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := strings.ToLower(e.Name())
				if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
					full := filepath.Join(pat, e.Name())
					if _, ok := seen[full]; !ok {
						seen[full] = struct{}{}
						files = append(files, full)
					}
				}
			}
			continue
		}

		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, fmt.Errorf("invalid prom rules glob %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("prom rules path matched no files: %q", pat)
		}
		for _, m := range matches {
			if _, ok := seen[m]; !ok {
				seen[m] = struct{}{}
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

func scanFile(path string, res *ScanResult) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read prom rules file %s: %w", path, err)
	}
	var rf ruleFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return fmt.Errorf("parse prom rules YAML %s: %w", path, err)
	}
	res.FilesScanned++
	displayFile := filepath.ToSlash(path)

	for _, grp := range rf.Groups {
		for _, rule := range grp.Rules {
			if rule.Expr == "" {
				continue
			}
			var tplTexts []string
			for _, v := range rule.Annotations {
				tplTexts = append(tplTexts, v)
			}
			for _, v := range rule.Labels {
				tplTexts = append(tplTexts, v)
			}
			tplLabels := ExtractTemplateLabels(tplTexts...)

			extracted, err := promql.Extract(rule.Expr)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("promrules %s/%s: failed to parse expr: %v", displayFile, grp.Name, err))
				continue
			}

			if rule.Alert != "" {
				res.TotalRuleCount++
				res.AlertRuleCount++
				sourceID := fmt.Sprintf("prom-alert:%s:%s:%s", displayFile, grp.Name, rule.Alert)
				for _, acc := range extracted.Accesses {
					mergedLabels := mergeUnique(acc.ExplicitLabels, tplLabels)
					res.AlertRefs = append(res.AlertRefs, metrics.UsageRef{
						MetricName:     acc.MetricName,
						MetricRegex:    acc.MetricRegex,
						IsRegex:        acc.IsRegex,
						Origin:         metrics.OriginPromAlert,
						SourceID:       sourceID,
						SourceName:     fmt.Sprintf("%s (%s)", rule.Alert, grp.Name),
						SourceFolder:   displayFile,
						Query:          rule.Expr,
						ExplicitLabels: mergedLabels,
						PreservesAll:   acc.PreservesAll,
						WithoutLabels:  acc.WithoutLabels,
					})
				}
			} else if rule.Record != "" {
				res.TotalRuleCount++
				res.RecordingRuleCount++
				res.RecordingRules = append(res.RecordingRules, RecordingRule{
					Name:            rule.Record,
					Expr:            rule.Expr,
					File:            displayFile,
					Group:           grp.Name,
					Inputs:          extracted.Accesses,
					TemplateLabels:  tplLabels,
					HasOrVectorZero: orVectorZeroRe.MatchString(rule.Expr),
				})
			}
		}
	}
	return nil
}

func mergeUnique(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	for _, s := range b {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
