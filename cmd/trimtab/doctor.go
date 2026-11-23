package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
)

type doctorCheckResult struct {
	Component string   `json:"component"`
	Target    string   `json:"target"`
	Status    string   `json:"status"` // "OK" | "FAIL"
	Detail    string   `json:"detail,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	LatencyMs int64    `json:"latencyMs"`
}

type doctorReport struct {
	ConfigPath string              `json:"configPath"`
	Passed     bool                `json:"passed"`
	Checks     []doctorCheckResult `json:"checks"`
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath string
		formatStr  string
	)
	fs.StringVar(&configPath, "config", "trimtab.yaml", "Path to trimtab.yaml")
	fs.StringVar(&formatStr, "format", "table", "Output format: table | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "trimtab doctor: %v\n", err)
		return 1
	}

	ctx := context.Background()
	rep := doctorReport{
		ConfigPath: configPath,
		Passed:     true,
	}

	record := func(res doctorCheckResult) {
		if res.Status != "OK" {
			rep.Passed = false
		}
		rep.Checks = append(rep.Checks, res)
	}

	// 1. Inventory check
	if cfg.Inventory.FixturePath != "" {
		record(checkFileReadable("inventory:fixture", cfg.Inventory.FixturePath))
	}
	if cfg.Inventory.PrometheusURL != "" {
		record(checkInventoryHTTP(ctx, cfg.Inventory))
	}

	// 2. Query logs check
	for i, ql := range cfg.Usage.QueryLogs {
		if ql.Glob != "" {
			record(checkGlobMatches(fmt.Sprintf("querylog:glob[%d]", i), ql.Glob))
		}
		if ql.Loki != nil && ql.Loki.URL != "" {
			record(checkLokiHTTP(ctx, i, ql.Loki))
		}
	}

	// 3. Grafana check
	if cfg.Usage.Grafana != nil {
		g := cfg.Usage.Grafana
		if g.DashboardsDir != "" {
			record(checkDirReadable("grafana:dashboardsDir", g.DashboardsDir))
		}
		if g.AlertRulesPath != "" {
			record(checkFileReadable("grafana:alertRulesPath", g.AlertRulesPath))
		}
		if g.URL != "" {
			record(checkGrafanaHTTP(ctx, g))
		}
	}

	// 4. Prometheus rules check
	for i, pat := range cfg.Usage.PromRules {
		record(checkGlobMatches(fmt.Sprintf("promrules:glob[%d]", i), pat))
	}

	// 5. External Ruler HTTP API checks (Mimir/Cortex/Prometheus/Thanos)
	for i, rc := range cfg.Usage.Rulers {
		record(checkRulerHTTP(ctx, i, rc))
	}

	// 6. Kubernetes workload consumer manifest globs
	if cfg.Usage.K8sConsumers != nil {
		for i, pat := range cfg.Usage.K8sConsumers.Globs {
			record(checkGlobMatches(fmt.Sprintf("k8sconsumers:glob[%d]", i), pat))
		}
	}

	// 7. Alertmanager config / HTTP API checks
	if cfg.Usage.Alertmanager != nil {
		am := cfg.Usage.Alertmanager
		for i, pat := range am.ConfigPaths {
			record(checkGlobMatches(fmt.Sprintf("alertmanager:config[%d]", i), pat))
		}
		if am.URL != "" {
			record(checkAlertmanagerHTTP(ctx, am))
		}
	}

	// 8. Perses dashboards dir / HTTP API checks
	if cfg.Usage.Perses != nil {
		ps := cfg.Usage.Perses
		if ps.DashboardsDir != "" {
			record(checkDirReadable("perses:dashboardsDir", ps.DashboardsDir))
		}
		if ps.URL != "" {
			record(checkPersesHTTP(ctx, ps))
		}
	}

	// 9. SLO specification globs
	if cfg.Usage.SLO != nil {
		for i, pat := range cfg.Usage.SLO.Globs {
			record(checkGlobMatches(fmt.Sprintf("slo:glob[%d]", i), pat))
		}
	}

	switch strings.ToLower(strings.TrimSpace(formatStr)) {
	case "", "table":
		renderDoctorTable(stdout, rep)
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	default:
		fmt.Fprintf(stderr, "trimtab doctor: unsupported --format %q (expected table or json)\n", formatStr)
		return 2
	}

	if !rep.Passed {
		return 1
	}
	return 0
}

func checkInventoryHTTP(ctx context.Context, cfg config.InventoryConfig) doctorCheckResult {
	start := time.Now()
	res := doctorCheckResult{Component: "inventory:prometheus", Target: cfg.PrometheusURL}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	hc, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.PrometheusURL,
		Timeout:     cfg.Timeout.Duration(),
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Warnings = hc.Warnings()
	if _, err := hc.Get(ctx, "/api/v1/status/runtimeinfo"); err != nil {
		if _, err2 := hc.Get(ctx, "/api/v1/label/__name__/values?limit=1"); err2 != nil {
			res.Status = "FAIL"
			res.Detail = err2.Error()
			res.LatencyMs = time.Since(start).Milliseconds()
			return res
		}
	}
	res.Status = "OK"
	res.Detail = "read-only Prometheus HTTP API reachable"
	if cfg.TenantID != "" {
		res.Detail += fmt.Sprintf(" (tenant=%s)", cfg.TenantID)
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	return res
}

func checkLokiHTTP(ctx context.Context, idx int, cfg *config.LokiQueryLogConfig) doctorCheckResult {
	start := time.Now()
	res := doctorCheckResult{Component: fmt.Sprintf("querylog:loki[%d]", idx), Target: cfg.URL}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	hc, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.URL,
		Timeout:     cfg.Timeout.Duration(),
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Warnings = hc.Warnings()
	if _, err := hc.Get(ctx, "/loki/api/v1/labels"); err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Status = "OK"
	res.Detail = "Loki read-only query API reachable"
	if cfg.TenantID != "" {
		res.Detail += fmt.Sprintf(" (tenant=%s)", cfg.TenantID)
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	return res
}

func checkGrafanaHTTP(ctx context.Context, cfg *config.GrafanaConfig) doctorCheckResult {
	start := time.Now()
	res := doctorCheckResult{Component: "grafana:http", Target: cfg.URL}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	hc, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.URL,
		Timeout:     cfg.Timeout.Duration(),
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Warnings = hc.Warnings()
	if _, err := hc.Get(ctx, "/api/search?type=dash-db&limit=1"); err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	if cfg.ManagedAlertsEnabled() {
		alertEndpoint := "/api/v1/provisioning/alert-rules"
		if strings.EqualFold(cfg.AlertAPI, "ruler") {
			alertEndpoint = "/api/ruler/grafana/api/v1/rules"
		}
		if _, err := hc.Get(ctx, alertEndpoint); err != nil {
			res.Status = "FAIL"
			res.Detail = "dashboards OK, alerts check failed: " + err.Error()
			res.LatencyMs = time.Since(start).Milliseconds()
			return res
		}
	}
	res.Status = "OK"
	res.Detail = "Grafana dashboards & alerts read-only APIs reachable"
	res.LatencyMs = time.Since(start).Milliseconds()
	return res
}

func checkRulerHTTP(ctx context.Context, idx int, cfg config.RulerConfig) doctorCheckResult {
	start := time.Now()
	rType := strings.ToLower(strings.TrimSpace(cfg.Type))
	if rType == "" {
		rType = "prometheus"
	}
	res := doctorCheckResult{
		Component: fmt.Sprintf("ruler:%s[%d]", rType, idx),
		Target:    cfg.URL,
	}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	hc, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.URL,
		Timeout:     cfg.Timeout.Duration(),
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Warnings = hc.Warnings()

	endpoint := "/api/v1/rules"
	if rType == "mimir" || rType == "cortex" {
		endpoint = "/prometheus/config/v1/rules"
	}
	if _, err := hc.Get(ctx, endpoint); err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Status = "OK"
	res.Detail = fmt.Sprintf("read-only %s Ruler API reachable", rType)
	if cfg.TenantID != "" {
		res.Detail += fmt.Sprintf(" (tenant=%s)", cfg.TenantID)
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	return res
}

func probeHTTPComponent(
	ctx context.Context,
	component, baseURL string,
	timeout time.Duration,
	tenantID string,
	headers, headersEnv, headersFile map[string]string,
	authErr error,
	auth config.HTTPAuthConfig,
	endpoint, okDetail string,
) doctorCheckResult {
	start := time.Now()
	res := doctorCheckResult{Component: component, Target: baseURL}
	if authErr != nil {
		res.Status = "FAIL"
		res.Detail = authErr.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	hc, err := httpclient.New(httpclient.Options{
		BaseURL:     baseURL,
		Timeout:     timeout,
		TenantID:    tenantID,
		Headers:     headers,
		HeadersEnv:  headersEnv,
		HeadersFile: headersFile,
		Auth:        auth,
	})
	if err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Warnings = hc.Warnings()
	if _, err := hc.Get(ctx, endpoint); err != nil {
		res.Status = "FAIL"
		res.Detail = err.Error()
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	res.Status = "OK"
	res.Detail = okDetail
	if tenantID != "" {
		res.Detail += fmt.Sprintf(" (tenant=%s)", tenantID)
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	return res
}

func checkAlertmanagerHTTP(ctx context.Context, cfg *config.AlertmanagerConfig) doctorCheckResult {
	auth, err := cfg.EffectiveAuth()
	return probeHTTPComponent(
		ctx,
		"alertmanager:http",
		cfg.URL,
		cfg.Timeout.Duration(),
		cfg.TenantID,
		cfg.Headers,
		cfg.HeadersEnv,
		cfg.HeadersFile,
		err,
		auth,
		"/api/v2/status",
		"Alertmanager read-only status API reachable",
	)
}

func checkPersesHTTP(ctx context.Context, cfg *config.PersesConfig) doctorCheckResult {
	auth, err := cfg.EffectiveAuth()
	return probeHTTPComponent(
		ctx,
		"perses:http",
		cfg.URL,
		cfg.Timeout.Duration(),
		cfg.TenantID,
		cfg.Headers,
		cfg.HeadersEnv,
		cfg.HeadersFile,
		err,
		auth,
		"/api/v1/projects",
		"Perses read-only projects API reachable",
	)
}

func checkFileReadable(component, path string) doctorCheckResult {
	start := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return doctorCheckResult{Component: component, Target: path, Status: "FAIL", Detail: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	if info.IsDir() {
		return checkDirReadable(component, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return doctorCheckResult{Component: component, Target: path, Status: "FAIL", Detail: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	_ = f.Close()
	return doctorCheckResult{Component: component, Target: path, Status: "OK", Detail: "readable", LatencyMs: time.Since(start).Milliseconds()}
}

func checkDirReadable(component, path string) doctorCheckResult {
	start := time.Now()
	entries, err := os.ReadDir(path)
	if err != nil {
		return doctorCheckResult{Component: component, Target: path, Status: "FAIL", Detail: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	return doctorCheckResult{Component: component, Target: path, Status: "OK", Detail: fmt.Sprintf("readable directory (%d entries)", len(entries)), LatencyMs: time.Since(start).Milliseconds()}
}

func checkGlobMatches(component, pattern string) doctorCheckResult {
	start := time.Now()
	if info, err := os.Stat(pattern); err == nil && info.IsDir() {
		return checkDirReadable(component, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return doctorCheckResult{Component: component, Target: pattern, Status: "FAIL", Detail: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	if len(matches) == 0 {
		return doctorCheckResult{Component: component, Target: pattern, Status: "FAIL", Detail: "glob matched 0 files", LatencyMs: time.Since(start).Milliseconds()}
	}
	return doctorCheckResult{Component: component, Target: pattern, Status: "OK", Detail: fmt.Sprintf("matched %d file(s)", len(matches)), LatencyMs: time.Since(start).Milliseconds()}
}

func renderDoctorTable(w io.Writer, rep doctorReport) {
	fmt.Fprintf(w, "TRIMTAB DOCTOR — %s\n", rep.ConfigPath)
	tw := tabwriter.NewWriter(w, 0, 2, 3, ' ', 0)
	fmt.Fprintln(tw, "COMPONENT\tSTATUS\tLATENCY\tTARGET\tDETAIL")
	for _, c := range rep.Checks {
		fmt.Fprintf(tw, "%s\t%s\t%dms\t%s\t%s\n", c.Component, c.Status, c.LatencyMs, c.Target, c.Detail)
		for _, warn := range c.Warnings {
			fmt.Fprintf(tw, "  !\tWARN\t-\t-\t%s\n", warn)
		}
	}
	_ = tw.Flush()
	if rep.Passed {
		fmt.Fprintln(w, "Doctor check PASSED (0 failures).")
	} else {
		fmt.Fprintln(w, "Doctor check FAILED.")
	}
}
