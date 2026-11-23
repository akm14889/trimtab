package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promql"
	"trimtab.io/trimtab/pkg/usage/promrules"
)

// DashboardMeta captures dashboard identity for wildcard reporting and excludeSources.
type DashboardMeta struct {
	UID    string
	Title  string
	Folder string
	Tags   []string
}

// ScanResult holds all extracted dashboard & managed-alert usage refs plus exact object counts.
type ScanResult struct {
	Refs           []metrics.UsageRef
	DashboardCount int
	AlertRuleCount int
	Dashboards     []DashboardMeta
	Warnings       []string
}

var (
	// Grafana legendFormat: {{pod}}, {{ instance }}, {{.pod}}
	legendLabelRe = regexp.MustCompile(`\{\{\s*\.?([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
	// Grafana data/panel links: ${__field.labels.pod}, ${__series.labels.env}, ${__data.fields.instance}
	linkLabelRe = regexp.MustCompile(`\$\{(?:__field\.labels|__series\.labels|__data\.fields)\.([a-zA-Z_][a-zA-Z0-9_]*)(?::[^}]+)?\}`)
	// Grafana template variable queries:
	//   label_values(metric_name, label_name)
	//   label_values(metric_name{env="prod"}, label_name)
	//   label_values(label_name)
	labelValuesRe  = regexp.MustCompile(`^\s*label_values\s*\(\s*(.+?)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)\s*$`)
	queryResultRe  = regexp.MustCompile(`^\s*query_result\s*\(\s*(.+)\s*\)\s*$`)
	metricsQueryRe = regexp.MustCompile(`^\s*metrics\s*\(\s*(.+?)\s*\)\s*$`)

	// Grafana duration template variables inside PromQL brackets or offsets
	grafanaDurationVarRe = regexp.MustCompile(`\$(?:\{__(?:rate_interval|interval|range|range_s|range_ms)(?::[^}]+)?\}|__(?:rate_interval|interval|range|range_s|range_ms)\b)`)
	// Grafana generic variable in quoted/unquoted PromQL contexts
	grafanaBracedVarRe = regexp.MustCompile(`\$\{[a-zA-Z_][a-zA-Z0-9_]*(?::[^}]+)?\}`)
	grafanaSimpleVarRe = regexp.MustCompile(`\$[a-zA-Z_][a-zA-Z0-9_]*\b`)
)

// SanitizePromQL replaces Grafana template variables with valid PromQL placeholders
// so the official PromQL parser can parse dashboard expressions cleanly.
func SanitizePromQL(expr string) string {
	s := grafanaDurationVarRe.ReplaceAllString(expr, "5m")
	s = grafanaBracedVarRe.ReplaceAllString(s, "__trimtab_var__")
	s = grafanaSimpleVarRe.ReplaceAllString(s, "__trimtab_var__")
	return s
}

// Scan loads Grafana dashboards and Grafana-managed alert rules from HTTP API and/or local files.
func Scan(ctx context.Context, cfg *config.GrafanaConfig) (*ScanResult, error) {
	res := &ScanResult{}
	if cfg == nil {
		return res, nil
	}

	if cfg.DashboardsDir != "" {
		if err := scanDashboardsDir(cfg.DashboardsDir, res); err != nil {
			return nil, err
		}
	}
	if cfg.AlertRulesPath != "" && cfg.ManagedAlertsEnabled() {
		if err := scanAlertRulesPath(cfg.AlertRulesPath, res); err != nil {
			return nil, err
		}
	}
	if cfg.URL != "" {
		if err := scanHTTP(ctx, cfg, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func scanDashboardsDir(dir string, res *ScanResult) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read grafana dashboardsDir %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read dashboard %s: %w", f, err)
		}
		if err := parseDashboardJSON(data, "", res); err != nil {
			return fmt.Errorf("parse dashboard %s: %w", f, err)
		}
	}
	return nil
}

func scanAlertRulesPath(path string, res *ScanResult) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat grafana alertRulesPath %s: %w", path, err)
	}
	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("read grafana alertRulesPath dir %s: %w", path, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
		sort.Strings(files)
	} else {
		files = []string{path}
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read grafana alert rules %s: %w", f, err)
		}
		if err := parseManagedAlertsJSON(data, res); err != nil {
			return fmt.Errorf("parse grafana alert rules %s: %w", f, err)
		}
	}
	return nil
}

func scanHTTP(ctx context.Context, cfg *config.GrafanaConfig, res *ScanResult) error {
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	baseURL := strings.TrimRight(cfg.URL, "/")

	doGet := func(endpoint string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+endpoint, nil)
		if err != nil {
			return nil, err
		}
		if cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		for k, v := range cfg.Headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("grafana GET %s: %w", endpoint, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("grafana read %s: %w", endpoint, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("grafana GET %s returned HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return body, nil
	}

	// 1. Search all dashboards
	searchBody, err := doGet("/api/search?type=dash-db&limit=5000")
	if err != nil {
		return err
	}
	var searchItems []struct {
		UID         string   `json:"uid"`
		Title       string   `json:"title"`
		FolderTitle string   `json:"folderTitle"`
		Tags        []string `json:"tags"`
	}
	if err := json.Unmarshal(searchBody, &searchItems); err != nil {
		return fmt.Errorf("decode grafana /api/search: %w", err)
	}
	for _, item := range searchItems {
		if item.UID == "" {
			continue
		}
		dashBody, err := doGet("/api/dashboards/uid/" + item.UID)
		if err != nil {
			return err
		}
		if err := parseDashboardJSON(dashBody, item.FolderTitle, res); err != nil {
			return fmt.Errorf("parse dashboard uid=%s: %w", item.UID, err)
		}
	}

	// 2. Grafana-managed alert rules
	if cfg.ManagedAlertsEnabled() {
		alertBody, err := doGet("/api/v1/provisioning/alert-rules")
		if err != nil {
			return err
		}
		if err := parseManagedAlertsJSON(alertBody, res); err != nil {
			return fmt.Errorf("parse grafana managed alerts: %w", err)
		}
	}

	return nil
}

type dashboardEnvelope struct {
	Meta struct {
		FolderTitle string `json:"folderTitle"`
	} `json:"meta"`
	Dashboard *dashboardDoc `json:"dashboard"`
	// Also support raw unwrapped dashboard JSON directly.
	UID        string   `json:"uid"`
	Title      string   `json:"title"`
	Folder     string   `json:"folderTitle"`
	Tags       []string `json:"tags"`
	Panels     []panel  `json:"panels"`
	Rows       []row    `json:"rows"`
	Templating struct {
		List []templateVar `json:"list"`
	} `json:"templating"`
}

type dashboardDoc struct {
	UID        string   `json:"uid"`
	Title      string   `json:"title"`
	Folder     string   `json:"folderTitle"`
	Tags       []string `json:"tags"`
	Panels     []panel  `json:"panels"`
	Rows       []row    `json:"rows"`
	Templating struct {
		List []templateVar `json:"list"`
	} `json:"templating"`
}

type row struct {
	Panels []panel `json:"panels"`
}

type panel struct {
	Title           string            `json:"title"`
	Targets         []target          `json:"targets"`
	Panels          []panel           `json:"panels"`
	Links           []linkObj         `json:"links"`
	FieldConfig     fieldConfig       `json:"fieldConfig"`
	Transformations []transformation  `json:"transformations"`
}

type target struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
}

type linkObj struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type fieldConfig struct {
	Defaults struct {
		Links []linkObj `json:"links"`
	} `json:"defaults"`
	Overrides []struct {
		Matcher struct {
			ID      string `json:"id"`
			Options any    `json:"options"`
		} `json:"matcher"`
		Properties []struct {
			ID    string `json:"id"`
			Value any    `json:"value"`
		} `json:"properties"`
	} `json:"overrides"`
}

type transformation struct {
	ID      string         `json:"id"`
	Options map[string]any `json:"options"`
}

type templateVar struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Query any    `json:"query"`
}

func parseDashboardJSON(data []byte, folderHint string, res *ScanResult) error {
	var env dashboardEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	var doc dashboardDoc
	folder := folderHint
	if env.Dashboard != nil {
		doc = *env.Dashboard
		if folder == "" {
			folder = env.Meta.FolderTitle
		}
	} else {
		doc = dashboardDoc{
			UID:        env.UID,
			Title:      env.Title,
			Folder:     env.Folder,
			Tags:       env.Tags,
			Panels:     env.Panels,
			Rows:       env.Rows,
			Templating: env.Templating,
		}
	}
	if folder == "" {
		folder = doc.Folder
	}
	if doc.UID == "" {
		doc.UID = doc.Title
	}

	res.DashboardCount++
	res.Dashboards = append(res.Dashboards, DashboardMeta{
		UID:    doc.UID,
		Title:  doc.Title,
		Folder: folder,
		Tags:   doc.Tags,
	})

	sourceID := "grafana-dashboard:" + doc.UID

	var allPanels []panel
	var collectPanels func([]panel)
	collectPanels = func(ps []panel) {
		for _, p := range ps {
			allPanels = append(allPanels, p)
			if len(p.Panels) > 0 {
				collectPanels(p.Panels)
			}
		}
	}
	collectPanels(doc.Panels)
	for _, r := range doc.Rows {
		collectPanels(r.Panels)
	}

	for _, p := range allPanels {
		panelExtraLabels := extractPanelExtraLabels(p)
		for _, tgt := range p.Targets {
			if strings.TrimSpace(tgt.Expr) == "" {
				continue
			}
			legendLabels := extractLegendLabels(tgt.LegendFormat)
			extraLabels := mergeStrings(panelExtraLabels, legendLabels)

			sanitized := SanitizePromQL(tgt.Expr)
			extracted, err := promql.Extract(sanitized)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("dashboard %q panel %q: failed to parse PromQL: %v", doc.Title, p.Title, err))
				continue
			}
			for _, acc := range extracted.Accesses {
				res.Refs = append(res.Refs, metrics.UsageRef{
					MetricName:     acc.MetricName,
					MetricRegex:    acc.MetricRegex,
					IsRegex:        acc.IsRegex,
					Origin:         metrics.OriginGrafanaDashboard,
					SourceID:       sourceID,
					SourceName:     doc.Title,
					SourceTags:     doc.Tags,
					SourceFolder:   folder,
					Query:          tgt.Expr,
					ExplicitLabels: mergeStrings(acc.ExplicitLabels, extraLabels),
					PreservesAll:   acc.PreservesAll,
					WithoutLabels:  acc.WithoutLabels,
				})
			}
		}
	}

	// Template variables
	for _, tv := range doc.Templating.List {
		extractTemplateVarRefs(tv, sourceID, doc.Title, folder, doc.Tags, res)
	}
	return nil
}

func extractLegendLabels(legend string) []string {
	if legend == "" {
		return nil
	}
	var out []string
	for _, m := range legendLabelRe.FindAllStringSubmatch(legend, -1) {
		if len(m) > 1 {
			out = append(out, m[1])
		}
	}
	return out
}

func extractPanelExtraLabels(p panel) []string {
	seen := make(map[string]struct{})
	addLinkText := func(s string) {
		for _, m := range linkLabelRe.FindAllStringSubmatch(s, -1) {
			if len(m) > 1 {
				seen[m[1]] = struct{}{}
			}
		}
	}
	for _, l := range p.Links {
		addLinkText(l.URL)
		addLinkText(l.Title)
	}
	for _, l := range p.FieldConfig.Defaults.Links {
		addLinkText(l.URL)
		addLinkText(l.Title)
	}
	for _, ov := range p.FieldConfig.Overrides {
		if ov.Matcher.ID == "byName" {
			if s, ok := ov.Matcher.Options.(string); ok && isValidLabelIdent(s) {
				seen[s] = struct{}{}
			}
		}
		for _, prop := range ov.Properties {
			if prop.ID == "links" {
				if raw, err := json.Marshal(prop.Value); err == nil {
					addLinkText(string(raw))
				}
			}
		}
	}
	for _, tr := range p.Transformations {
		extractTransformLabels(tr, seen)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func extractTransformLabels(tr transformation, seen map[string]struct{}) {
	if tr.Options == nil {
		return
	}
	switch tr.ID {
	case "organize":
		if rename, ok := tr.Options["renameByName"].(map[string]any); ok {
			for k := range rename {
				if isValidLabelIdent(k) {
					seen[k] = struct{}{}
				}
			}
		}
		if idx, ok := tr.Options["indexByName"].(map[string]any); ok {
			excluded, _ := tr.Options["excludeByName"].(map[string]any)
			for k := range idx {
				if ex, _ := excluded[k].(bool); !ex && isValidLabelIdent(k) {
					seen[k] = struct{}{}
				}
			}
		}
	case "groupBy":
		if fields, ok := tr.Options["fields"].(map[string]any); ok {
			for k := range fields {
				if isValidLabelIdent(k) {
					seen[k] = struct{}{}
				}
			}
		}
	case "filterFieldsByName":
		if inc, ok := tr.Options["include"].(map[string]any); ok {
			if names, ok := inc["names"].([]any); ok {
				for _, n := range names {
					if s, ok := n.(string); ok && isValidLabelIdent(s) {
						seen[s] = struct{}{}
					}
				}
			}
		}
	case "labelsToFields":
		if vl, ok := tr.Options["valueLabel"].(string); ok && isValidLabelIdent(vl) {
			seen[vl] = struct{}{}
		}
	}
}

var labelIdentRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func isValidLabelIdent(s string) bool {
	if s == "" || s == "Time" || s == "Value" || s == "__name__" {
		return false
	}
	return labelIdentRe.MatchString(s)
}

func extractTemplateVarRefs(tv templateVar, sourceID, dashTitle, folder string, tags []string, res *ScanResult) {
	queryStr := ""
	switch q := tv.Query.(type) {
	case string:
		queryStr = strings.TrimSpace(q)
	case map[string]any:
		if s, ok := q["query"].(string); ok {
			queryStr = strings.TrimSpace(s)
		}
	}
	if queryStr == "" {
		return
	}

	if m := labelValuesRe.FindStringSubmatch(queryStr); len(m) == 3 {
		selectorExpr := SanitizePromQL(strings.TrimSpace(m[1]))
		targetLabel := m[2]
		extracted, err := promql.Extract(selectorExpr)
		if err == nil {
			for _, acc := range extracted.Accesses {
				res.Refs = append(res.Refs, metrics.UsageRef{
					MetricName:     acc.MetricName,
					MetricRegex:    acc.MetricRegex,
					IsRegex:        acc.IsRegex,
					Origin:         metrics.OriginGrafanaDashboard,
					SourceID:       sourceID,
					SourceName:     dashTitle,
					SourceTags:     tags,
					SourceFolder:   folder,
					Query:          queryStr,
					ExplicitLabels: mergeStrings(acc.ExplicitLabels, []string{targetLabel}),
				})
			}
		}
		return
	}

	if m := queryResultRe.FindStringSubmatch(queryStr); len(m) == 2 {
		extracted, err := promql.Extract(SanitizePromQL(m[1]))
		if err == nil {
			for _, acc := range extracted.Accesses {
				res.Refs = append(res.Refs, metrics.UsageRef{
					MetricName:     acc.MetricName,
					MetricRegex:    acc.MetricRegex,
					IsRegex:        acc.IsRegex,
					Origin:         metrics.OriginGrafanaDashboard,
					SourceID:       sourceID,
					SourceName:     dashTitle,
					SourceTags:     tags,
					SourceFolder:   folder,
					Query:          queryStr,
					ExplicitLabels: acc.ExplicitLabels,
					PreservesAll:   acc.PreservesAll,
					WithoutLabels:  acc.WithoutLabels,
				})
			}
		}
		return
	}

	if m := metricsQueryRe.FindStringSubmatch(queryStr); len(m) == 2 {
		pat := strings.Trim(strings.TrimSpace(m[1]), `"'`)
		if pat != "" {
			res.Refs = append(res.Refs, metrics.UsageRef{
				MetricRegex:  pat,
				IsRegex:      true,
				Origin:       metrics.OriginGrafanaDashboard,
				SourceID:     sourceID,
				SourceName:   dashTitle,
				SourceTags:   tags,
				SourceFolder: folder,
				Query:        queryStr,
			})
		}
	}
}

type managedAlertRule struct {
	UID         string            `json:"uid"`
	Title       string            `json:"title"`
	RuleGroup   string            `json:"ruleGroup"`
	FolderUID   string            `json:"folderUID"`
	Annotations map[string]string `json:"annotations"`
	Labels      map[string]string `json:"labels"`
	Data        []struct {
		RefID string `json:"refId"`
		Model struct {
			Expr string `json:"expr"`
		} `json:"model"`
	} `json:"data"`
}

func parseManagedAlertsJSON(data []byte, res *ScanResult) error {
	var rules []managedAlertRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return err
	}
	for _, r := range rules {
		res.AlertRuleCount++
		var tplTexts []string
		for _, v := range r.Annotations {
			tplTexts = append(tplTexts, v)
		}
		for _, v := range r.Labels {
			tplTexts = append(tplTexts, v)
		}
		tplLabels := promrules.ExtractTemplateLabels(tplTexts...)
		sourceID := "grafana-alert:" + r.UID
		if r.UID == "" {
			sourceID = "grafana-alert:" + r.Title
		}
		for _, d := range r.Data {
			expr := strings.TrimSpace(d.Model.Expr)
			if expr == "" {
				continue
			}
			extracted, err := promql.Extract(SanitizePromQL(expr))
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("grafana alert %q: failed to parse expr: %v", r.Title, err))
				continue
			}
			for _, acc := range extracted.Accesses {
				res.Refs = append(res.Refs, metrics.UsageRef{
					MetricName:     acc.MetricName,
					MetricRegex:    acc.MetricRegex,
					IsRegex:        acc.IsRegex,
					Origin:         metrics.OriginGrafanaAlert,
					SourceID:       sourceID,
					SourceName:     r.Title,
					SourceFolder:   r.FolderUID,
					Query:          expr,
					ExplicitLabels: mergeStrings(acc.ExplicitLabels, tplLabels),
					PreservesAll:   acc.PreservesAll,
					WithoutLabels:  acc.WithoutLabels,
				})
			}
		}
	}
	return nil
}

func mergeStrings(a, b []string) []string {
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
