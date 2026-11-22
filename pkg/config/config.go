package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration and supports day ("d") and week ("w") units in YAML/JSON
// (e.g. "30d", "7d", "24h", "15m"), in addition to standard Go time.ParseDuration syntax.
type Duration time.Duration

func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "w") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "w"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return Duration(time.Duration(n * 7 * 24 * float64(time.Hour))), nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return Duration(time.Duration(n * 24 * float64(time.Hour))), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return Duration(d), nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

func (d Duration) Days() float64 {
	return time.Duration(d).Hours() / 24.0
}

func (d Duration) String() string {
	td := time.Duration(d)
	if td > 0 && td%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", td/(24*time.Hour))
	}
	return td.String()
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := ParseDuration(s)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	}
	var ns int64
	if err := json.Unmarshal(b, &ns); err != nil {
		return fmt.Errorf("invalid duration JSON %s", string(b))
	}
	*d = Duration(time.Duration(ns))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// DefaultNeverDropPatterns returns the mandatory meta-monitoring allowlist (Invariant #6).
func DefaultNeverDropPatterns() []string {
	return []string{
		`^up$`,
		`^ALERTS$`,
		`^ALERTS_FOR_STATE$`,
		`^scrape_.*`,
		`^prometheus_.*`,
		`^alloy_.*`,
		`^otelcol_.*`,
	}
}

// Config is the root trimtab.yaml configuration schema.
type Config struct {
	Name        string            `yaml:"name" json:"name"`
	StatePath   string            `yaml:"statePath" json:"statePath"`
	Inventory   InventoryConfig   `yaml:"inventory" json:"inventory"`
	Usage       UsageConfig       `yaml:"usage" json:"usage"`
	Policy      PolicyConfig      `yaml:"policy" json:"policy"`
	Synthesizer SynthesizerConfig `yaml:"synthesizer" json:"synthesizer"`
}

type InventoryConfig struct {
	PrometheusURL         string            `yaml:"prometheusURL" json:"prometheusURL"`
	BearerToken           string            `yaml:"bearerToken" json:"bearerToken"`
	Headers               map[string]string `yaml:"headers" json:"headers"`
	FixturePath           string            `yaml:"fixturePath" json:"fixturePath"`
	Timeout               Duration          `yaml:"timeout" json:"timeout"`
	EnableCollapseQueries *bool             `yaml:"enableCollapseQueries" json:"enableCollapseQueries"`
	MaxCollapseFamilies   int               `yaml:"maxCollapseFamilies" json:"maxCollapseFamilies"`
	// NamingMode documents how metric names enter the pipeline:
	// "prometheus_receiver", "utf8_no_translation", "explicit_otlp", or "translated_ambiguous".
	NamingMode  string            `yaml:"namingMode" json:"namingMode"`
	OTLPNameMap map[string]string `yaml:"otlpNameMap" json:"otlpNameMap"`
}

func (c InventoryConfig) CollapseQueriesEnabled() bool {
	if c.EnableCollapseQueries == nil {
		return true
	}
	return *c.EnableCollapseQueries
}

type UsageConfig struct {
	QueryLogs []QueryLogSourceConfig `yaml:"queryLogs" json:"queryLogs"`
	Grafana   *GrafanaConfig         `yaml:"grafana" json:"grafana"`
	PromRules []string               `yaml:"promRules" json:"promRules"`
}

type QueryLogSourceConfig struct {
	Glob   string              `yaml:"glob" json:"glob"`
	Format string              `yaml:"format" json:"format"` // "auto" (default), "prometheus", "prom-analytics-proxy"
	Loki   *LokiQueryLogConfig `yaml:"loki" json:"loki"`
}

type LokiQueryLogConfig struct {
	URL         string            `yaml:"url" json:"url"`
	Query       string            `yaml:"query" json:"query"` // LogQL selector e.g. `{job="prometheus", component="query-log"}`
	TenantID    string            `yaml:"tenantID" json:"tenantID"`
	BearerToken string            `yaml:"bearerToken" json:"bearerToken"`
	Headers     map[string]string `yaml:"headers" json:"headers"`
	Window      Duration          `yaml:"window" json:"window"` // Lookback window; defaults to policy.minObservationWindow
	ChunkSize   Duration          `yaml:"chunkSize" json:"chunkSize"`
	Limit       int               `yaml:"limit" json:"limit"`
	Timeout     Duration          `yaml:"timeout" json:"timeout"`
}

type GrafanaConfig struct {
	URL                 string            `yaml:"url" json:"url"`
	APIKey              string            `yaml:"apiKey" json:"apiKey"`
	Headers             map[string]string `yaml:"headers" json:"headers"`
	DashboardsDir       string            `yaml:"dashboardsDir" json:"dashboardsDir"`
	AlertRulesPath      string            `yaml:"alertRulesPath" json:"alertRulesPath"`
	EnableManagedAlerts *bool             `yaml:"enableManagedAlerts" json:"enableManagedAlerts"`
	Timeout             Duration          `yaml:"timeout" json:"timeout"`
}

func (g *GrafanaConfig) ManagedAlertsEnabled() bool {
	if g == nil {
		return false
	}
	if g.EnableManagedAlerts == nil {
		return true
	}
	return *g.EnableManagedAlerts
}

type PolicyConfig struct {
	MinObservationWindow          Duration        `yaml:"minObservationWindow" json:"minObservationWindow"`
	SoakWindow                    Duration        `yaml:"soakWindow" json:"soakWindow"`
	MaxDropPercentPerTranche      float64         `yaml:"maxDropPercentPerTranche" json:"maxDropPercentPerTranche"`
	MaxTotalDroppablePercent      float64         `yaml:"maxTotalDroppablePercent" json:"maxTotalDroppablePercent"`
	BaselineDriftThresholdPercent float64         `yaml:"baselineDriftThresholdPercent" json:"baselineDriftThresholdPercent"`
	WildcardThresholdPercent      float64         `yaml:"wildcardThresholdPercent" json:"wildcardThresholdPercent"`
	RegretThreshold               int             `yaml:"regretThreshold" json:"regretThreshold"`
	MinRegretHits                 int             `yaml:"minRegretHits" json:"minRegretHits"`
	NeverDrop                     []string        `yaml:"neverDrop" json:"neverDrop"`
	DisableDefaultNeverDrop       bool            `yaml:"disableDefaultNeverDrop" json:"disableDefaultNeverDrop"`
	ExcludeSources                []ExcludeSource `yaml:"excludeSources" json:"excludeSources"`
	ExcludeRegretCallers          []string        `yaml:"excludeRegretCallers" json:"excludeRegretCallers"`
}

type ExcludeSource struct {
	// Type: "dashboard_uid", "dashboard_title", "dashboard_folder", "dashboard_tag",
	// "query_caller", "query_path", "query_regex", "rule_file".
	Type  string `yaml:"type" json:"type"`
	Value string `yaml:"value" json:"value"`
}

type SynthesizerConfig struct {
	Target     string        `yaml:"target" json:"target"`         // "alloy" (default), "otel", or "promrw"
	OutputPath string        `yaml:"outputPath" json:"outputPath"` // optional output path
	PromRW     *PromRWConfig `yaml:"promRW,omitempty" json:"promRW,omitempty"`
}

// PromRWConfig configures Prometheus remote_write[].write_relabel_configs synthesis.
type PromRWConfig struct {
	Mode              string `yaml:"mode,omitempty" json:"mode,omitempty"`                           // "relabel_configs" (default), "remote_write", or "prometheus_operator_patch"
	RemoteWriteName   string `yaml:"remoteWriteName,omitempty" json:"remoteWriteName,omitempty"`     // optional remote_write[].name
	RemoteWriteURL    string `yaml:"remoteWriteURL,omitempty" json:"remoteWriteURL,omitempty"`       // optional remote_write[].url (required for prometheus_operator_patch)
	OperatorKind      string `yaml:"operatorKind,omitempty" json:"operatorKind,omitempty"`           // "Prometheus" (default) or "PrometheusAgent"
	OperatorName      string `yaml:"operatorName,omitempty" json:"operatorName,omitempty"`           // metadata.name (default "prometheus")
	OperatorNamespace string `yaml:"operatorNamespace,omitempty" json:"operatorNamespace,omitempty"` // optional metadata.namespace
	MaxTermsPerRule   int    `yaml:"maxTermsPerRule,omitempty" json:"maxTermsPerRule,omitempty"`     // default 40
}

// EffectiveRegretThreshold returns the active threshold (default 1 per Invariant #10).
func (p PolicyConfig) EffectiveRegretThreshold() int {
	if p.MinRegretHits > 0 {
		return p.MinRegretHits
	}
	if p.RegretThreshold > 0 {
		return p.RegretThreshold
	}
	return 1
}

// EffectiveNeverDropPatterns merges default meta-monitoring patterns with user patterns.
func (p PolicyConfig) EffectiveNeverDropPatterns() []string {
	var out []string
	seen := make(map[string]struct{})
	if !p.DisableDefaultNeverDrop {
		for _, pat := range DefaultNeverDropPatterns() {
			seen[pat] = struct{}{}
			out = append(out, pat)
		}
	}
	for _, pat := range p.NeverDrop {
		if _, ok := seen[pat]; !ok {
			seen[pat] = struct{}{}
			out = append(out, pat)
		}
	}
	return out
}

// DefaultConfig returns a Config populated with all safe Trimtab defaults.
func DefaultConfig() *Config {
	return &Config{
		Name:      "default",
		StatePath: ".trimtab-state.json",
		Inventory: InventoryConfig{
			Timeout:             Duration(30 * time.Second),
			MaxCollapseFamilies: 25,
			NamingMode:          "prometheus_receiver",
		},
		Policy: PolicyConfig{
			MinObservationWindow:          Duration(30 * 24 * time.Hour),
			SoakWindow:                    Duration(7 * 24 * time.Hour),
			MaxDropPercentPerTranche:      15.0,
			MaxTotalDroppablePercent:      80.0,
			BaselineDriftThresholdPercent: 20.0,
			WildcardThresholdPercent:      50.0,
			RegretThreshold:               1,
		},
		Synthesizer: SynthesizerConfig{
			Target: "alloy",
		},
	}
}

// ApplyDefaults fills any unset fields with safe defaults.
func (c *Config) ApplyDefaults() {
	def := DefaultConfig()
	if c.Name == "" {
		c.Name = def.Name
	}
	if c.StatePath == "" {
		c.StatePath = def.StatePath
	}
	if c.Inventory.Timeout == 0 {
		c.Inventory.Timeout = def.Inventory.Timeout
	}
	if c.Inventory.MaxCollapseFamilies <= 0 {
		c.Inventory.MaxCollapseFamilies = def.Inventory.MaxCollapseFamilies
	}
	if c.Inventory.NamingMode == "" {
		c.Inventory.NamingMode = def.Inventory.NamingMode
	}
	if c.Policy.MinObservationWindow == 0 {
		c.Policy.MinObservationWindow = def.Policy.MinObservationWindow
	}
	if c.Policy.SoakWindow == 0 {
		c.Policy.SoakWindow = def.Policy.SoakWindow
	}
	if c.Policy.MaxDropPercentPerTranche <= 0 {
		c.Policy.MaxDropPercentPerTranche = def.Policy.MaxDropPercentPerTranche
	}
	if c.Policy.MaxTotalDroppablePercent <= 0 {
		c.Policy.MaxTotalDroppablePercent = def.Policy.MaxTotalDroppablePercent
	}
	if c.Policy.BaselineDriftThresholdPercent <= 0 {
		c.Policy.BaselineDriftThresholdPercent = def.Policy.BaselineDriftThresholdPercent
	}
	if c.Policy.WildcardThresholdPercent <= 0 {
		c.Policy.WildcardThresholdPercent = def.Policy.WildcardThresholdPercent
	}
	if c.Policy.RegretThreshold <= 0 {
		c.Policy.RegretThreshold = def.Policy.RegretThreshold
	}
	if c.Synthesizer.Target == "" {
		c.Synthesizer.Target = def.Synthesizer.Target
	}
	if c.Synthesizer.PromRW != nil {
		if c.Synthesizer.PromRW.Mode == "" {
			c.Synthesizer.PromRW.Mode = "relabel_configs"
		}
		if c.Synthesizer.PromRW.OperatorKind == "" {
			c.Synthesizer.PromRW.OperatorKind = "Prometheus"
		}
		if c.Synthesizer.PromRW.OperatorName == "" {
			c.Synthesizer.PromRW.OperatorName = "prometheus"
		}
		if c.Synthesizer.PromRW.MaxTermsPerRule == 0 {
			c.Synthesizer.PromRW.MaxTermsPerRule = 40
		}
	}
	for i := range c.Usage.QueryLogs {
		if c.Usage.QueryLogs[i].Format == "" {
			c.Usage.QueryLogs[i].Format = "auto"
		}
		if c.Usage.QueryLogs[i].Loki != nil {
			if c.Usage.QueryLogs[i].Loki.Timeout == 0 {
				c.Usage.QueryLogs[i].Loki.Timeout = Duration(30 * time.Second)
			}
			if c.Usage.QueryLogs[i].Loki.Limit <= 0 {
				c.Usage.QueryLogs[i].Loki.Limit = 5000
			}
			if c.Usage.QueryLogs[i].Loki.ChunkSize == 0 {
				c.Usage.QueryLogs[i].Loki.ChunkSize = Duration(24 * time.Hour)
			}
		}
	}
	if c.Usage.Grafana != nil && c.Usage.Grafana.Timeout == 0 {
		c.Usage.Grafana.Timeout = Duration(30 * time.Second)
	}
}

// Validate checks configuration sanity and regex syntax.
func (c *Config) Validate() error {
	if c.Inventory.PrometheusURL == "" && c.Inventory.FixturePath == "" {
		return errors.New("inventory requires either prometheusURL or fixturePath")
	}
	if c.Policy.MaxDropPercentPerTranche <= 0 || c.Policy.MaxDropPercentPerTranche > 100 {
		return fmt.Errorf("policy.maxDropPercentPerTranche must be in (0, 100], got %v", c.Policy.MaxDropPercentPerTranche)
	}
	if c.Policy.MaxTotalDroppablePercent <= 0 || c.Policy.MaxTotalDroppablePercent > 100 {
		return fmt.Errorf("policy.maxTotalDroppablePercent must be in (0, 100], got %v", c.Policy.MaxTotalDroppablePercent)
	}
	if c.Policy.BaselineDriftThresholdPercent <= 0 || c.Policy.BaselineDriftThresholdPercent > 100 {
		return fmt.Errorf("policy.baselineDriftThresholdPercent must be in (0, 100], got %v", c.Policy.BaselineDriftThresholdPercent)
	}
	if c.Policy.WildcardThresholdPercent <= 0 || c.Policy.WildcardThresholdPercent > 100 {
		return fmt.Errorf("policy.wildcardThresholdPercent must be in (0, 100], got %v", c.Policy.WildcardThresholdPercent)
	}
	if c.Policy.MinObservationWindow <= 0 {
		return errors.New("policy.minObservationWindow must be positive")
	}
	for _, pat := range c.Policy.EffectiveNeverDropPatterns() {
		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("invalid neverDrop regex %q: %w", pat, err)
		}
	}
	for _, ex := range c.Policy.ExcludeSources {
		if ex.Type == "query_regex" {
			if _, err := regexp.Compile(ex.Value); err != nil {
				return fmt.Errorf("invalid excludeSources query_regex %q: %w", ex.Value, err)
			}
		}
	}
	switch strings.ToLower(c.Synthesizer.Target) {
	case "alloy", "otel", "promrw", "prometheus-remote-write":
	default:
		return fmt.Errorf("unsupported synthesizer.target %q (expected \"alloy\", \"otel\", or \"promrw\")", c.Synthesizer.Target)
	}
	if prw := c.Synthesizer.PromRW; prw != nil {
		switch prw.Mode {
		case "", "relabel_configs", "remote_write", "prometheus_operator_patch":
		default:
			return fmt.Errorf("unsupported synthesizer.promRW.mode %q (expected \"relabel_configs\", \"remote_write\", or \"prometheus_operator_patch\")", prw.Mode)
		}
		if prw.Mode == "remote_write" && prw.RemoteWriteURL == "" && prw.RemoteWriteName == "" {
			return errors.New("synthesizer.promRW requires remoteWriteURL or remoteWriteName when mode is \"remote_write\"")
		}
		if prw.Mode == "prometheus_operator_patch" {
			if prw.RemoteWriteURL == "" {
				return errors.New("synthesizer.promRW.remoteWriteURL is required when mode is \"prometheus_operator_patch\"")
			}
			if prw.OperatorKind != "" && prw.OperatorKind != "Prometheus" && prw.OperatorKind != "PrometheusAgent" {
				return fmt.Errorf("unsupported synthesizer.promRW.operatorKind %q (expected \"Prometheus\" or \"PrometheusAgent\")", prw.OperatorKind)
			}
		}
		if prw.MaxTermsPerRule < 0 {
			return fmt.Errorf("synthesizer.promRW.maxTermsPerRule must be >= 0, got %d", prw.MaxTermsPerRule)
		}
	}
	return nil
}

// Load parses a trimtab.yaml file, resolves relative paths against the config directory,
// applies defaults, and validates the configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.ApplyDefaults()

	baseDir := filepath.Dir(path)
	resolvePath := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	cfg.StatePath = resolvePath(cfg.StatePath)
	cfg.Inventory.FixturePath = resolvePath(cfg.Inventory.FixturePath)
	for i := range cfg.Usage.QueryLogs {
		cfg.Usage.QueryLogs[i].Glob = resolvePath(cfg.Usage.QueryLogs[i].Glob)
	}
	if cfg.Usage.Grafana != nil {
		cfg.Usage.Grafana.DashboardsDir = resolvePath(cfg.Usage.Grafana.DashboardsDir)
		cfg.Usage.Grafana.AlertRulesPath = resolvePath(cfg.Usage.Grafana.AlertRulesPath)
	}
	for i := range cfg.Usage.PromRules {
		cfg.Usage.PromRules[i] = resolvePath(cfg.Usage.PromRules[i])
	}
	cfg.Synthesizer.OutputPath = resolvePath(cfg.Synthesizer.OutputPath)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
