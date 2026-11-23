package ruler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promql"
	"trimtab.io/trimtab/pkg/usage/promrules"
)

// ScanResult holds all alerting-rule live refs, recording rules, and object counts fetched from Ruler HTTP APIs.
type ScanResult struct {
	AlertRefs          []metrics.UsageRef
	RecordingRules     []promrules.RecordingRule
	TotalRuleCount     int
	AlertRuleCount     int
	RecordingRuleCount int
	RulersScanned      int
	Warnings           []string
}

// ScanAll polls every configured Ruler endpoint sequentially and merges all refs and recording rules.
func ScanAll(ctx context.Context, cfgs []config.RulerConfig) (*ScanResult, error) {
	out := &ScanResult{}
	for _, cfg := range cfgs {
		res, err := Scan(ctx, cfg)
		if err != nil {
			return nil, err
		}
		out.AlertRefs = append(out.AlertRefs, res.AlertRefs...)
		out.RecordingRules = append(out.RecordingRules, res.RecordingRules...)
		out.TotalRuleCount += res.TotalRuleCount
		out.AlertRuleCount += res.AlertRuleCount
		out.RecordingRuleCount += res.RecordingRuleCount
		out.RulersScanned += res.RulersScanned
		out.Warnings = append(out.Warnings, res.Warnings...)
	}
	return out, nil
}

// Scan polls a single Mimir/Cortex/Prometheus/Thanos Ruler HTTP API (read-only).
func Scan(ctx context.Context, cfg config.RulerConfig) (*ScanResult, error) {
	rType := strings.ToLower(strings.TrimSpace(cfg.Type))
	if rType == "" {
		rType = "prometheus"
	}
	if rType == "cortex" {
		rType = "mimir"
	}

	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		return nil, err
	}
	client, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.URL,
		Timeout:     timeout,
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		return nil, fmt.Errorf("ruler client (%s): %w", rType, err)
	}

	res := &ScanResult{
		Warnings: client.Warnings(),
	}

	switch rType {
	case "mimir":
		if err := scanMimirRuler(ctx, client, cfg.TenantID, res); err != nil {
			return nil, err
		}
	case "prometheus", "thanos":
		if err := scanPromRuler(ctx, client, rType, res); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported ruler type %q", cfg.Type)
	}
	res.RulersScanned = 1
	return res, nil
}

type mimirRuleGroup struct {
	Name  string          `yaml:"name"`
	Rules []mimirRuleItem `yaml:"rules"`
}

type mimirRuleItem struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func scanMimirRuler(ctx context.Context, client *httpclient.Client, tenantID string, res *ScanResult) error {
	body, err := client.Get(ctx, "/prometheus/config/v1/rules")
	if err != nil {
		return fmt.Errorf("mimir ruler %w", err)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	var byNamespace map[string][]mimirRuleGroup
	if err := yaml.Unmarshal(trimmed, &byNamespace); err != nil {
		return fmt.Errorf("decode mimir ruler YAML: %w", err)
	}

	namespaces := make([]string, 0, len(byNamespace))
	for ns := range byNamespace {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	for _, ns := range namespaces {
		fileLabel := "mimir-ruler/" + ns
		if tenantID != "" {
			fileLabel = fmt.Sprintf("mimir-ruler/%s/%s", tenantID, ns)
		}
		for _, grp := range byNamespace[ns] {
			for _, rule := range grp.Rules {
				recordParsedRule(parsedRuleInput{
					alert:       rule.Alert,
					record:      rule.Record,
					expr:        rule.Expr,
					labels:      rule.Labels,
					annotations: rule.Annotations,
					fileLabel:   fileLabel,
					groupName:   grp.Name,
					sourceID:    fmt.Sprintf("ruler-alert:mimir:%s:%s:%s", ns, grp.Name, rule.Alert),
					sourceName:  fmt.Sprintf("%s (%s/%s)", rule.Alert, ns, grp.Name),
				}, res)
			}
		}
	}
	return nil
}

type promRulesEnvelope struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Groups []struct {
			Name  string `json:"name"`
			File  string `json:"file"`
			Rules []struct {
				Name        string            `json:"name"`
				Query       string            `json:"query"`
				Type        string            `json:"type"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"data"`
}

func scanPromRuler(ctx context.Context, client *httpclient.Client, rType string, res *ScanResult) error {
	body, err := client.Get(ctx, "/api/v1/rules")
	if err != nil {
		return fmt.Errorf("%s ruler %w", rType, err)
	}
	var env promRulesEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode %s /api/v1/rules JSON: %w", rType, err)
	}
	if !strings.EqualFold(strings.TrimSpace(env.Status), "success") {
		return fmt.Errorf("%s /api/v1/rules status=%q error=%s", rType, env.Status, client.Redact(env.Error))
	}

	for _, grp := range env.Data.Groups {
		fileLabel := grp.File
		if fileLabel == "" {
			fileLabel = rType + "-ruler"
		}
		for _, rule := range grp.Rules {
			var alertName, recordName string
			switch strings.ToLower(strings.TrimSpace(rule.Type)) {
			case "alerting":
				alertName = rule.Name
			case "recording":
				recordName = rule.Name
			default:
				continue
			}
			recordParsedRule(parsedRuleInput{
				alert:       alertName,
				record:      recordName,
				expr:        rule.Query,
				labels:      rule.Labels,
				annotations: rule.Annotations,
				fileLabel:   fileLabel,
				groupName:   grp.Name,
				sourceID:    fmt.Sprintf("ruler-alert:%s:%s:%s:%s", rType, fileLabel, grp.Name, rule.Name),
				sourceName:  fmt.Sprintf("%s (%s)", rule.Name, grp.Name),
			}, res)
		}
	}
	return nil
}

type parsedRuleInput struct {
	alert       string
	record      string
	expr        string
	labels      map[string]string
	annotations map[string]string
	fileLabel   string
	groupName   string
	sourceID    string
	sourceName  string
}

func recordParsedRule(in parsedRuleInput, res *ScanResult) {
	if strings.TrimSpace(in.expr) == "" {
		return
	}
	if in.alert == "" && in.record == "" {
		return
	}

	var tplTexts []string
	for _, v := range in.annotations {
		tplTexts = append(tplTexts, v)
	}
	for _, v := range in.labels {
		tplTexts = append(tplTexts, v)
	}
	tplLabels := promrules.ExtractTemplateLabels(tplTexts...)

	extracted, err := promql.Extract(in.expr)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("ruler %s/%s: failed to parse expr: %v", in.fileLabel, in.groupName, err))
		return
	}

	if in.alert != "" {
		res.TotalRuleCount++
		res.AlertRuleCount++
		for _, acc := range extracted.Accesses {
			mergedLabels := mergeUnique(acc.ExplicitLabels, tplLabels)
			res.AlertRefs = append(res.AlertRefs, metrics.UsageRef{
				MetricName:     acc.MetricName,
				MetricRegex:    acc.MetricRegex,
				IsRegex:        acc.IsRegex,
				Origin:         metrics.OriginRulerAlert,
				SourceID:       in.sourceID,
				SourceName:     in.sourceName,
				SourceFolder:   in.fileLabel,
				Query:          in.expr,
				ExplicitLabels: mergedLabels,
				PreservesAll:   acc.PreservesAll,
				WithoutLabels:  acc.WithoutLabels,
			})
		}
		return
	}

	res.TotalRuleCount++
	res.RecordingRuleCount++
	res.RecordingRules = append(res.RecordingRules, promrules.RecordingRule{
		Name:            in.record,
		Expr:            in.expr,
		File:            in.fileLabel,
		Group:           in.groupName,
		Inputs:          extracted.Accesses,
		TemplateLabels:  tplLabels,
		HasOrVectorZero: promrules.HasOrVectorZero(in.expr),
	})
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
