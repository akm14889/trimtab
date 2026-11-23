package k8sconsumers

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

// RawManifest represents a single raw YAML/JSON Kubernetes manifest buffer for consumer scanning.
type RawManifest struct {
	Source string
	Data   []byte
}

// ManifestSource loads Kubernetes manifests from files, directories, globs, or live cluster objects (P2.6).
type ManifestSource interface {
	Load() ([]RawManifest, error)
}

// GlobSource loads manifests from local file globs and directories recursively.
type GlobSource struct {
	Patterns []string
}

// StaticSource supplies in-memory manifests for testing or operator-backed callers.
type StaticSource struct {
	Manifests []RawManifest
}

func (s StaticSource) Load() ([]RawManifest, error) {
	return s.Manifests, nil
}

// ScanResult holds extracted live usage refs and per-consumer object counts for baseline tracking.
type ScanResult struct {
	Refs                 []metrics.UsageRef
	KEDATriggerCount     int
	PromAdapterRuleCount int
	RolloutTemplateCount int
	FlaggerTemplateCount int
	FilesScanned         int
	Warnings             []string
}

// Load resolves all directories and file globs into deterministic sorted RawManifest buffers.
func (g GlobSource) Load() ([]RawManifest, error) {
	files, err := expandManifestFiles(g.Patterns)
	if err != nil {
		return nil, err
	}
	out := make([]RawManifest, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read k8s consumer manifest %s: %w", f, err)
		}
		out = append(out, RawManifest{
			Source: filepath.ToSlash(f),
			Data:   data,
		})
	}
	return out, nil
}

func expandManifestFiles(patterns []string) ([]string, error) {
	seen := make(map[string]struct{})
	var files []string
	addFile := func(p string) {
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
				if isManifestFileName(d.Name()) {
					addFile(path)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("walk k8s consumers dir %s: %w", pat, err)
			}
			continue
		}

		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, fmt.Errorf("invalid k8s consumers glob %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("k8s consumers path matched no files: %q", pat)
		}
		for _, m := range matches {
			minfo, statErr := os.Stat(m)
			if statErr == nil && minfo.IsDir() {
				continue
			}
			addFile(m)
		}
	}
	sort.Strings(files)
	return files, nil
}

func isManifestFileName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".json")
}

// Scan reads manifests from cfg.Globs and extracts consumer PromQL usage.
func Scan(cfg *config.K8sConsumersConfig) (*ScanResult, error) {
	if cfg == nil || len(cfg.Globs) == 0 {
		return &ScanResult{}, nil
	}
	return ScanSource(GlobSource{Patterns: cfg.Globs}, cfg)
}

// ScanSource extracts KEDA, prometheus-adapter, Argo Rollouts, and Flagger usage from any ManifestSource.
func ScanSource(src ManifestSource, cfg *config.K8sConsumersConfig) (*ScanResult, error) {
	res := &ScanResult{}
	if src == nil {
		return res, nil
	}
	if cfg == nil {
		cfg = &config.K8sConsumersConfig{}
	}
	manifests, err := src.Load()
	if err != nil {
		return nil, err
	}
	res.FilesScanned = len(manifests)

	for _, m := range manifests {
		dec := yaml.NewDecoder(bytes.NewReader(m.Data))
		docIdx := 0
		for {
			var doc yaml.Node
			err := dec.Decode(&doc)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("parse YAML %s (doc %d): %w", m.Source, docIdx, err)
			}
			docIdx++
			if doc.Kind == 0 || len(doc.Content) == 0 {
				continue
			}
			scanYAMLNode(&doc, m.Source, fmt.Sprintf("doc%d", docIdx), cfg, res)
		}
	}
	return res, nil
}

type k8sObjectMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

func scanYAMLNode(node *yaml.Node, source, docPath string, cfg *config.K8sConsumersConfig, res *ScanResult) {
	var head k8sObjectMeta
	if err := node.Decode(&head); err != nil {
		return
	}

	if head.Kind == "List" || strings.HasSuffix(head.Kind, "List") {
		var listObj struct {
			Items []yaml.Node `yaml:"items"`
		}
		if err := node.Decode(&listObj); err == nil && len(listObj.Items) > 0 {
			for i := range listObj.Items {
				scanYAMLNode(&listObj.Items[i], source, fmt.Sprintf("%s.item%d", docPath, i), cfg, res)
			}
			return
		}
	}

	switch {
	case strings.HasPrefix(head.APIVersion, "keda.sh/") && (head.Kind == "ScaledObject" || head.Kind == "ScaledJob"):
		if cfg.KEDAEnabled() {
			scanKEDA(node, head, source, docPath, res)
		}
	case strings.HasPrefix(head.APIVersion, "argoproj.io/") && (head.Kind == "AnalysisTemplate" || head.Kind == "ClusterAnalysisTemplate"):
		if cfg.ArgoRolloutsEnabled() {
			scanArgoRollouts(node, head, source, docPath, res)
		}
	case strings.HasPrefix(head.APIVersion, "flagger.app/") && (head.Kind == "MetricTemplate" || head.Kind == "Canary"):
		if cfg.FlaggerEnabled() {
			scanFlagger(node, head, source, docPath, res)
		}
	case head.Kind == "ConfigMap":
		if cfg.PromAdapterEnabled() {
			scanPromAdapterConfigMap(node, head, source, docPath, res)
		}
	case head.Kind == "" && head.APIVersion == "":
		if cfg.PromAdapterEnabled() {
			scanPromAdapterStandalone(node, source, docPath, res)
		}
	}
}

var (
	tplBracketDurationRe = regexp.MustCompile(`\[\s*\{\{[^}]+\}\}\s*\]`)
	tplOffsetRe          = regexp.MustCompile(`(?i)\boffset\s+\{\{[^}]+\}\}`)
	tplAnyRe             = regexp.MustCompile(`\{\{[^}]+\}\}`)
	adapterBracketRe     = regexp.MustCompile(`\[\s*<<[^>]+>>\s*\]`)
	adapterAnyRe         = regexp.MustCompile(`<<[^>]+>>`)
	validLabelNameRe     = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

func sanitizeGoTemplatePromQL(expr string) string {
	s := tplBracketDurationRe.ReplaceAllString(expr, "[5m]")
	s = tplOffsetRe.ReplaceAllString(s, "offset 5m")
	s = tplAnyRe.ReplaceAllString(s, "1")
	return s
}

func scanKEDA(node *yaml.Node, head k8sObjectMeta, source, docPath string, res *ScanResult) {
	var obj struct {
		Spec struct {
			Triggers []struct {
				Type     string            `yaml:"type"`
				Name     string            `yaml:"name"`
				Metadata map[string]string `yaml:"metadata"`
			} `yaml:"triggers"`
		} `yaml:"spec"`
	}
	if err := node.Decode(&obj); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("keda %s (%s): decode failed: %v", source, docPath, err))
		return
	}

	nsName := namespacedName(head.Metadata.Namespace, head.Metadata.Name, docPath)
	for i, trig := range obj.Spec.Triggers {
		if !strings.EqualFold(strings.TrimSpace(trig.Type), "prometheus") {
			continue
		}
		rawQuery := strings.TrimSpace(trig.Metadata["query"])
		if rawQuery == "" {
			continue
		}
		res.KEDATriggerCount++
		extracted, err := promql.Extract(sanitizeGoTemplatePromQL(rawQuery))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("keda %s %s trigger[%d]: parse PromQL: %v", source, nsName, i, err))
			continue
		}
		trigLabel := trig.Name
		if trigLabel == "" {
			trigLabel = fmt.Sprintf("#%d", i)
		}
		sourceID := fmt.Sprintf("keda:%s:%s:%s:%d", head.Kind, source, nsName, i)
		sourceName := fmt.Sprintf("%s %s (%s)", head.Kind, nsName, trigLabel)
		appendAccessesAsRefs(res, extracted.Accesses, metrics.OriginKEDA, sourceID, sourceName, source, rawQuery)
	}
}

func scanArgoRollouts(node *yaml.Node, head k8sObjectMeta, source, docPath string, res *ScanResult) {
	var obj struct {
		Spec struct {
			Metrics []struct {
				Name     string `yaml:"name"`
				Provider struct {
					Prometheus struct {
						Query string `yaml:"query"`
					} `yaml:"prometheus"`
				} `yaml:"provider"`
			} `yaml:"metrics"`
		} `yaml:"spec"`
	}
	if err := node.Decode(&obj); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("argo-rollouts %s (%s): decode failed: %v", source, docPath, err))
		return
	}

	nsName := namespacedName(head.Metadata.Namespace, head.Metadata.Name, docPath)
	counted := false
	for i, m := range obj.Spec.Metrics {
		rawQuery := strings.TrimSpace(m.Provider.Prometheus.Query)
		if rawQuery == "" {
			continue
		}
		if !counted {
			res.RolloutTemplateCount++
			counted = true
		}
		extracted, err := promql.Extract(sanitizeGoTemplatePromQL(rawQuery))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("argo-rollouts %s %s metric %q: parse PromQL: %v", source, nsName, m.Name, err))
			continue
		}
		metricLabel := m.Name
		if metricLabel == "" {
			metricLabel = fmt.Sprintf("#%d", i)
		}
		sourceID := fmt.Sprintf("argo-rollouts:%s:%s:%s:%s", head.Kind, source, nsName, metricLabel)
		sourceName := fmt.Sprintf("%s %s (%s)", head.Kind, nsName, metricLabel)
		appendAccessesAsRefs(res, extracted.Accesses, metrics.OriginArgoRollouts, sourceID, sourceName, source, rawQuery)
	}
}

func scanFlagger(node *yaml.Node, head k8sObjectMeta, source, docPath string, res *ScanResult) {
	nsName := namespacedName(head.Metadata.Namespace, head.Metadata.Name, docPath)
	if head.Kind == "MetricTemplate" {
		var obj struct {
			Spec struct {
				Provider struct {
					Type string `yaml:"type"`
				} `yaml:"provider"`
				Query string `yaml:"query"`
			} `yaml:"spec"`
		}
		if err := node.Decode(&obj); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("flagger %s (%s): decode failed: %v", source, docPath, err))
			return
		}
		if !isPrometheusCompatibleProvider(obj.Spec.Provider.Type) {
			return
		}
		rawQuery := strings.TrimSpace(obj.Spec.Query)
		if rawQuery == "" {
			return
		}
		res.FlaggerTemplateCount++
		extracted, err := promql.Extract(sanitizeGoTemplatePromQL(rawQuery))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("flagger %s %s: parse PromQL: %v", source, nsName, err))
			return
		}
		sourceID := fmt.Sprintf("flagger:MetricTemplate:%s:%s", source, nsName)
		sourceName := fmt.Sprintf("MetricTemplate %s", nsName)
		appendAccessesAsRefs(res, extracted.Accesses, metrics.OriginFlagger, sourceID, sourceName, source, rawQuery)
		return
	}

	var canary struct {
		Spec struct {
			Analysis struct {
				Metrics []struct {
					Name  string `yaml:"name"`
					Query string `yaml:"query"`
				} `yaml:"metrics"`
			} `yaml:"analysis"`
		} `yaml:"spec"`
	}
	if err := node.Decode(&canary); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("flagger Canary %s (%s): decode failed: %v", source, docPath, err))
		return
	}
	counted := false
	for i, m := range canary.Spec.Analysis.Metrics {
		rawQuery := strings.TrimSpace(m.Query)
		if rawQuery == "" {
			continue
		}
		if !counted {
			res.FlaggerTemplateCount++
			counted = true
		}
		extracted, err := promql.Extract(sanitizeGoTemplatePromQL(rawQuery))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("flagger Canary %s %s metric %q: parse PromQL: %v", source, nsName, m.Name, err))
			continue
		}
		metricLabel := m.Name
		if metricLabel == "" {
			metricLabel = fmt.Sprintf("#%d", i)
		}
		sourceID := fmt.Sprintf("flagger:Canary:%s:%s:%s", source, nsName, metricLabel)
		sourceName := fmt.Sprintf("Canary %s (%s)", nsName, metricLabel)
		appendAccessesAsRefs(res, extracted.Accesses, metrics.OriginFlagger, sourceID, sourceName, source, rawQuery)
	}
}

func isPrometheusCompatibleProvider(pType string) bool {
	switch strings.ToLower(strings.TrimSpace(pType)) {
	case "", "prometheus", "thanos", "mimir", "cortex", "victoriametrics", "amp", "gcm":
		return true
	default:
		return false
	}
}

type promAdapterConfig struct {
	Rules         []promAdapterSeriesRule          `yaml:"rules"`
	ExternalRules []promAdapterSeriesRule          `yaml:"externalRules"`
	ResourceRules map[string]promAdapterResourceRule `yaml:"resourceRules"`
}

type promAdapterResources struct {
	Overrides map[string]any `yaml:"overrides"`
}

type promAdapterSeriesRule struct {
	SeriesQuery  string               `yaml:"seriesQuery"`
	Resources    promAdapterResources `yaml:"resources"`
	MetricsQuery string               `yaml:"metricsQuery"`
}

type promAdapterResourceRule struct {
	ContainerQuery string               `yaml:"containerQuery"`
	NodeQuery      string               `yaml:"nodeQuery"`
	Resources      promAdapterResources `yaml:"resources"`
	ContainerLabel string               `yaml:"containerLabel"`
}

func (c promAdapterConfig) isEmpty() bool {
	return len(c.Rules) == 0 && len(c.ExternalRules) == 0 && len(c.ResourceRules) == 0
}

func scanPromAdapterConfigMap(node *yaml.Node, head k8sObjectMeta, source, docPath string, res *ScanResult) {
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := node.Decode(&cm); err != nil || len(cm.Data) == 0 {
		return
	}
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	nsName := namespacedName(head.Metadata.Namespace, head.Metadata.Name, docPath)
	for _, k := range keys {
		raw := strings.TrimSpace(cm.Data[k])
		if raw == "" {
			continue
		}
		var cfg promAdapterConfig
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil || cfg.isEmpty() {
			continue
		}
		extractPromAdapterConfig(cfg, source, nsName+"/"+k, res)
	}
}

func scanPromAdapterStandalone(node *yaml.Node, source, docPath string, res *ScanResult) {
	var cfg promAdapterConfig
	if err := node.Decode(&cfg); err != nil || cfg.isEmpty() {
		return
	}
	extractPromAdapterConfig(cfg, source, docPath, res)
}

func extractPromAdapterConfig(cfg promAdapterConfig, source, scope string, res *ScanResult) {
	processSeriesRules := func(kindLabel string, rules []promAdapterSeriesRule) {
		for i, r := range rules {
			sq := strings.TrimSpace(r.SeriesQuery)
			mq := strings.TrimSpace(r.MetricsQuery)
			if sq == "" && mq == "" {
				continue
			}
			res.PromAdapterRuleCount++
			overrideLabels := sortedOverrideLabels(r.Resources.Overrides)
			ruleScope := fmt.Sprintf("%s:%s[%d]", scope, kindLabel, i)
			sourceID := fmt.Sprintf("prometheus-adapter:%s:%s", source, ruleScope)
			sourceName := fmt.Sprintf("prometheus-adapter %s", ruleScope)
			extractPromAdapterSeriesRule(sq, mq, overrideLabels, sourceID, sourceName, source, res)
		}
	}

	processSeriesRules("rules", cfg.Rules)
	processSeriesRules("externalRules", cfg.ExternalRules)

	resKeys := make([]string, 0, len(cfg.ResourceRules))
	for k := range cfg.ResourceRules {
		resKeys = append(resKeys, k)
	}
	sort.Strings(resKeys)
	for _, rk := range resKeys {
		rr := cfg.ResourceRules[rk]
		overrideLabels := sortedOverrideLabels(rr.Resources.Overrides)
		if cLbl := strings.TrimSpace(rr.ContainerLabel); validLabelNameRe.MatchString(cLbl) {
			overrideLabels = mergeLabelsUnique(overrideLabels, []string{cLbl})
		}
		for _, qPair := range []struct {
			name string
			q    string
		}{
			{"containerQuery", rr.ContainerQuery},
			{"nodeQuery", rr.NodeQuery},
		} {
			q := strings.TrimSpace(qPair.q)
			if q == "" {
				continue
			}
			res.PromAdapterRuleCount++
			sanitized, _ := sanitizeAdapterPromQL(q, overrideLabels)
			extracted, err := promql.Extract(sanitized)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("prometheus-adapter %s %s.%s: parse PromQL: %v", source, rk, qPair.name, err))
				continue
			}
			ruleScope := fmt.Sprintf("%s:resourceRules.%s.%s", scope, rk, qPair.name)
			sourceID := fmt.Sprintf("prometheus-adapter:%s:%s", source, ruleScope)
			sourceName := fmt.Sprintf("prometheus-adapter %s", ruleScope)
			for _, acc := range extracted.Accesses {
				cleanAcc := stripSyntheticLabels(acc)
				cleanAcc.ExplicitLabels = mergeLabelsUnique(cleanAcc.ExplicitLabels, overrideLabels)
				res.Refs = append(res.Refs, buildRef(cleanAcc, metrics.OriginPromAdapter, sourceID, sourceName, source, q))
			}
		}
	}
}

func extractPromAdapterSeriesRule(sq, mq string, overrideLabels []string, sourceID, sourceName, source string, res *ScanResult) {
	var sqAccesses []promql.MetricAccess
	if sq != "" {
		extractedSQ, err := promql.Extract(sq)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("prometheus-adapter %s seriesQuery: parse PromQL: %v", source, err))
			return
		}
		sqAccesses = extractedSQ.Accesses
	}

	if mq == "" {
		for _, acc := range sqAccesses {
			merged := acc
			merged.ExplicitLabels = mergeLabelsUnique(acc.ExplicitLabels, overrideLabels)
			if len(overrideLabels) == 0 && len(merged.ExplicitLabels) == 0 {
				merged.PreservesAll = true
			}
			res.Refs = append(res.Refs, buildRef(merged, metrics.OriginPromAdapter, sourceID, sourceName, source, sq))
		}
		return
	}

	sanitizedMQ, dynamicGroupBy := sanitizeAdapterPromQL(mq, overrideLabels)
	extractedMQ, err := promql.Extract(sanitizedMQ)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("prometheus-adapter %s metricsQuery: parse PromQL: %v", source, err))
		return
	}

	for _, acc := range extractedMQ.Accesses {
		cleanAcc := stripSyntheticLabels(acc)
		if cleanAcc.MetricName != "__trimtab_adapter_series__" {
			cleanAcc.ExplicitLabels = mergeLabelsUnique(cleanAcc.ExplicitLabels, overrideLabels)
			res.Refs = append(res.Refs, buildRef(cleanAcc, metrics.OriginPromAdapter, sourceID, sourceName, source, mq))
			continue
		}
		if len(sqAccesses) == 0 {
			continue
		}
		for _, sqAcc := range sqAccesses {
			merged := promql.MetricAccess{
				MetricName:     sqAcc.MetricName,
				MetricRegex:    sqAcc.MetricRegex,
				IsRegex:        sqAcc.IsRegex,
				ExplicitLabels: mergeLabelsUnique(mergeLabelsUnique(sqAcc.ExplicitLabels, overrideLabels), cleanAcc.ExplicitLabels),
				PreservesAll:   cleanAcc.PreservesAll || dynamicGroupBy,
				WithoutLabels:  cleanAcc.WithoutLabels,
			}
			res.Refs = append(res.Refs, buildRef(merged, metrics.OriginPromAdapter, sourceID, sourceName, source, sq+" | "+mq))
		}
	}
}

func sanitizeAdapterPromQL(expr string, overrideLabels []string) (sanitized string, dynamicGroupBy bool) {
	lmPlaceholder := `__trimtab_lm__="1"`
	gbPlaceholder := `__trimtab_gb__`
	if len(overrideLabels) > 0 {
		pairs := make([]string, 0, len(overrideLabels))
		for _, lbl := range overrideLabels {
			pairs = append(pairs, lbl+`="1"`)
		}
		lmPlaceholder = strings.Join(pairs, ",")
		gbPlaceholder = strings.Join(overrideLabels, ",")
	} else if strings.Contains(expr, "<<.GroupBy>>") {
		dynamicGroupBy = true
	}

	r := strings.NewReplacer(
		"<<.Series>>", "__trimtab_adapter_series__",
		"<<.LabelMatchers>>", lmPlaceholder,
		"<<.GroupBy>>", gbPlaceholder,
	)
	s := r.Replace(expr)
	s = adapterBracketRe.ReplaceAllString(s, "[5m]")
	s = adapterAnyRe.ReplaceAllString(s, "1")
	s = sanitizeGoTemplatePromQL(s)
	return s, dynamicGroupBy
}

func stripSyntheticLabels(acc promql.MetricAccess) promql.MetricAccess {
	out := acc
	var kept []string
	for _, lbl := range acc.ExplicitLabels {
		if lbl == "__trimtab_lm__" || lbl == "__trimtab_gb__" {
			continue
		}
		kept = append(kept, lbl)
	}
	out.ExplicitLabels = kept
	return out
}

func sortedOverrideLabels(overrides map[string]any) []string {
	if len(overrides) == 0 {
		return nil
	}
	out := make([]string, 0, len(overrides))
	for k := range overrides {
		k = strings.TrimSpace(k)
		if validLabelNameRe.MatchString(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func appendAccessesAsRefs(
	res *ScanResult,
	accesses []promql.MetricAccess,
	origin metrics.UsageOrigin,
	sourceID, sourceName, sourceFolder, query string,
) {
	for _, acc := range accesses {
		res.Refs = append(res.Refs, buildRef(acc, origin, sourceID, sourceName, sourceFolder, query))
	}
}

func buildRef(
	acc promql.MetricAccess,
	origin metrics.UsageOrigin,
	sourceID, sourceName, sourceFolder, query string,
) metrics.UsageRef {
	return metrics.UsageRef{
		MetricName:     acc.MetricName,
		MetricRegex:    acc.MetricRegex,
		IsRegex:        acc.IsRegex,
		Origin:         origin,
		SourceID:       sourceID,
		SourceName:     sourceName,
		SourceFolder:   sourceFolder,
		Query:          query,
		ExplicitLabels: append([]string(nil), acc.ExplicitLabels...),
		PreservesAll:   acc.PreservesAll,
		WithoutLabels:  append([]string(nil), acc.WithoutLabels...),
	}
}

func namespacedName(ns, name, fallback string) string {
	switch {
	case ns != "" && name != "":
		return ns + "/" + name
	case name != "":
		return name
	default:
		return fallback
	}
}

func mergeLabelsUnique(a, b []string) []string {
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
