package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
)

func TestCLI_DemoAnalyzeAndSynthEndToEnd(t *testing.T) {
	demoCfg := filepath.Join("..", "..", "examples", "demo", "trimtab.yaml")
	demoOTelCfg := filepath.Join("..", "..", "examples", "demo", "trimtab-otel.yaml")
	tmpDir := t.TempDir()
	nowFlag := "2026-11-21T12:00:00Z"

	t.Run("version", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run([]string{"version"}, &out, &errBuf)
		if code != 0 || !strings.Contains(out.String(), "trimtab ") {
			t.Fatalf("version failed: code=%d out=%q err=%q", code, out.String(), errBuf.String())
		}
	})

	t.Run("analyze demo Alloy target", func(t *testing.T) {
		alloyOut := filepath.Join(tmpDir, "trimtab.alloy")
		stateFile := filepath.Join(tmpDir, "state-alloy.json")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--out", alloyOut,
			"--state", stateFile,
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("analyze failed: code=%d stderr=%s", code, errBuf.String())
		}
		reportTxt := out.String()
		for _, want := range []string{
			"TRIMTAB AUDIT REPORT — prod-metrics",
			"Active Series: 391,000",
			"Droppable Series: 184,200 (47.1%)",
			"Active Tranche:  1/4",
			"57,868 (14.8%)",
			"checkout_legacy_cart_gauge",
			"orders_deprecated_latency_seconds",
			"orders:deprecated_latency:p99",
			"orders:deprecated_latency:rate5m",
			"0.0% (no collapse)",
			"-88.0%",
			"Cardinality Explorer",
			"REGRET CHECK (forward-only): 0 regrets",
		} {
			if !strings.Contains(reportTxt, want) {
				t.Errorf("report missing %q:\n%s", want, reportTxt)
			}
		}

		// Verify generated Alloy module contract
		alloyBytes, err := os.ReadFile(alloyOut)
		if err != nil {
			t.Fatalf("read generated alloy file: %v", err)
		}
		alloyStr := string(alloyBytes)
		if !strings.Contains(alloyStr, "forward_to = argument.forward_to.value") {
			t.Fatalf("generated alloy missing argument.forward_to.value:\n%s", alloyStr)
		}
		if !strings.Contains(alloyStr, "checkout_legacy_cart_gauge") ||
			!strings.Contains(alloyStr, "orders_deprecated_latency_seconds_(bucket|sum|count|created)") {
			t.Fatalf("generated alloy missing expected Tranche 1 drops:\n%s", alloyStr)
		}

		// Verify state ledger persisted active_drops & baseline counts
		ledger, err := state.Load(stateFile)
		if err != nil {
			t.Fatalf("load persisted state: %v", err)
		}
		if len(ledger.ActiveDrops) != 2 {
			t.Fatalf("expected 2 ActiveDrops in persisted ledger, got %d", len(ledger.ActiveDrops))
		}
	})

	t.Run("analyze demo JSON format", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--format", "json",
			"--out", filepath.Join(tmpDir, "json-run.alloy"),
			"--no-state-write",
			"--state", filepath.Join(tmpDir, "missing.json"),
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("analyze json failed: code=%d err=%s", code, errBuf.String())
		}
		var rep metrics.AuditReport
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("decode json output: %v", err)
		}
		if rep.Summary.TotalActiveSeries != 391000 || rep.Summary.SynthesizedSeries != 57868 {
			t.Fatalf("unexpected JSON summary: %+v", rep.Summary)
		}
	})

	t.Run("synth demo OTel Collector target", func(t *testing.T) {
		otelOut := filepath.Join(tmpDir, "trimtab-rules.yaml")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"synth",
			"--config", demoOTelCfg,
			"--out", otelOut,
			"--state", filepath.Join(tmpDir, "state-otel.json"),
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("synth otel failed: code=%d stderr=%s", code, errBuf.String())
		}
		raw, err := os.ReadFile(otelOut)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("invalid OTel YAML: %v", err)
		}
		if _, hasService := doc["service"]; hasService {
			t.Fatal("OTel overlay MUST NEVER contain service: block")
		}
		s := string(raw)
		if !strings.Contains(s, `'name == "checkout_legacy_cart_gauge"'`) ||
			!strings.Contains(s, `'name == "orders.deprecated.latency"'`) {
			t.Fatalf("OTel overlay missing expected deterministic drops:\n%s", s)
		}
	})

	t.Run("regret fires on subsequent run when api-user queries dropped family", func(t *testing.T) {
		// Seed ledger with `http_requests_total` as if previously dropped 2 days ago;
		// demo query logs contain api-user queries to `http_requests_total` on 2026-11-21!
		regretState := filepath.Join(tmpDir, "state-regret.json")
		l := state.NewLedger()
		l.RecordTrancheActivation(1, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), 7*24*time.Hour, []metrics.Recommendation{
			{FamilyName: "http_requests_total", Action: metrics.ActionDropFamily, Series: 74200},
		})
		if err := l.Save(regretState); err != nil {
			t.Fatal(err)
		}

		var out, errBuf bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--out", filepath.Join(tmpDir, "regret.alloy"),
			"--state", regretState,
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("regret run failed: code=%d err=%s", code, errBuf.String())
		}
		if !strings.Contains(out.String(), "1 REGRET(S) DETECTED") || !strings.Contains(out.String(), "http_requests_total") {
			t.Fatalf("expected regret event for http_requests_total in report:\n%s", out.String())
		}
		updated, err := state.Load(regretState)
		if err != nil {
			t.Fatal(err)
		}
		if !updated.IsPinnedNeverDrop("http_requests_total") {
			t.Fatal("http_requests_total should be pinned to neverDrop after regret")
		}
	})
}
