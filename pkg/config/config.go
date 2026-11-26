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
		`^trimtab_.*`,
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

// BasicAuthConfig configures HTTP Basic Auth using literal, env-backed, or file-backed credentials.
type BasicAuthConfig struct {
	Username     string `yaml:"username,omitempty" json:"username,omitempty"`
	UsernameEnv  string `yaml:"usernameEnv,omitempty" json:"usernameEnv,omitempty"`
	UsernameFile string `yaml:"usernameFile,omitempty" json:"usernameFile,omitempty"`
	Password     string `yaml:"password,omitempty" json:"password,omitempty"`
	PasswordEnv  string `yaml:"passwordEnv,omitempty" json:"passwordEnv,omitempty"`
	PasswordFile string `yaml:"passwordFile,omitempty" json:"passwordFile,omitempty"`
}

// TLSConfig configures TLS trust anchors and optional mTLS client certificates.
type TLSConfig struct {
	CAFile             string `yaml:"caFile,omitempty" json:"caFile,omitempty"`
	CertFile           string `yaml:"certFile,omitempty" json:"certFile,omitempty"`
	KeyFile            string `yaml:"keyFile,omitempty" json:"keyFile,omitempty"`
	ServerName         string `yaml:"serverName,omitempty" json:"serverName,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty" json:"insecureSkipVerify,omitempty"`
}

// HTTPAuthConfig provides unified, vendor-neutral authentication across all HTTP backends.
type HTTPAuthConfig struct {
	BearerToken     string            `yaml:"bearerToken,omitempty" json:"bearerToken,omitempty"`
	BearerTokenEnv  string            `yaml:"bearerTokenEnv,omitempty" json:"bearerTokenEnv,omitempty"`
	BearerTokenFile string            `yaml:"bearerTokenFile,omitempty" json:"bearerTokenFile,omitempty"`
	BasicAuth       *BasicAuthConfig  `yaml:"basicAuth,omitempty" json:"basicAuth,omitempty"`
	HeadersEnv      map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile     map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	TLS             *TLSConfig        `yaml:"tls,omitempty" json:"tls,omitempty"`
}

// Validate enforces fail-closed mutual exclusion across primary auth modes and TLS pairs.
func (a HTTPAuthConfig) Validate(prefix string) error {
	bearerCount := countNonEmpty(a.BearerToken, a.BearerTokenEnv, a.BearerTokenFile)
	if bearerCount > 1 {
		return fmt.Errorf("%s: bearerToken, bearerTokenEnv, and bearerTokenFile are mutually exclusive", prefix)
	}
	if a.BasicAuth != nil {
		if bearerCount > 0 {
			return fmt.Errorf("%s: basicAuth and bearerToken* are mutually exclusive", prefix)
		}
		uCount := countNonEmpty(a.BasicAuth.Username, a.BasicAuth.UsernameEnv, a.BasicAuth.UsernameFile)
		pCount := countNonEmpty(a.BasicAuth.Password, a.BasicAuth.PasswordEnv, a.BasicAuth.PasswordFile)
		if uCount > 1 {
			return fmt.Errorf("%s.basicAuth: username, usernameEnv, and usernameFile are mutually exclusive", prefix)
		}
		if pCount > 1 {
			return fmt.Errorf("%s.basicAuth: password, passwordEnv, and passwordFile are mutually exclusive", prefix)
		}
		if uCount == 0 && pCount == 0 {
			return fmt.Errorf("%s.basicAuth: requires username or password", prefix)
		}
	}
	if a.TLS != nil {
		if (a.TLS.CertFile == "") != (a.TLS.KeyFile == "") {
			return fmt.Errorf("%s.tls: certFile and keyFile must both be provided for mTLS", prefix)
		}
	}
	return nil
}

func countNonEmpty(vals ...string) int {
	n := 0
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			n++
		}
	}
	return n
}

func mergeHTTPAuth(legacyBearer string, auth *HTTPAuthConfig, topHeadersEnv, topHeadersFile map[string]string, prefix string) (HTTPAuthConfig, error) {
	var out HTTPAuthConfig
	if auth != nil {
		out = *auth
	}
	if legacyBearer != "" {
		if out.BasicAuth != nil {
			return out, fmt.Errorf("%s: legacy bearer token/apiKey conflicts with auth.basicAuth", prefix)
		}
		if out.BearerTokenEnv != "" || out.BearerTokenFile != "" || (out.BearerToken != "" && out.BearerToken != legacyBearer) {
			return out, fmt.Errorf("%s: legacy bearer token/apiKey conflicts with auth.bearerToken*", prefix)
		}
		out.BearerToken = legacyBearer
	}
	if len(topHeadersEnv) > 0 {
		merged := make(map[string]string, len(topHeadersEnv)+len(out.HeadersEnv))
		for k, v := range topHeadersEnv {
			merged[k] = v
		}
		for k, v := range out.HeadersEnv {
			merged[k] = v
		}
		out.HeadersEnv = merged
	}
	if len(topHeadersFile) > 0 {
		merged := make(map[string]string, len(topHeadersFile)+len(out.HeadersFile))
		for k, v := range topHeadersFile {
			merged[k] = v
		}
		for k, v := range out.HeadersFile {
			merged[k] = v
		}
		out.HeadersFile = merged
	}
	if err := out.Validate(prefix); err != nil {
		return out, err
	}
	return out, nil
}

func resolveAuthPaths(auth *HTTPAuthConfig, resolve func(string) string) {
	if auth == nil {
		return
	}
	auth.BearerTokenFile = resolve(auth.BearerTokenFile)
	if auth.BasicAuth != nil {
		auth.BasicAuth.UsernameFile = resolve(auth.BasicAuth.UsernameFile)
		auth.BasicAuth.PasswordFile = resolve(auth.BasicAuth.PasswordFile)
	}
	for k, v := range auth.HeadersFile {
		auth.HeadersFile[k] = resolve(v)
	}
	if auth.TLS != nil {
		auth.TLS.CAFile = resolve(auth.TLS.CAFile)
		auth.TLS.CertFile = resolve(auth.TLS.CertFile)
		auth.TLS.KeyFile = resolve(auth.TLS.KeyFile)
	}
}

func resolvePathMap(m map[string]string, resolve func(string) string) {
	for k, v := range m {
		m[k] = resolve(v)
	}
}

type InventoryConfig struct {
	PrometheusURL         string            `yaml:"prometheusURL" json:"prometheusURL"`
	TenantID              string            `yaml:"tenantID,omitempty" json:"tenantID,omitempty"`
	BearerToken           string            `yaml:"bearerToken" json:"bearerToken"`
	Headers               map[string]string `yaml:"headers" json:"headers"`
	HeadersEnv            map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile           map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	Auth                  *HTTPAuthConfig   `yaml:"auth,omitempty" json:"auth,omitempty"`
	Selector              string            `yaml:"selector,omitempty" json:"selector,omitempty"`
	TSDBLimit             int               `yaml:"tsdbLimit,omitempty" json:"tsdbLimit,omitempty"`
	ProbeFirstSeen        bool              `yaml:"probeFirstSeen,omitempty" json:"probeFirstSeen,omitempty"`
	FixturePath           string            `yaml:"fixturePath" json:"fixturePath"`
	Timeout               Duration          `yaml:"timeout" json:"timeout"`
	EnableCollapseQueries *bool             `yaml:"enableCollapseQueries" json:"enableCollapseQueries"`
	MaxCollapseFamilies   int               `yaml:"maxCollapseFamilies" json:"maxCollapseFamilies"`
	// NamingMode documents how metric names enter the pipeline:
	// "prometheus_receiver", "utf8_no_translation", "explicit_otlp", or "translated_ambiguous".
	NamingMode  string            `yaml:"namingMode" json:"namingMode"`
	OTLPNameMap map[string]string `yaml:"otlpNameMap" json:"otlpNameMap"`
}

func (c InventoryConfig) EffectiveAuth() (HTTPAuthConfig, error) {
	return mergeHTTPAuth(c.BearerToken, c.Auth, c.HeadersEnv, c.HeadersFile, "inventory.auth")
}

func (c InventoryConfig) CollapseQueriesEnabled() bool {
	if c.EnableCollapseQueries == nil {
		return true
	}
	return *c.EnableCollapseQueries
}

type UsageConfig struct {
	QueryLogs    []QueryLogSourceConfig `yaml:"queryLogs" json:"queryLogs"`
	Grafana      *GrafanaConfig         `yaml:"grafana" json:"grafana"`
	PromRules    []string               `yaml:"promRules" json:"promRules"`
	Rulers       []RulerConfig          `yaml:"rulers,omitempty" json:"rulers,omitempty"`
	K8sConsumers *K8sConsumersConfig    `yaml:"k8sConsumers,omitempty" json:"k8sConsumers,omitempty"`
	Alertmanager *AlertmanagerConfig    `yaml:"alertmanager,omitempty" json:"alertmanager,omitempty"`
	Perses       *PersesConfig          `yaml:"perses,omitempty" json:"perses,omitempty"`
	SLO          *SLOConfig             `yaml:"slo,omitempty" json:"slo,omitempty"`
}

// AlertmanagerConfig configures Alertmanager YAML file/glob or /api/v2/status scanning (P2.3).
type AlertmanagerConfig struct {
	ConfigPaths []string          `yaml:"configPaths,omitempty" json:"configPaths,omitempty"`
	URL         string            `yaml:"url,omitempty" json:"url,omitempty"`
	TenantID    string            `yaml:"tenantID,omitempty" json:"tenantID,omitempty"`
	BearerToken string            `yaml:"bearerToken,omitempty" json:"bearerToken,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	HeadersEnv  map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	Auth        *HTTPAuthConfig   `yaml:"auth,omitempty" json:"auth,omitempty"`
	Timeout     Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

func (c AlertmanagerConfig) EffectiveAuth() (HTTPAuthConfig, error) {
	return mergeHTTPAuth(c.BearerToken, c.Auth, c.HeadersEnv, c.HeadersFile, "usage.alertmanager.auth")
}

// PersesConfig configures Perses local dashboard directory or REST API scanning (P2.3).
type PersesConfig struct {
	URL           string            `yaml:"url,omitempty" json:"url,omitempty"`
	TenantID      string            `yaml:"tenantID,omitempty" json:"tenantID,omitempty"`
	APIKey        string            `yaml:"apiKey,omitempty" json:"apiKey,omitempty"`
	Headers       map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	HeadersEnv    map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile   map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	Auth          *HTTPAuthConfig   `yaml:"auth,omitempty" json:"auth,omitempty"`
	DashboardsDir string            `yaml:"dashboardsDir,omitempty" json:"dashboardsDir,omitempty"`
	Projects      []string          `yaml:"projects,omitempty" json:"projects,omitempty"`
	Timeout       Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

func (c PersesConfig) EffectiveAuth() (HTTPAuthConfig, error) {
	return mergeHTTPAuth(c.APIKey, c.Auth, c.HeadersEnv, c.HeadersFile, "usage.perses.auth")
}

// SLOConfig configures Sloth, Pyrra, and OpenSLO specification YAML scanning (P2.3).
type SLOConfig struct {
	Globs []string `yaml:"globs,omitempty" json:"globs,omitempty"`
}

// RulerConfig configures read-only polling of Mimir/Cortex/Prometheus/Thanos Ruler HTTP APIs.
type RulerConfig struct {
	Type        string            `yaml:"type" json:"type"` // "mimir", "cortex", "prometheus", or "thanos"
	URL         string            `yaml:"url" json:"url"`
	TenantID    string            `yaml:"tenantID,omitempty" json:"tenantID,omitempty"`
	BearerToken string            `yaml:"bearerToken,omitempty" json:"bearerToken,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	HeadersEnv  map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	Auth        *HTTPAuthConfig   `yaml:"auth,omitempty" json:"auth,omitempty"`
	Timeout     Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

func (c RulerConfig) EffectiveAuth() (HTTPAuthConfig, error) {
	return mergeHTTPAuth(c.BearerToken, c.Auth, c.HeadersEnv, c.HeadersFile, "usage.rulers.auth")
}

// K8sConsumersConfig configures multi-document YAML/JSON scanning for Kubernetes autoscalers and rollout templates.
type K8sConsumersConfig struct {
	Globs              []string `yaml:"globs,omitempty" json:"globs,omitempty"`
	EnableKEDA         *bool    `yaml:"enableKEDA,omitempty" json:"enableKEDA,omitempty"`
	EnablePromAdapter  *bool    `yaml:"enablePromAdapter,omitempty" json:"enablePromAdapter,omitempty"`
	EnableArgoRollouts *bool    `yaml:"enableArgoRollouts,omitempty" json:"enableArgoRollouts,omitempty"`
	EnableFlagger      *bool    `yaml:"enableFlagger,omitempty" json:"enableFlagger,omitempty"`
}

func (c *K8sConsumersConfig) KEDAEnabled() bool {
	return c != nil && (c.EnableKEDA == nil || *c.EnableKEDA)
}

func (c *K8sConsumersConfig) PromAdapterEnabled() bool {
	return c != nil && (c.EnablePromAdapter == nil || *c.EnablePromAdapter)
}

func (c *K8sConsumersConfig) ArgoRolloutsEnabled() bool {
	return c != nil && (c.EnableArgoRollouts == nil || *c.EnableArgoRollouts)
}

func (c *K8sConsumersConfig) FlaggerEnabled() bool {
	return c != nil && (c.EnableFlagger == nil || *c.EnableFlagger)
}

type QueryLogSourceConfig struct {
	Glob   string              `yaml:"glob" json:"glob"`
	Format string              `yaml:"format" json:"format"` // "auto" (default), "prometheus", "prom-analytics-proxy"
	Loki   *LokiQueryLogConfig `yaml:"loki" json:"loki"`
}

type LokiQueryLogConfig struct {
	URL              string            `yaml:"url" json:"url"`
	Query            string            `yaml:"query" json:"query"` // LogQL selector e.g. `{job="prometheus", component="query-log"}`
	TenantID         string            `yaml:"tenantID" json:"tenantID"`
	BearerToken      string            `yaml:"bearerToken" json:"bearerToken"`
	Headers          map[string]string `yaml:"headers" json:"headers"`
	HeadersEnv       map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile      map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	Auth             *HTTPAuthConfig   `yaml:"auth,omitempty" json:"auth,omitempty"`
	Window           Duration          `yaml:"window" json:"window"` // Lookback window; defaults to policy.minObservationWindow
	ChunkSize        Duration          `yaml:"chunkSize" json:"chunkSize"`
	Limit            int               `yaml:"limit" json:"limit"`
	MaxPagesPerChunk int               `yaml:"maxPagesPerChunk,omitempty" json:"maxPagesPerChunk,omitempty"`
	Timeout          Duration          `yaml:"timeout" json:"timeout"`
}

func (c LokiQueryLogConfig) EffectiveAuth() (HTTPAuthConfig, error) {
	return mergeHTTPAuth(c.BearerToken, c.Auth, c.HeadersEnv, c.HeadersFile, "usage.queryLogs.loki.auth")
}

type GrafanaConfig struct {
	URL                 string            `yaml:"url" json:"url"`
	TenantID            string            `yaml:"tenantID,omitempty" json:"tenantID,omitempty"`
	APIKey              string            `yaml:"apiKey" json:"apiKey"`
	Headers             map[string]string `yaml:"headers" json:"headers"`
	HeadersEnv          map[string]string `yaml:"headersEnv,omitempty" json:"headersEnv,omitempty"`
	HeadersFile         map[string]string `yaml:"headersFile,omitempty" json:"headersFile,omitempty"`
	Auth                *HTTPAuthConfig   `yaml:"auth,omitempty" json:"auth,omitempty"`
	DashboardsDir       string            `yaml:"dashboardsDir" json:"dashboardsDir"`
	AlertRulesPath      string            `yaml:"alertRulesPath" json:"alertRulesPath"`
	AlertAPI            string            `yaml:"alertAPI,omitempty" json:"alertAPI,omitempty"` // "provisioning" (default) | "ruler"
	EnableManagedAlerts *bool             `yaml:"enableManagedAlerts" json:"enableManagedAlerts"`
	Timeout             Duration          `yaml:"timeout" json:"timeout"`
}

func (g GrafanaConfig) EffectiveAuth() (HTTPAuthConfig, error) {
	return mergeHTTPAuth(g.APIKey, g.Auth, g.HeadersEnv, g.HeadersFile, "usage.grafana.auth")
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
	MinObservationWindow                  Duration        `yaml:"minObservationWindow" json:"minObservationWindow"`
	SoakWindow                            Duration        `yaml:"soakWindow" json:"soakWindow"`
	SoakStartMode                         string          `yaml:"soakStartMode" json:"soakStartMode"` // "observed" (default) | "immediate"
	ConfirmationMaxRemainingPercent       float64         `yaml:"confirmationMaxRemainingPercent" json:"confirmationMaxRemainingPercent"`
	MaxDropPercentPerTranche              float64         `yaml:"maxDropPercentPerTranche" json:"maxDropPercentPerTranche"`
	MaxTotalDroppablePercent              float64         `yaml:"maxTotalDroppablePercent" json:"maxTotalDroppablePercent"`
	BaselineDriftThresholdPercent         float64         `yaml:"baselineDriftThresholdPercent" json:"baselineDriftThresholdPercent"`
	QueryLogBaselineDriftThresholdPercent float64         `yaml:"queryLogBaselineDriftThresholdPercent" json:"queryLogBaselineDriftThresholdPercent"`
	WildcardThresholdPercent              float64         `yaml:"wildcardThresholdPercent" json:"wildcardThresholdPercent"`
	RegretThreshold                       int             `yaml:"regretThreshold" json:"regretThreshold"`
	MinRegretHits                         int             `yaml:"minRegretHits" json:"minRegretHits"`
	NeverDrop                             []string        `yaml:"neverDrop" json:"neverDrop"`
	DisableDefaultNeverDrop               bool            `yaml:"disableDefaultNeverDrop" json:"disableDefaultNeverDrop"`
	ExcludeSources                        []ExcludeSource `yaml:"excludeSources" json:"excludeSources"`
	ExcludeRegretCallers                  []string        `yaml:"excludeRegretCallers" json:"excludeRegretCallers"`
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
	Alloy      *AlloyConfig  `yaml:"alloy,omitempty" json:"alloy,omitempty"`
	PromRW     *PromRWConfig `yaml:"promRW,omitempty" json:"promRW,omitempty"`
}

// AlloyConfig configures Grafana Alloy rule synthesis modes.
type AlloyConfig struct {
	Mode               string `yaml:"mode,omitempty" json:"mode,omitempty"`                             // "module" (default), "relabel_rules", or "configmap"
	ConfigMapName      string `yaml:"configMapName,omitempty" json:"configMapName,omitempty"`           // default "trimtab-alloy-rules"
	ConfigMapNamespace string `yaml:"configMapNamespace,omitempty" json:"configMapNamespace,omitempty"` // optional metadata.namespace
	ConfigMapKey       string `yaml:"configMapKey,omitempty" json:"configMapKey,omitempty"`             // default "trimtab.alloy"
	MaxTermsPerRule    int    `yaml:"maxTermsPerRule,omitempty" json:"maxTermsPerRule,omitempty"`       // default 40
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
			TSDBLimit:           10000,
			NamingMode:          "prometheus_receiver",
		},
		Policy: PolicyConfig{
			MinObservationWindow:                  Duration(30 * 24 * time.Hour),
			SoakWindow:                            Duration(7 * 24 * time.Hour),
			SoakStartMode:                         "observed",
			ConfirmationMaxRemainingPercent:       0.0,
			MaxDropPercentPerTranche:              15.0,
			MaxTotalDroppablePercent:              80.0,
			BaselineDriftThresholdPercent:         20.0,
			QueryLogBaselineDriftThresholdPercent: 50.0,
			WildcardThresholdPercent:              50.0,
			RegretThreshold:                       1,
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
	if c.Inventory.TSDBLimit <= 0 {
		c.Inventory.TSDBLimit = def.Inventory.TSDBLimit
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
	if c.Policy.SoakStartMode == "" {
		c.Policy.SoakStartMode = def.Policy.SoakStartMode
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
	if c.Policy.QueryLogBaselineDriftThresholdPercent <= 0 {
		c.Policy.QueryLogBaselineDriftThresholdPercent = def.Policy.QueryLogBaselineDriftThresholdPercent
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
	if c.Synthesizer.Alloy != nil {
		if c.Synthesizer.Alloy.Mode == "" {
			c.Synthesizer.Alloy.Mode = "module"
		}
		if c.Synthesizer.Alloy.ConfigMapName == "" {
			c.Synthesizer.Alloy.ConfigMapName = "trimtab-alloy-rules"
		}
		if c.Synthesizer.Alloy.ConfigMapKey == "" {
			c.Synthesizer.Alloy.ConfigMapKey = "trimtab.alloy"
		}
		if c.Synthesizer.Alloy.MaxTermsPerRule == 0 {
			c.Synthesizer.Alloy.MaxTermsPerRule = 40
		}
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
			if c.Usage.QueryLogs[i].Loki.MaxPagesPerChunk <= 0 {
				c.Usage.QueryLogs[i].Loki.MaxPagesPerChunk = 100
			}
			if c.Usage.QueryLogs[i].Loki.ChunkSize == 0 {
				c.Usage.QueryLogs[i].Loki.ChunkSize = Duration(24 * time.Hour)
			}
		}
	}
	if c.Usage.Grafana != nil {
		if c.Usage.Grafana.Timeout == 0 {
			c.Usage.Grafana.Timeout = Duration(30 * time.Second)
		}
		if c.Usage.Grafana.AlertAPI == "" {
			c.Usage.Grafana.AlertAPI = "provisioning"
		}
	}
	for i := range c.Usage.Rulers {
		if c.Usage.Rulers[i].Type == "" {
			c.Usage.Rulers[i].Type = "prometheus"
		}
		if c.Usage.Rulers[i].Timeout == 0 {
			c.Usage.Rulers[i].Timeout = Duration(30 * time.Second)
		}
	}
	if c.Usage.Alertmanager != nil {
		if c.Usage.Alertmanager.Timeout == 0 {
			c.Usage.Alertmanager.Timeout = Duration(30 * time.Second)
		}
	}
	if c.Usage.Perses != nil {
		if c.Usage.Perses.Timeout == 0 {
			c.Usage.Perses.Timeout = Duration(30 * time.Second)
		}
	}
}

// Validate checks configuration sanity and regex syntax.
func (c *Config) Validate() error {
	if c.Inventory.PrometheusURL == "" && c.Inventory.FixturePath == "" {
		return errors.New("inventory requires either prometheusURL or fixturePath")
	}
	if c.Inventory.TSDBLimit < 0 {
		return fmt.Errorf("inventory.tsdbLimit must be >= 0, got %d", c.Inventory.TSDBLimit)
	}
	if _, err := c.Inventory.EffectiveAuth(); err != nil {
		return err
	}
	for _, ql := range c.Usage.QueryLogs {
		if ql.Loki != nil {
			if ql.Loki.MaxPagesPerChunk < 0 {
				return fmt.Errorf("usage.queryLogs.loki.maxPagesPerChunk must be >= 0, got %d", ql.Loki.MaxPagesPerChunk)
			}
			if _, err := ql.Loki.EffectiveAuth(); err != nil {
				return err
			}
		}
	}
	if c.Usage.Grafana != nil {
		switch c.Usage.Grafana.AlertAPI {
		case "", "provisioning", "ruler":
		default:
			return fmt.Errorf("unsupported usage.grafana.alertAPI %q (expected \"provisioning\" or \"ruler\")", c.Usage.Grafana.AlertAPI)
		}
		if _, err := c.Usage.Grafana.EffectiveAuth(); err != nil {
			return err
		}
	}
	for i, r := range c.Usage.Rulers {
		if strings.TrimSpace(r.URL) == "" {
			return fmt.Errorf("usage.rulers[%d].url is required", i)
		}
		switch strings.ToLower(strings.TrimSpace(r.Type)) {
		case "", "mimir", "cortex", "prometheus", "thanos":
		default:
			return fmt.Errorf("unsupported usage.rulers[%d].type %q (expected \"mimir\", \"cortex\", \"prometheus\", or \"thanos\")", i, r.Type)
		}
		if _, err := r.EffectiveAuth(); err != nil {
			return err
		}
	}
	if c.Usage.Alertmanager != nil {
		if _, err := c.Usage.Alertmanager.EffectiveAuth(); err != nil {
			return err
		}
	}
	if c.Usage.Perses != nil {
		if _, err := c.Usage.Perses.EffectiveAuth(); err != nil {
			return err
		}
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
	if c.Policy.QueryLogBaselineDriftThresholdPercent <= 0 || c.Policy.QueryLogBaselineDriftThresholdPercent > 100 {
		return fmt.Errorf("policy.queryLogBaselineDriftThresholdPercent must be in (0, 100], got %v", c.Policy.QueryLogBaselineDriftThresholdPercent)
	}
	switch c.Policy.SoakStartMode {
	case "", "observed", "immediate":
	default:
		return fmt.Errorf("unsupported policy.soakStartMode %q (expected \"observed\" or \"immediate\")", c.Policy.SoakStartMode)
	}
	if c.Policy.ConfirmationMaxRemainingPercent < 0 || c.Policy.ConfirmationMaxRemainingPercent > 100 {
		return fmt.Errorf("policy.confirmationMaxRemainingPercent must be in [0, 100], got %v", c.Policy.ConfirmationMaxRemainingPercent)
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
	if al := c.Synthesizer.Alloy; al != nil {
		switch al.Mode {
		case "", "module", "relabel_rules", "configmap":
		default:
			return fmt.Errorf("unsupported synthesizer.alloy.mode %q (expected \"module\", \"relabel_rules\", or \"configmap\")", al.Mode)
		}
		if al.MaxTermsPerRule < 0 {
			return fmt.Errorf("synthesizer.alloy.maxTermsPerRule must be >= 0, got %d", al.MaxTermsPerRule)
		}
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
	resolveAuthPaths(cfg.Inventory.Auth, resolvePath)
	resolvePathMap(cfg.Inventory.HeadersFile, resolvePath)
	for i := range cfg.Usage.QueryLogs {
		cfg.Usage.QueryLogs[i].Glob = resolvePath(cfg.Usage.QueryLogs[i].Glob)
		if cfg.Usage.QueryLogs[i].Loki != nil {
			resolveAuthPaths(cfg.Usage.QueryLogs[i].Loki.Auth, resolvePath)
			resolvePathMap(cfg.Usage.QueryLogs[i].Loki.HeadersFile, resolvePath)
		}
	}
	if cfg.Usage.Grafana != nil {
		cfg.Usage.Grafana.DashboardsDir = resolvePath(cfg.Usage.Grafana.DashboardsDir)
		cfg.Usage.Grafana.AlertRulesPath = resolvePath(cfg.Usage.Grafana.AlertRulesPath)
		resolveAuthPaths(cfg.Usage.Grafana.Auth, resolvePath)
		resolvePathMap(cfg.Usage.Grafana.HeadersFile, resolvePath)
	}
	for i := range cfg.Usage.PromRules {
		cfg.Usage.PromRules[i] = resolvePath(cfg.Usage.PromRules[i])
	}
	for i := range cfg.Usage.Rulers {
		resolveAuthPaths(cfg.Usage.Rulers[i].Auth, resolvePath)
		resolvePathMap(cfg.Usage.Rulers[i].HeadersFile, resolvePath)
	}
	if cfg.Usage.K8sConsumers != nil {
		for i := range cfg.Usage.K8sConsumers.Globs {
			cfg.Usage.K8sConsumers.Globs[i] = resolvePath(cfg.Usage.K8sConsumers.Globs[i])
		}
	}
	if cfg.Usage.Alertmanager != nil {
		for i := range cfg.Usage.Alertmanager.ConfigPaths {
			cfg.Usage.Alertmanager.ConfigPaths[i] = resolvePath(cfg.Usage.Alertmanager.ConfigPaths[i])
		}
		resolveAuthPaths(cfg.Usage.Alertmanager.Auth, resolvePath)
		resolvePathMap(cfg.Usage.Alertmanager.HeadersFile, resolvePath)
	}
	if cfg.Usage.Perses != nil {
		cfg.Usage.Perses.DashboardsDir = resolvePath(cfg.Usage.Perses.DashboardsDir)
		resolveAuthPaths(cfg.Usage.Perses.Auth, resolvePath)
		resolvePathMap(cfg.Usage.Perses.HeadersFile, resolvePath)
	}
	if cfg.Usage.SLO != nil {
		for i := range cfg.Usage.SLO.Globs {
			cfg.Usage.SLO.Globs[i] = resolvePath(cfg.Usage.SLO.Globs[i])
		}
	}
	cfg.Synthesizer.OutputPath = resolvePath(cfg.Synthesizer.OutputPath)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
