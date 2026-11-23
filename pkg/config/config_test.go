package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"15m", 15 * time.Minute},
		{"0.5d", 12 * time.Hour},
	}
	for _, tc := range tests {
		got, err := ParseDuration(tc.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q) unexpected error: %v", tc.in, err)
		}
		if got.Duration() != tc.want {
			t.Fatalf("ParseDuration(%q) = %v, want %v", tc.in, got.Duration(), tc.want)
		}
	}
	if _, err := ParseDuration("bogus"); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoadAndDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "trimtab.yaml")
	yamlContent := `
name: prod-metrics
inventory:
  fixturePath: inventory.json
usage:
  queryLogs:
    - glob: "logs/*.jsonl"
  promRules:
    - "rules/*.yaml"
policy:
  minObservationWindow: 30d
  soakWindow: 7d
  neverDrop:
    - "^billing_.*"
  excludeSources:
    - type: dashboard_title
      value: "Cardinality Explorer"
synthesizer:
  target: alloy
  outputPath: out/trimtab.alloy
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Policy.MaxDropPercentPerTranche != 15.0 {
		t.Errorf("MaxDropPercentPerTranche = %v, want 15.0", cfg.Policy.MaxDropPercentPerTranche)
	}
	if cfg.Policy.MaxTotalDroppablePercent != 80.0 {
		t.Errorf("MaxTotalDroppablePercent = %v, want 80.0", cfg.Policy.MaxTotalDroppablePercent)
	}
	if cfg.Policy.EffectiveRegretThreshold() != 1 {
		t.Errorf("EffectiveRegretThreshold = %d, want 1", cfg.Policy.EffectiveRegretThreshold())
	}
	if cfg.Inventory.FixturePath != filepath.Join(dir, "inventory.json") {
		t.Errorf("FixturePath not resolved relative to config dir: %s", cfg.Inventory.FixturePath)
	}
	patterns := cfg.Policy.EffectiveNeverDropPatterns()
	if len(patterns) != len(DefaultNeverDropPatterns())+1 {
		t.Errorf("Expected default + user neverDrop patterns, got %v", patterns)
	}
}

func TestValidateRejectsInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when neither prometheusURL nor fixturePath is set")
	}
	cfg.Inventory.FixturePath = "inv.json"
	cfg.Policy.NeverDrop = []string{"[invalid-regex"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid regex in neverDrop")
	}
}

func TestPromRWConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "trimtab-promrw.yaml")
	yamlContent := `
name: promrw-test
inventory:
  fixturePath: inv.json
synthesizer:
  target: promrw
  outputPath: out/trimtab-remote-write.yaml
  promRW:
    mode: prometheus_operator_patch
    remoteWriteName: prod-mimir
    remoteWriteURL: https://mimir.example.com/api/v1/push
    operatorNamespace: monitoring
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Synthesizer.PromRW == nil {
		t.Fatal("expected PromRW config to be populated")
	}
	if cfg.Synthesizer.PromRW.OperatorKind != "Prometheus" || cfg.Synthesizer.PromRW.OperatorName != "prometheus" {
		t.Fatalf("unexpected PromRW operator defaults: %+v", cfg.Synthesizer.PromRW)
	}
	if cfg.Synthesizer.PromRW.MaxTermsPerRule != 40 {
		t.Fatalf("expected MaxTermsPerRule=40 default, got %d", cfg.Synthesizer.PromRW.MaxTermsPerRule)
	}

	// Alias target supported
	cfg.Synthesizer.Target = "prometheus-remote-write"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected alias prometheus-remote-write to validate, got %v", err)
	}

	// Invalid mode
	bad := DefaultConfig()
	bad.Inventory.FixturePath = "inv.json"
	bad.Synthesizer.Target = "promrw"
	bad.Synthesizer.PromRW = &PromRWConfig{Mode: "bogus"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for bogus promRW.mode")
	}

	// remote_write without url or name
	bad.Synthesizer.PromRW = &PromRWConfig{Mode: "remote_write"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for remote_write without url or name")
	}

	// prometheus_operator_patch without url
	bad.Synthesizer.PromRW = &PromRWConfig{Mode: "prometheus_operator_patch", RemoteWriteName: "only-name"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for prometheus_operator_patch without remoteWriteURL")
	}

	// invalid operatorKind
	bad.Synthesizer.PromRW = &PromRWConfig{
		Mode:           "prometheus_operator_patch",
		RemoteWriteURL: "https://example.com/push",
		OperatorKind:   "Deployment",
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for invalid operatorKind")
	}

	// negative maxTermsPerRule
	bad.Synthesizer.PromRW = &PromRWConfig{Mode: "relabel_configs", MaxTermsPerRule: -1}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for negative maxTermsPerRule")
	}
}

func TestHardeningPolicyDefaultsAndValidation(t *testing.T) {
	def := DefaultConfig()
	if def.Policy.SoakStartMode != "observed" {
		t.Errorf("default SoakStartMode = %q, want observed", def.Policy.SoakStartMode)
	}
	if def.Policy.QueryLogBaselineDriftThresholdPercent != 50.0 {
		t.Errorf("default QueryLogBaselineDriftThresholdPercent = %v, want 50.0", def.Policy.QueryLogBaselineDriftThresholdPercent)
	}
	if def.Policy.ConfirmationMaxRemainingPercent != 0.0 {
		t.Errorf("default ConfirmationMaxRemainingPercent = %v, want 0.0", def.Policy.ConfirmationMaxRemainingPercent)
	}

	// Invalid soakStartMode
	bad := DefaultConfig()
	bad.Inventory.FixturePath = "inv.json"
	bad.Policy.SoakStartMode = "never"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for invalid soakStartMode")
	}

	// Invalid queryLogBaselineDriftThresholdPercent
	bad = DefaultConfig()
	bad.Inventory.FixturePath = "inv.json"
	bad.Policy.QueryLogBaselineDriftThresholdPercent = 150
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for queryLogBaselineDriftThresholdPercent > 100")
	}

	// Invalid confirmationMaxRemainingPercent
	bad = DefaultConfig()
	bad.Inventory.FixturePath = "inv.json"
	bad.Policy.ConfirmationMaxRemainingPercent = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for negative confirmationMaxRemainingPercent")
	}

	// Valid immediate mode
	good := DefaultConfig()
	good.Inventory.FixturePath = "inv.json"
	good.Policy.SoakStartMode = "immediate"
	good.Policy.ConfirmationMaxRemainingPercent = 5.0
	if err := good.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestUnifiedAuthAndAlloyConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "trimtab.yaml")
	yamlContent := `
name: auth-test
inventory:
  prometheusURL: https://prom.internal
  tenantID: team-a
  selector: 'cluster="prod"'
  auth:
    basicAuth:
      usernameEnv: PROM_USER
      passwordFile: secrets/prom-pass
    tls:
      caFile: certs/ca.crt
      certFile: certs/cli.crt
      keyFile: certs/cli.key
usage:
  queryLogs:
    - loki:
        url: https://loki.internal
        query: '{app="prom"}'
        headersFile:
          X-API-Key: secrets/loki-key
  grafana:
    url: https://grafana.internal
    alertAPI: ruler
    auth:
      bearerTokenFile: secrets/graf-tok
synthesizer:
  target: alloy
  alloy:
    mode: configmap
    configMapNamespace: monitoring
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Inventory.TSDBLimit != 10000 {
		t.Errorf("TSDBLimit = %d, want 10000", cfg.Inventory.TSDBLimit)
	}
	if cfg.Inventory.Auth.BasicAuth.PasswordFile != filepath.Join(dir, "secrets", "prom-pass") {
		t.Errorf("PasswordFile not resolved: %s", cfg.Inventory.Auth.BasicAuth.PasswordFile)
	}
	if cfg.Inventory.Auth.TLS.CAFile != filepath.Join(dir, "certs", "ca.crt") {
		t.Errorf("CAFile not resolved: %s", cfg.Inventory.Auth.TLS.CAFile)
	}
	if cfg.Usage.QueryLogs[0].Loki.MaxPagesPerChunk != 100 {
		t.Errorf("MaxPagesPerChunk = %d, want 100", cfg.Usage.QueryLogs[0].Loki.MaxPagesPerChunk)
	}
	if cfg.Usage.QueryLogs[0].Loki.HeadersFile["X-API-Key"] != filepath.Join(dir, "secrets", "loki-key") {
		t.Errorf("Loki HeadersFile not resolved: %+v", cfg.Usage.QueryLogs[0].Loki.HeadersFile)
	}
	if cfg.Usage.Grafana.Auth.BearerTokenFile != filepath.Join(dir, "secrets", "graf-tok") {
		t.Errorf("Grafana BearerTokenFile not resolved: %s", cfg.Usage.Grafana.Auth.BearerTokenFile)
	}
	if cfg.Synthesizer.Alloy == nil || cfg.Synthesizer.Alloy.Mode != "configmap" || cfg.Synthesizer.Alloy.ConfigMapName != "trimtab-alloy-rules" {
		t.Fatalf("unexpected Alloy defaults: %+v", cfg.Synthesizer.Alloy)
	}

	// Conflicting basicAuth + bearerToken fails closed
	bad := DefaultConfig()
	bad.Inventory.PrometheusURL = "https://prom.internal"
	bad.Inventory.BearerToken = "legacy"
	bad.Inventory.Auth = &HTTPAuthConfig{
		BasicAuth: &BasicAuthConfig{Username: "u", Password: "p"},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for conflicting legacy bearerToken + auth.basicAuth")
	}

	// Partial mTLS (cert without key) fails closed
	badTLS := DefaultConfig()
	badTLS.Inventory.PrometheusURL = "https://prom.internal"
	badTLS.Inventory.Auth = &HTTPAuthConfig{
		TLS: &TLSConfig{CertFile: "cli.crt"},
	}
	if err := badTLS.Validate(); err == nil {
		t.Fatal("expected error for certFile without keyFile")
	}

	// Invalid alloy mode
	badAlloy := DefaultConfig()
	badAlloy.Inventory.FixturePath = "inv.json"
	badAlloy.Synthesizer.Alloy = &AlloyConfig{Mode: "bogus"}
	if err := badAlloy.Validate(); err == nil {
		t.Fatal("expected error for unsupported synthesizer.alloy.mode")
	}
}

func TestRulerAndK8sConsumersConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "trimtab.yaml")
	yamlContent := `
inventory:
  fixturePath: inv.json
usage:
  rulers:
    - type: mimir
      url: https://mimir.internal
      tenantID: prod
      auth:
        bearerTokenFile: secrets/ruler-tok
      headersFile:
        X-Ruler: secrets/ruler-hdr
    - url: https://prom-ruler.internal
  k8sConsumers:
    globs:
      - k8s/*.yaml
      - adapters/
    enableKEDA: true
    enablePromAdapter: false
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(cfg.Usage.Rulers) != 2 {
		t.Fatalf("expected 2 rulers, got %d", len(cfg.Usage.Rulers))
	}
	if cfg.Usage.Rulers[0].Auth.BearerTokenFile != filepath.Join(dir, "secrets", "ruler-tok") {
		t.Errorf("ruler bearerTokenFile not resolved: %s", cfg.Usage.Rulers[0].Auth.BearerTokenFile)
	}
	if cfg.Usage.Rulers[0].HeadersFile["X-Ruler"] != filepath.Join(dir, "secrets", "ruler-hdr") {
		t.Errorf("ruler headersFile not resolved: %+v", cfg.Usage.Rulers[0].HeadersFile)
	}
	if cfg.Usage.Rulers[1].Type != "prometheus" {
		t.Errorf("expected default ruler type=prometheus, got %q", cfg.Usage.Rulers[1].Type)
	}
	if cfg.Usage.K8sConsumers == nil || len(cfg.Usage.K8sConsumers.Globs) != 2 {
		t.Fatalf("unexpected k8sConsumers: %+v", cfg.Usage.K8sConsumers)
	}
	if cfg.Usage.K8sConsumers.Globs[0] != filepath.Join(dir, "k8s", "*.yaml") {
		t.Errorf("k8sConsumers glob not resolved: %s", cfg.Usage.K8sConsumers.Globs[0])
	}
	if !cfg.Usage.K8sConsumers.KEDAEnabled() || cfg.Usage.K8sConsumers.PromAdapterEnabled() || !cfg.Usage.K8sConsumers.ArgoRolloutsEnabled() || !cfg.Usage.K8sConsumers.FlaggerEnabled() {
		t.Errorf("unexpected k8sConsumers toggle evaluation: %+v", cfg.Usage.K8sConsumers)
	}

	// Unsupported ruler type fails closed
	badRuler := DefaultConfig()
	badRuler.Inventory.FixturePath = "inv.json"
	badRuler.Usage.Rulers = []RulerConfig{{Type: "unknown", URL: "https://r.internal"}}
	if err := badRuler.Validate(); err == nil {
		t.Fatal("expected error for unsupported usage.rulers[].type")
	}

	// Missing ruler URL fails closed
	noURL := DefaultConfig()
	noURL.Inventory.FixturePath = "inv.json"
	noURL.Usage.Rulers = []RulerConfig{{Type: "mimir"}}
	if err := noURL.Validate(); err == nil {
		t.Fatal("expected error for empty usage.rulers[].url")
	}
}

func TestP23EcosystemUsageConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "trimtab.yaml")
	yamlContent := `
inventory:
  fixturePath: inv.json
usage:
  alertmanager:
    configPaths:
      - am/*.yaml
    url: https://am.internal
    auth:
      bearerTokenFile: secrets/am-tok
    headersFile:
      X-AM: secrets/am-hdr
  perses:
    url: https://perses.internal
    dashboardsDir: perses-dashboards
    projects: [payments, core]
    auth:
      bearerTokenFile: secrets/perses-tok
    headersFile:
      X-Perses: secrets/perses-hdr
  slo:
    globs:
      - slos/*.yaml
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Usage.Alertmanager == nil || cfg.Usage.Perses == nil || cfg.Usage.SLO == nil {
		t.Fatalf("expected P2.3 usage blocks populated: %+v", cfg.Usage)
	}
	if cfg.Usage.Alertmanager.ConfigPaths[0] != filepath.Join(dir, "am", "*.yaml") {
		t.Errorf("Alertmanager ConfigPaths[0] not resolved: %s", cfg.Usage.Alertmanager.ConfigPaths[0])
	}
	if cfg.Usage.Alertmanager.Auth.BearerTokenFile != filepath.Join(dir, "secrets", "am-tok") {
		t.Errorf("Alertmanager BearerTokenFile not resolved: %s", cfg.Usage.Alertmanager.Auth.BearerTokenFile)
	}
	if cfg.Usage.Alertmanager.HeadersFile["X-AM"] != filepath.Join(dir, "secrets", "am-hdr") {
		t.Errorf("Alertmanager HeadersFile not resolved: %+v", cfg.Usage.Alertmanager.HeadersFile)
	}
	if cfg.Usage.Alertmanager.Timeout.Duration() == 0 {
		t.Errorf("expected default Alertmanager timeout")
	}
	if cfg.Usage.Perses.DashboardsDir != filepath.Join(dir, "perses-dashboards") {
		t.Errorf("Perses DashboardsDir not resolved: %s", cfg.Usage.Perses.DashboardsDir)
	}
	if cfg.Usage.Perses.Auth.BearerTokenFile != filepath.Join(dir, "secrets", "perses-tok") {
		t.Errorf("Perses BearerTokenFile not resolved: %s", cfg.Usage.Perses.Auth.BearerTokenFile)
	}
	if cfg.Usage.Perses.Timeout.Duration() == 0 {
		t.Errorf("expected default Perses timeout")
	}
	if cfg.Usage.SLO.Globs[0] != filepath.Join(dir, "slos", "*.yaml") {
		t.Errorf("SLO Globs[0] not resolved: %s", cfg.Usage.SLO.Globs[0])
	}

	badAM := DefaultConfig()
	badAM.Inventory.FixturePath = "inv.json"
	badAM.Usage.Alertmanager = &AlertmanagerConfig{
		BearerToken: "legacy",
		Auth:        &HTTPAuthConfig{BasicAuth: &BasicAuthConfig{Username: "u", Password: "p"}},
	}
	if err := badAM.Validate(); err == nil {
		t.Fatal("expected error for conflicting alertmanager auth modes")
	}
}
