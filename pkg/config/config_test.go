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
