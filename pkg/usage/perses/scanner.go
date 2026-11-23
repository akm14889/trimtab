package perses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/grafana"
	"trimtab.io/trimtab/pkg/usage/promql"
)

// ScanResult holds extracted Perses dashboard PromQL refs and object count for baseline tracking.
type ScanResult struct {
	Refs           []metrics.UsageRef
	DashboardCount int
	Warnings       []string
}

var seriesNameFormatRe = regexp.MustCompile(`\{\{\s*(?:\$labels\.|\.Labels\.|\.)?([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// Scan loads Perses dashboards from local dashboardsDir and/or Perses HTTP API.
func Scan(ctx context.Context, cfg *config.PersesConfig) (*ScanResult, error) {
	res := &ScanResult{}
	if cfg == nil {
		return res, nil
	}

	if cfg.DashboardsDir != "" {
		if err := scanDashboardsDir(cfg.DashboardsDir, res); err != nil {
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
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		if strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read perses dashboardsDir %s: %w", dir, err)
	}
	sort.Strings(files)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read perses file %s: %w", f, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var doc persesDashboard
			err := dec.Decode(&doc)
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("parse perses file %s: %w", f, err)
			}
			if !doc.isDashboard() {
				continue
			}
			processDashboard(doc, "", filepath.ToSlash(f), res)
		}
	}
	return nil
}

func scanHTTP(ctx context.Context, cfg *config.PersesConfig, res *ScanResult) error {
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		return err
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
		return fmt.Errorf("perses client: %w", err)
	}
	res.Warnings = append(res.Warnings, client.Warnings()...)

	projects := make([]string, 0, len(cfg.Projects))
	if len(cfg.Projects) > 0 {
		for _, p := range cfg.Projects {
			if p = strings.TrimSpace(p); p != "" {
				projects = append(projects, p)
			}
		}
	} else {
		projBody, err := client.Get(ctx, "/api/v1/projects")
		if err != nil {
			return fmt.Errorf("perses %w", err)
		}
		var projDocs []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(projBody, &projDocs); err != nil {
			return fmt.Errorf("decode perses /api/v1/projects: %w", err)
		}
		for _, pd := range projDocs {
			if pd.Metadata.Name != "" {
				projects = append(projects, pd.Metadata.Name)
			}
		}
		sort.Strings(projects)
	}

	for _, project := range projects {
		endpoint := "/api/v1/projects/" + url.PathEscape(project) + "/dashboards"
		body, err := client.Get(ctx, endpoint)
		if err != nil {
			return fmt.Errorf("perses %w", err)
		}
		var dashboards []persesDashboard
		if err := json.Unmarshal(body, &dashboards); err != nil {
			return fmt.Errorf("decode perses %s: %w", endpoint, err)
		}
		for _, dash := range dashboards {
			if !dash.isDashboard() && dash.Metadata.Name == "" {
				continue
			}
			processDashboard(dash, project, project, res)
		}
	}
	return nil
}

type persesDashboard struct {
	Kind     string `yaml:"kind" json:"kind"`
	Metadata struct {
		Name    string `yaml:"name" json:"name"`
		Project string `yaml:"project" json:"project"`
	} `yaml:"metadata" json:"metadata"`
	Spec struct {
		Display struct {
			Name string `yaml:"name" json:"name"`
		} `yaml:"display" json:"display"`
		Panels    map[string]persesPanel `yaml:"panels" json:"panels"`
		Variables []persesVariable       `yaml:"variables" json:"variables"`
	} `yaml:"spec" json:"spec"`
}

func (d persesDashboard) isDashboard() bool {
	if strings.EqualFold(d.Kind, "Dashboard") {
		return true
	}
	return d.Kind == "" && (len(d.Spec.Panels) > 0 || len(d.Spec.Variables) > 0)
}

type persesPanel struct {
	Spec struct {
		Display struct {
			Name string `yaml:"name" json:"name"`
		} `yaml:"display" json:"display"`
		Queries []struct {
			Kind string `yaml:"kind" json:"kind"`
			Spec struct {
				Plugin struct {
					Kind string         `yaml:"kind" json:"kind"`
					Spec map[string]any `yaml:"spec" json:"spec"`
				} `yaml:"plugin" json:"plugin"`
			} `yaml:"spec" json:"spec"`
		} `yaml:"queries" json:"queries"`
	} `yaml:"spec" json:"spec"`
}

type persesVariable struct {
	Kind string `yaml:"kind" json:"kind"`
	Spec struct {
		Name   string `yaml:"name" json:"name"`
		Plugin struct {
			Kind string `yaml:"kind" json:"kind"`
			Spec struct {
				Expr      string   `yaml:"expr" json:"expr"`
				LabelName string   `yaml:"labelName" json:"labelName"`
				Matchers  []string `yaml:"matchers" json:"matchers"`
			} `yaml:"spec" json:"spec"`
		} `yaml:"plugin" json:"plugin"`
	} `yaml:"spec" json:"spec"`
}

func processDashboard(doc persesDashboard, projectHint, folderHint string, res *ScanResult) {
	project := doc.Metadata.Project
	if project == "" {
		project = projectHint
	}
	name := doc.Metadata.Name
	if name == "" {
		name = doc.Spec.Display.Name
	}
	uid := name
	if project != "" && name != "" {
		uid = project + "/" + name
	} else if uid == "" {
		uid = fmt.Sprintf("dashboard-%d", res.DashboardCount+1)
	}
	title := doc.Spec.Display.Name
	if title == "" {
		title = name
	}
	if folderHint == "" {
		folderHint = project
	}

	res.DashboardCount++
	sourceID := "perses-dashboard:" + uid

	panelKeys := make([]string, 0, len(doc.Spec.Panels))
	for k := range doc.Spec.Panels {
		panelKeys = append(panelKeys, k)
	}
	sort.Strings(panelKeys)

	for _, pk := range panelKeys {
		panel := doc.Spec.Panels[pk]
		panelTitle := panel.Spec.Display.Name
		if panelTitle == "" {
			panelTitle = pk
		}
		for _, q := range panel.Spec.Queries {
			if !strings.EqualFold(strings.TrimSpace(q.Spec.Plugin.Kind), "PrometheusTimeSeriesQuery") {
				continue
			}
			rawQuery := stringField(q.Spec.Plugin.Spec, "query")
			if rawQuery == "" {
				rawQuery = stringField(q.Spec.Plugin.Spec, "expr")
			}
			if strings.TrimSpace(rawQuery) == "" {
				continue
			}
			seriesFmt := stringField(q.Spec.Plugin.Spec, "seriesNameFormat")
			legendLabels := extractLegendLabels(seriesFmt)

			extracted, err := promql.Extract(grafana.SanitizePromQL(rawQuery))
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("perses %q panel %q: parse PromQL: %v", title, panelTitle, err))
				continue
			}
			appendPersesRefs(res, extracted.Accesses, sourceID, title, folderHint, uid, rawQuery, legendLabels)
		}
	}

	for _, v := range doc.Spec.Variables {
		pKind := strings.ToLower(strings.TrimSpace(v.Spec.Plugin.Kind))
		pSpec := v.Spec.Plugin.Spec
		switch pKind {
		case "prometheuslabelvaluesvariable":
			var extra []string
			if lbl := strings.TrimSpace(pSpec.LabelName); lbl != "" && lbl != "__name__" {
				extra = []string{lbl}
			}
			for _, matcher := range pSpec.Matchers {
				if strings.TrimSpace(matcher) == "" {
					continue
				}
				extracted, err := promql.Extract(grafana.SanitizePromQL(matcher))
				if err != nil {
					res.Warnings = append(res.Warnings, fmt.Sprintf("perses %q var %q matcher: parse PromQL: %v", title, v.Spec.Name, err))
					continue
				}
				appendPersesRefs(res, extracted.Accesses, sourceID, title, folderHint, uid, matcher, extra)
			}
		case "prometheuslabelnamesvariable":
			for _, matcher := range pSpec.Matchers {
				if strings.TrimSpace(matcher) == "" {
					continue
				}
				extracted, err := promql.Extract(grafana.SanitizePromQL(matcher))
				if err != nil {
					res.Warnings = append(res.Warnings, fmt.Sprintf("perses %q var %q matcher: parse PromQL: %v", title, v.Spec.Name, err))
					continue
				}
				appendPersesRefs(res, extracted.Accesses, sourceID, title, folderHint, uid, matcher, nil)
			}
		case "prometheuspromqlvariable":
			if strings.TrimSpace(pSpec.Expr) == "" {
				continue
			}
			var extra []string
			if lbl := strings.TrimSpace(pSpec.LabelName); lbl != "" && lbl != "__name__" {
				extra = []string{lbl}
			}
			extracted, err := promql.Extract(grafana.SanitizePromQL(pSpec.Expr))
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("perses %q var %q expr: parse PromQL: %v", title, v.Spec.Name, err))
				continue
			}
			appendPersesRefs(res, extracted.Accesses, sourceID, title, folderHint, uid, pSpec.Expr, extra)
		}
	}
}

func appendPersesRefs(
	res *ScanResult,
	accesses []promql.MetricAccess,
	sourceID, title, folder, uid, rawQuery string,
	extraLabels []string,
) {
	for _, acc := range accesses {
		res.Refs = append(res.Refs, metrics.UsageRef{
			MetricName:     acc.MetricName,
			MetricRegex:    acc.MetricRegex,
			IsRegex:        acc.IsRegex,
			Origin:         metrics.OriginPersesDashboard,
			SourceID:       sourceID,
			SourceName:     title,
			SourceFolder:   folder,
			DashboardUID:   uid,
			Query:          rawQuery,
			ExplicitLabels: mergeStrings(acc.ExplicitLabels, extraLabels),
			PreservesAll:   acc.PreservesAll,
			WithoutLabels:  acc.WithoutLabels,
		})
	}
}

func extractLegendLabels(format string) []string {
	if format == "" {
		return nil
	}
	var out []string
	for _, m := range seriesNameFormatRe.FindAllStringSubmatch(format, -1) {
		if len(m) > 1 && m[1] != "" && m[1] != "__name__" {
			out = append(out, m[1])
		}
	}
	return out
}

func stringField(m map[string]any, key string) string {
	if len(m) == 0 {
		return ""
	}
	s, _ := m[key].(string)
	return s
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
