package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func helmChartDir() string {
	return filepath.Join("..", "..", "charts", "trimtab")
}

func TestHelmChartMetadataAndSchema(t *testing.T) {
	chartDir := helmChartDir()

	chartRaw, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var chartMeta struct {
		APIVersion string `yaml:"apiVersion"`
		Name       string `yaml:"name"`
		Type       string `yaml:"type"`
		Version    string `yaml:"version"`
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(chartRaw, &chartMeta); err != nil {
		t.Fatalf("parse Chart.yaml: %v", err)
	}
	if chartMeta.APIVersion != "v2" {
		t.Fatalf("expected Chart apiVersion v2, got %q", chartMeta.APIVersion)
	}
	if chartMeta.Name != "trimtab" || chartMeta.Type != "application" {
		t.Fatalf("unexpected Chart metadata name=%q type=%q", chartMeta.Name, chartMeta.Type)
	}
	if chartMeta.Version == "" || chartMeta.AppVersion == "" {
		t.Fatalf("expected non-empty Chart version and appVersion, got %q / %q", chartMeta.Version, chartMeta.AppVersion)
	}

	schemaRaw, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatalf("read values.schema.json: %v", err)
	}
	if !json.Valid(schemaRaw) {
		t.Fatalf("values.schema.json is not valid JSON")
	}
	var schemaMap map[string]any
	if err := json.Unmarshal(schemaRaw, &schemaMap); err != nil {
		t.Fatalf("unmarshal values.schema.json: %v", err)
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok || props["config"] == nil {
		t.Fatalf("expected properties.config in values.schema.json")
	}

	requiredTemplates := []string{
		"templates/_helpers.tpl",
		"templates/NOTES.txt",
		"templates/configmap.yaml",
		"templates/cronjob.yaml",
		"templates/grafana-dashboard-configmap.yaml",
		"templates/pvc.yaml",
		"templates/serviceaccount.yaml",
		"templates/tests/test-doctor.yaml",
	}
	for _, rel := range requiredTemplates {
		content, err := os.ReadFile(filepath.Join(chartDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(content)
		if open, close := strings.Count(text, "{{"), strings.Count(text, "}}"); open == 0 || open != close {
			t.Fatalf("%s has unbalanced template delimiters: {{=%d }}=%d", rel, open, close)
		}
	}

	if _, err := os.Stat(filepath.Join(chartDir, "README.md")); err != nil {
		t.Fatalf("stat chart README.md: %v", err)
	}
}

func TestHelmChartSafetyGuards(t *testing.T) {
	chartDir := helmChartDir()

	cronjobRaw, err := os.ReadFile(filepath.Join(chartDir, "templates", "cronjob.yaml"))
	if err != nil {
		t.Fatalf("read cronjob.yaml: %v", err)
	}
	cronjobText := string(cronjobRaw)
	if !strings.Contains(cronjobText, "- analyze") {
		t.Fatalf("expected cronjob.yaml to execute `analyze` subcommand")
	}
	if strings.Contains(cronjobText, "--activate") {
		t.Fatalf("cronjob.yaml must never auto-enable --activate")
	}

	doctorRaw, err := os.ReadFile(filepath.Join(chartDir, "templates", "tests", "test-doctor.yaml"))
	if err != nil {
		t.Fatalf("read test-doctor.yaml: %v", err)
	}
	if !strings.Contains(string(doctorRaw), "- doctor") {
		t.Fatalf("expected test-doctor.yaml to execute `doctor` subcommand")
	}

	values := readYAMLMap(t, filepath.Join(chartDir, "values.yaml"))
	sa, _ := values["serviceAccount"].(map[string]any)
	if automount, ok := sa["automount"].(bool); !ok || automount {
		t.Fatalf("expected serviceAccount.automount=false by default, got %v", sa["automount"])
	}
	podSec, _ := values["podSecurityContext"].(map[string]any)
	if runAsNonRoot, ok := podSec["runAsNonRoot"].(bool); !ok || !runAsNonRoot {
		t.Fatalf("expected podSecurityContext.runAsNonRoot=true, got %v", podSec["runAsNonRoot"])
	}
	contSec, _ := values["securityContext"].(map[string]any)
	if roRoot, ok := contSec["readOnlyRootFilesystem"].(bool); !ok || !roRoot {
		t.Fatalf("expected securityContext.readOnlyRootFilesystem=true, got %v", contSec["readOnlyRootFilesystem"])
	}
}

func TestHelmValuesValidateConfig(t *testing.T) {
	chartDir := helmChartDir()
	baseValues := readYAMLMap(t, filepath.Join(chartDir, "values.yaml"))

	cases := []struct {
		name         string
		overrideFile string
		assertFn     func(t *testing.T, cfg *Config)
	}{
		{
			name: "default",
			assertFn: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Inventory.ProbeFirstSeen {
					t.Fatalf("expected default probeFirstSeen=false")
				}
				if got := cfg.Policy.MinObservationWindow.Duration(); got != 30*24*time.Hour {
					t.Fatalf("expected default minObservationWindow=30d, got %s", got)
				}
				if got := cfg.Policy.SoakWindow.Duration(); got != 7*24*time.Hour {
					t.Fatalf("expected default soakWindow=7d, got %s", got)
				}
				if cfg.Synthesizer.Target != "alloy" {
					t.Fatalf("expected target=alloy, got %q", cfg.Synthesizer.Target)
				}
			},
		},
		{
			name:         "values-nonprod",
			overrideFile: filepath.Join(chartDir, "examples", "values-nonprod.yaml"),
			assertFn: func(t *testing.T, cfg *Config) {
				t.Helper()
				if !cfg.Inventory.ProbeFirstSeen {
					t.Fatalf("expected nonprod probeFirstSeen=true")
				}
				if got := cfg.Policy.MinObservationWindow.Duration(); got != 24*time.Hour {
					t.Fatalf("expected nonprod minObservationWindow=24h, got %s", got)
				}
				if got := cfg.Policy.SoakWindow.Duration(); got != 15*time.Minute {
					t.Fatalf("expected nonprod soakWindow=15m, got %s", got)
				}
				if cfg.Policy.SoakStartMode != "observed" {
					t.Fatalf("expected soakStartMode=observed, got %q", cfg.Policy.SoakStartMode)
				}
				if cfg.Synthesizer.Target != "alloy" {
					t.Fatalf("expected target=alloy, got %q", cfg.Synthesizer.Target)
				}
			},
		},
		{
			name:         "values-prod",
			overrideFile: filepath.Join(chartDir, "examples", "values-prod.yaml"),
			assertFn: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Inventory.ProbeFirstSeen {
					t.Fatalf("expected prod probeFirstSeen=false")
				}
				if got := cfg.Policy.MinObservationWindow.Duration(); got != 30*24*time.Hour {
					t.Fatalf("expected prod minObservationWindow=30d, got %s", got)
				}
				if got := cfg.Policy.SoakWindow.Duration(); got != 7*24*time.Hour {
					t.Fatalf("expected prod soakWindow=7d, got %s", got)
				}
				if cfg.Inventory.Auth == nil || cfg.Inventory.Auth.BearerTokenEnv == "" {
					t.Fatalf("expected prod prometheus bearerTokenEnv auth configured")
				}
			},
		},
		{
			name:         "values-otel",
			overrideFile: filepath.Join(chartDir, "examples", "values-otel.yaml"),
			assertFn: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Synthesizer.Target != "otel" {
					t.Fatalf("expected target=otel, got %q", cfg.Synthesizer.Target)
				}
				if cfg.Synthesizer.Alloy != nil {
					t.Fatalf("expected alloy block cleared for otel profile")
				}
			},
		},
		{
			name:         "values-promrw",
			overrideFile: filepath.Join(chartDir, "examples", "values-promrw.yaml"),
			assertFn: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Synthesizer.Target != "promrw" {
					t.Fatalf("expected target=promrw, got %q", cfg.Synthesizer.Target)
				}
				if cfg.Synthesizer.PromRW == nil || cfg.Synthesizer.PromRW.Mode != "relabel_configs" {
					t.Fatalf("expected promRW.mode=relabel_configs, got %+v", cfg.Synthesizer.PromRW)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := cloneMap(baseValues)
			if tc.overrideFile != "" {
				override := readYAMLMap(t, tc.overrideFile)
				merged = deepMergeMaps(merged, override)
			}

			cfgBlock, ok := merged["config"]
			if !ok {
				t.Fatalf("merged values missing top-level `config` key")
			}
			cfgBytes, err := yaml.Marshal(cfgBlock)
			if err != nil {
				t.Fatalf("marshal merged config block: %v", err)
			}

			var cfg Config
			dec := yaml.NewDecoder(bytes.NewReader(cfgBytes))
			dec.KnownFields(true)
			if err := dec.Decode(&cfg); err != nil {
				t.Fatalf("strict decode Config failed: %v\n---\n%s", err, string(cfgBytes))
			}
			cfg.ApplyDefaults()
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Config.Validate() failed: %v\n---\n%s", err, string(cfgBytes))
			}
			tc.assertFn(t, &cfg)
		})
	}
}

func readYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if m, ok := v.(map[string]any); ok {
			dst[k] = cloneMap(m)
		} else {
			dst[k] = v
		}
	}
	return dst
}

func deepMergeMaps(dst, src map[string]any) map[string]any {
	out := cloneMap(dst)
	for k, v := range src {
		if v == nil {
			delete(out, k)
			continue
		}
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, exists := out[k].(map[string]any); exists {
				out[k] = deepMergeMaps(dstMap, srcMap)
				continue
			}
			out[k] = cloneMap(srcMap)
			continue
		}
		out[k] = v
	}
	return out
}
