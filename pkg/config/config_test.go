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
