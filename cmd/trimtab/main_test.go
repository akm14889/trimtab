package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	demoPromRWCfg := filepath.Join("..", "..", "examples", "demo", "trimtab-promrw.yaml")
	tmpDir := t.TempDir()
	nowFlag := "2026-11-21T12:00:00Z"

	t.Run("version", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run([]string{"version"}, &out, &errBuf)
		if code != 0 || !strings.Contains(out.String(), "trimtab ") {
			t.Fatalf("version failed: code=%d out=%q err=%q", code, out.String(), errBuf.String())
		}
	})

	t.Run("analyze demo Alloy target is observation-only", func(t *testing.T) {
		stateFile := filepath.Join(tmpDir, "state-alloy.json")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--state", stateFile,
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("analyze failed: code=%d stderr=%s", code, errBuf.String())
		}
		reportTxt := out.String()
		for _, want := range []string{
			"TRIMTAB AUDIT REPORT — prod-metrics",
			"Query Log Coverage: 30/30 days",
			"Active Series: 391,000",
			"Droppable Series: 184,200 (47.1%)",
			"NEXT ELIGIBLE TRANCHE (Tranche 1/4",
			"checkout_legacy_cart_gauge",
			"orders_deprecated_latency_seconds",
			"orders:deprecated_latency:p99",
			"orders:deprecated_latency:rate5m",
			"0.0% (no collapse)",
			"-88.0%",
			"Cardinality Explorer",
			"REGRET CHECK (forward-only): 0 regrets",
			"trimtab synth --activate",
		} {
			if !strings.Contains(reportTxt, want) {
				t.Errorf("report missing %q:\n%s", want, reportTxt)
			}
		}

		// Observation-only analyze must NOT populate ActiveDrops or Tranches.
		ledger, err := state.Load(stateFile)
		if err != nil {
			t.Fatalf("load persisted state: %v", err)
		}
		if len(ledger.ActiveDrops) != 0 || len(ledger.Tranches) != 0 {
			t.Fatalf("observation-only analyze must not activate drops/tranches, got active=%d tranches=%d", len(ledger.ActiveDrops), len(ledger.Tranches))
		}
		if ledger.QueryLogCoverage.CoveredDays != 30 {
			t.Fatalf("expected 30 covered days in ledger, got %v", ledger.QueryLogCoverage.CoveredDays)
		}
	})

	t.Run("analyze demo JSON format", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--format", "json",
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
		if rep.Summary.TotalActiveSeries != 391000 || rep.Summary.DroppableSeries != 184200 || rep.Summary.SynthesizedSeries != 0 {
			t.Fatalf("unexpected JSON summary: %+v", rep.Summary)
		}
		if !rep.Safety.NextTrancheReady || len(rep.Tranches) < 4 {
			t.Fatalf("expected NextTrancheReady=true and >=4 planned tranches, got ready=%v tranches=%d", rep.Safety.NextTrancheReady, len(rep.Tranches))
		}
	})

	t.Run("synth --preview-next Alloy target without mutating ledger", func(t *testing.T) {
		alloyOut := filepath.Join(tmpDir, "preview.alloy")
		stateFile := filepath.Join(tmpDir, "state-preview.json")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"synth",
			"--config", demoCfg,
			"--preview-next",
			"--out", alloyOut,
			"--state", stateFile,
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("synth --preview-next failed: code=%d stderr=%s", code, errBuf.String())
		}
		alloyBytes, err := os.ReadFile(alloyOut)
		if err != nil {
			t.Fatalf("read generated alloy file: %v", err)
		}
		alloyStr := string(alloyBytes)
		if !strings.Contains(alloyStr, "forward_to = argument.forward_to.value") ||
			!strings.Contains(alloyStr, "checkout_legacy_cart_gauge") ||
			!strings.Contains(alloyStr, "orders_deprecated_latency_seconds_(bucket|sum|count|created)") {
			t.Fatalf("generated preview alloy missing expected Tranche 1 drops:\n%s", alloyStr)
		}
		ledger, err := state.Load(stateFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger.ActiveDrops) != 0 || len(ledger.Tranches) != 0 {
			t.Fatalf("synth --preview-next must not mutate ActiveDrops or Tranches, got active=%d tranches=%d", len(ledger.ActiveDrops), len(ledger.Tranches))
		}
	})

	t.Run("synth --activate, plain synth, and --confirm-tranche lifecycle", func(t *testing.T) {
		alloyOut := filepath.Join(tmpDir, "active.alloy")
		stateFile := filepath.Join(tmpDir, "state-activate.json")

		// 1. Plain synth before activation emits pass-through (0 drops).
		var out0, err0 bytes.Buffer
		if code := run([]string{"synth", "--config", demoCfg, "--out", alloyOut, "--state", stateFile, "--now", nowFlag}, &out0, &err0); code != 0 {
			t.Fatalf("plain synth failed: code=%d err=%s", code, err0.String())
		}
		if !strings.Contains(out0.String(), "0 families, 0 series") {
			t.Fatalf("expected 0 drops before --activate, got: %s", out0.String())
		}

		// 2. synth --activate enters pending_confirmation and writes Tranche 1 drops.
		var out1, err1 bytes.Buffer
		if code := run([]string{"synth", "--config", demoCfg, "--activate", "--out", alloyOut, "--state", stateFile, "--now", nowFlag}, &out1, &err1); code != 0 {
			t.Fatalf("synth --activate failed: code=%d err=%s", code, err1.String())
		}
		if !strings.Contains(out1.String(), "2 families, 57868 series") {
			t.Fatalf("expected 2 families / 57868 series on --activate, got: %s", out1.String())
		}
		ledger, err := state.Load(stateFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger.ActiveDrops) != 2 || ledger.LatestTranche() == nil || ledger.LatestTranche().Status != "pending_confirmation" {
			t.Fatalf("expected 2 ActiveDrops and pending_confirmation tranche, got active=%d latest=%+v", len(ledger.ActiveDrops), ledger.LatestTranche())
		}

		// 3. --confirm-tranche transitions pending_confirmation -> soaking.
		var out2, err2 bytes.Buffer
		if code := run([]string{"analyze", "--config", demoCfg, "--confirm-tranche", "--state", stateFile, "--now", nowFlag}, &out2, &err2); code != 0 {
			t.Fatalf("analyze --confirm-tranche failed: code=%d err=%s", code, err2.String())
		}
		ledger2, err := state.Load(stateFile)
		if err != nil {
			t.Fatal(err)
		}
		if ledger2.LatestTranche().Status != "soaking" {
			t.Fatalf("expected soaking after --confirm-tranche, got %q", ledger2.LatestTranche().Status)
		}

		// 4. --activate and --preview-next are mutually exclusive.
		var outBad, errBad bytes.Buffer
		if code := run([]string{"synth", "--config", demoCfg, "--activate", "--preview-next"}, &outBad, &errBad); code != 2 {
			t.Fatalf("expected exit code 2 for --activate --preview-next, got %d", code)
		}
	})

	t.Run("synth demo OTel Collector target", func(t *testing.T) {
		otelOut := filepath.Join(tmpDir, "trimtab-rules.yaml")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"synth",
			"--config", demoOTelCfg,
			"--activate",
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

	t.Run("regret fires on observation-only analyze when api-user queries dropped family", func(t *testing.T) {
		// Seed ledger with `http_requests_total` as if previously dropped 2 days ago;
		// demo query logs contain api-user queries to `http_requests_total` on 2026-11-21!
		regretState := filepath.Join(tmpDir, "state-regret.json")
		l := state.NewLedger()
		l.RecordTrancheActivation(1, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), 7*24*time.Hour, true, []metrics.Recommendation{
			{FamilyName: "http_requests_total", Action: metrics.ActionDropFamily, Series: 74200},
		})
		if err := l.Save(regretState); err != nil {
			t.Fatal(err)
		}

		var out, errBuf bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--state", regretState,
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != exitCodeRegret {
			t.Fatalf("regret run expected exit code %d, got code=%d err=%s", exitCodeRegret, code, errBuf.String())
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
		if len(updated.ActiveDrops) != 0 {
			t.Fatalf("observation-only analyze after regret must not activate new drops, got %d", len(updated.ActiveDrops))
		}

		// Resolve regret once reverted rules are applied; exits 0 and clears blocked_regret while keeping pin.
		var outRes, errRes bytes.Buffer
		codeRes := run([]string{
			"analyze",
			"--config", demoCfg,
			"--state", regretState,
			"--resolve-regret",
			"--now", nowFlag,
		}, &outRes, &errRes)
		if codeRes != 0 {
			t.Fatalf("--resolve-regret failed: code=%d err=%s", codeRes, errRes.String())
		}
		resolved, err := state.Load(regretState)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.LatestTranche().Status == "blocked_regret" || resolved.LatestTranche().UnresolvedRegrets != 0 {
			t.Fatalf("expected regret cleared, got tranche=%+v", resolved.LatestTranche())
		}
		if !resolved.IsPinnedNeverDrop("http_requests_total") {
			t.Fatal("http_requests_total must stay pinned to neverDrop after resolution")
		}
	})

	t.Run("synth demo Prometheus remote_write target", func(t *testing.T) {
		promRWOut := filepath.Join(tmpDir, "trimtab-remote-write.yaml")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"synth",
			"--config", demoPromRWCfg,
			"--activate",
			"--out", promRWOut,
			"--state", filepath.Join(tmpDir, "state-promrw.json"),
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("synth promrw failed: code=%d stderr=%s", code, errBuf.String())
		}
		raw, err := os.ReadFile(promRWOut)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			WriteRelabelConfigs []struct {
				SourceLabels []string `yaml:"source_labels"`
				Regex        string   `yaml:"regex"`
				Action       string   `yaml:"action"`
			} `yaml:"write_relabel_configs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("invalid promrw YAML: %v\n%s", err, string(raw))
		}
		if len(doc.WriteRelabelConfigs) != 1 {
			t.Fatalf("expected 1 write_relabel_configs rule, got %d", len(doc.WriteRelabelConfigs))
		}
		rx := doc.WriteRelabelConfigs[0].Regex
		if !strings.Contains(rx, "checkout_legacy_cart_gauge") ||
			!strings.Contains(rx, "orders_deprecated_latency_seconds_(bucket|sum|count|created)") {
			t.Fatalf("promrw output missing expected Tranche 1 drops: %q", rx)
		}
	})

	t.Run("synth demo with --target promrw --preview-next override", func(t *testing.T) {
		promRWOut := filepath.Join(tmpDir, "override-remote-write.yaml")
		var out, errBuf bytes.Buffer
		code := run([]string{
			"synth",
			"--config", demoCfg,
			"--target", "promrw",
			"--preview-next",
			"--out", promRWOut,
			"--no-state-write",
			"--state", filepath.Join(tmpDir, "missing-promrw.json"),
			"--now", nowFlag,
		}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("synth --target promrw failed: code=%d stderr=%s", code, errBuf.String())
		}
		if !strings.Contains(out.String(), "Synthesized promrw artifact ->") {
			t.Fatalf("expected promrw synth output message:\n%s", out.String())
		}
	})

	t.Run("doctor offline demo passes", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run([]string{"doctor", "--config", demoCfg}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("doctor offline failed: code=%d out=%s err=%s", code, out.String(), errBuf.String())
		}
		if !strings.Contains(out.String(), "Doctor check PASSED") {
			t.Fatalf("expected Doctor check PASSED:\n%s", out.String())
		}
	})

	t.Run("doctor HTTP pass and 401 fail-closed with secret redaction", func(t *testing.T) {
		promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Scope-OrgID") != "prod-tenant" || r.Header.Get("Authorization") != "Bearer prom-secret-val" {
				http.Error(w, "denied prom-secret-val", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"storageRetention":"30d"}}`))
		}))
		defer promSrv.Close()

		lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Scope-OrgID") != "prod-tenant" || r.Header.Get("X-API-Key") != "loki-secret-val" {
				http.Error(w, "denied loki-secret-val", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
		}))
		defer lokiSrv.Close()

		grafSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer graf-secret-val" {
				http.Error(w, "denied graf-secret-val", http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/api/search":
				_, _ = w.Write([]byte(`[]`))
			case "/api/v1/provisioning/alert-rules":
				_, _ = w.Write([]byte(`[]`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer grafSrv.Close()

		t.Setenv("DOC_PROM_TOK", "prom-secret-val")
		t.Setenv("DOC_LOKI_KEY", "loki-secret-val")
		t.Setenv("DOC_GRAF_TOK", "graf-secret-val")

		cfgFile := filepath.Join(tmpDir, "doctor-http.yaml")
		cfgYAML := `
name: doctor-test
inventory:
  prometheusURL: ` + promSrv.URL + `
  tenantID: prod-tenant
  auth:
    bearerTokenEnv: DOC_PROM_TOK
usage:
  queryLogs:
    - loki:
        url: ` + lokiSrv.URL + `
        tenantID: prod-tenant
        query: '{job="prom"}'
        headersEnv:
          X-API-Key: DOC_LOKI_KEY
  grafana:
    url: ` + grafSrv.URL + `
    auth:
      bearerTokenEnv: DOC_GRAF_TOK
`
		if err := os.WriteFile(cfgFile, []byte(cfgYAML), 0o600); err != nil {
			t.Fatal(err)
		}

		var outOK, errOK bytes.Buffer
		if code := run([]string{"doctor", "--config", cfgFile, "--format", "json"}, &outOK, &errOK); code != 0 {
			t.Fatalf("doctor HTTP pass failed: code=%d out=%s err=%s", code, outOK.String(), errOK.String())
		}
		var docRep doctorReport
		if err := json.Unmarshal(outOK.Bytes(), &docRep); err != nil {
			t.Fatalf("decode doctor json: %v", err)
		}
		if !docRep.Passed || len(docRep.Checks) != 3 {
			t.Fatalf("unexpected doctor report: %+v", docRep)
		}

		// Force 401 failure and verify secret is redacted from output
		t.Setenv("DOC_PROM_TOK", "wrong-secret-val")
		badProm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "invalid token wrong-secret-val", http.StatusUnauthorized)
		}))
		defer badProm.Close()

		badCfg := filepath.Join(tmpDir, "doctor-bad.yaml")
		badYAML := strings.ReplaceAll(cfgYAML, promSrv.URL, badProm.URL)
		if err := os.WriteFile(badCfg, []byte(badYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		var outFail, errFail bytes.Buffer
		if code := run([]string{"doctor", "--config", badCfg}, &outFail, &errFail); code != 1 {
			t.Fatalf("expected exit code 1 on 401 doctor failure, got %d", code)
		}
		if strings.Contains(outFail.String(), "wrong-secret-val") {
			t.Fatalf("secret leaked in doctor output:\n%s", outFail.String())
		}
		if !strings.Contains(outFail.String(), "[REDACTED]") || !strings.Contains(outFail.String(), "Doctor check FAILED") {
			t.Fatalf("expected redacted FAIL output:\n%s", outFail.String())
		}
	})

	t.Run("doctor verifies ruler HTTP endpoints and k8s consumer globs", func(t *testing.T) {
		invSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"storageRetention":"30d"}}`))
		}))
		defer invSrv.Close()

		mimirRulerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/prometheus/config/v1/rules" || r.Header.Get("X-Scope-OrgID") != "prod-tenant" || r.Header.Get("Authorization") != "Bearer ruler-tok" {
				http.Error(w, "denied ruler-tok", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte("{}\n"))
		}))
		defer mimirRulerSrv.Close()

		k8sDir := filepath.Join(tmpDir, "k8s-consumers-doc")
		if err := os.MkdirAll(k8sDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(k8sDir, "keda.yaml"), []byte("apiVersion: keda.sh/v1alpha1\nkind: ScaledObject\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DOC_RULER_TOK", "ruler-tok")

		cfgFile := filepath.Join(tmpDir, "doctor-p22.yaml")
		cfgYAML := `
name: doctor-p22
inventory:
  prometheusURL: ` + invSrv.URL + `
usage:
  rulers:
    - type: mimir
      url: ` + mimirRulerSrv.URL + `
      tenantID: prod-tenant
      auth:
        bearerTokenEnv: DOC_RULER_TOK
  k8sConsumers:
    globs:
      - ` + filepath.Join(k8sDir, "*.yaml") + `
`
		if err := os.WriteFile(cfgFile, []byte(cfgYAML), 0o600); err != nil {
			t.Fatal(err)
		}

		var outOK, errOK bytes.Buffer
		if code := run([]string{"doctor", "--config", cfgFile, "--format", "json"}, &outOK, &errOK); code != 0 {
			t.Fatalf("doctor p22 failed: code=%d out=%s err=%s", code, outOK.String(), errOK.String())
		}
		var rep doctorReport
		if err := json.Unmarshal(outOK.Bytes(), &rep); err != nil {
			t.Fatalf("decode doctor json: %v", err)
		}
		if !rep.Passed || len(rep.Checks) != 3 {
			t.Fatalf("unexpected p22 doctor report: %+v", rep)
		}

		// 403 on ruler fails closed and redacts secret; missing k8s glob fails closed
		badCfg := filepath.Join(tmpDir, "doctor-p22-bad.yaml")
		badYAML := strings.ReplaceAll(cfgYAML, "prod-tenant", "wrong-tenant")
		badYAML = strings.ReplaceAll(badYAML, "*.yaml", "nonexistent-*.yaml")
		if err := os.WriteFile(badCfg, []byte(badYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		var outFail, errFail bytes.Buffer
		if code := run([]string{"doctor", "--config", badCfg}, &outFail, &errFail); code != 1 {
			t.Fatalf("expected exit code 1 on p22 doctor failures, got %d", code)
		}
		if strings.Contains(outFail.String(), "ruler-tok") {
			t.Fatalf("ruler secret leaked in doctor output:\n%s", outFail.String())
		}
		if !strings.Contains(outFail.String(), "[REDACTED]") || !strings.Contains(outFail.String(), "glob matched 0 files") {
			t.Fatalf("expected redacted ruler FAIL + missing glob FAIL:\n%s", outFail.String())
		}
	})

	t.Run("doctor verifies alertmanager, perses, and slo sources", func(t *testing.T) {
		invSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"storageRetention":"30d"}}`))
		}))
		defer invSrv.Close()

		amSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v2/status" || r.Header.Get("Authorization") != "Bearer am-doc-tok" {
				http.Error(w, "denied am-doc-tok", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"config":{"original":"route:\n  receiver: ops\n"}}`))
		}))
		defer amSrv.Close()

		persesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/projects" || r.Header.Get("Authorization") != "Bearer perses-doc-tok" {
				http.Error(w, "denied "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}))
		defer persesSrv.Close()

		amDir := filepath.Join(tmpDir, "am-doc")
		persesDir := filepath.Join(tmpDir, "perses-doc")
		sloDir := filepath.Join(tmpDir, "slo-doc")
		for _, d := range []string{amDir, persesDir, sloDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(amDir, "am.yaml"), []byte("route:\n  receiver: ops\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sloDir, "slo.yaml"), []byte("apiVersion: openslo/v1\nkind: SLO\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		t.Setenv("DOC_AM_TOK", "am-doc-tok")
		t.Setenv("DOC_PERSES_TOK", "perses-doc-tok")

		cfgFile := filepath.Join(tmpDir, "doctor-p23.yaml")
		cfgYAML := `
name: doctor-p23
inventory:
  prometheusURL: ` + invSrv.URL + `
usage:
  alertmanager:
    url: ` + amSrv.URL + `
    configPaths:
      - ` + filepath.Join(amDir, "*.yaml") + `
    auth:
      bearerTokenEnv: DOC_AM_TOK
  perses:
    url: ` + persesSrv.URL + `
    dashboardsDir: ` + persesDir + `
    auth:
      bearerTokenEnv: DOC_PERSES_TOK
  slo:
    globs:
      - ` + filepath.Join(sloDir, "*.yaml") + `
`
		if err := os.WriteFile(cfgFile, []byte(cfgYAML), 0o600); err != nil {
			t.Fatal(err)
		}

		var outOK, errOK bytes.Buffer
		if code := run([]string{"doctor", "--config", cfgFile, "--format", "json"}, &outOK, &errOK); code != 0 {
			t.Fatalf("doctor p23 failed: code=%d out=%s err=%s", code, outOK.String(), errOK.String())
		}
		var rep doctorReport
		if err := json.Unmarshal(outOK.Bytes(), &rep); err != nil {
			t.Fatalf("decode doctor json: %v", err)
		}
		if !rep.Passed || len(rep.Checks) != 6 {
			t.Fatalf("unexpected p23 doctor report: %+v", rep)
		}

		// Force failure on wrong token + missing SLO glob
		t.Setenv("DOC_PERSES_TOK", "bad-perses-secret")
		badCfg := filepath.Join(tmpDir, "doctor-p23-bad.yaml")
		badYAML := strings.ReplaceAll(cfgYAML, filepath.Join(sloDir, "*.yaml"), filepath.Join(sloDir, "missing-*.yaml"))
		if err := os.WriteFile(badCfg, []byte(badYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		var outFail, errFail bytes.Buffer
		if code := run([]string{"doctor", "--config", badCfg}, &outFail, &errFail); code != 1 {
			t.Fatalf("expected exit code 1 on p23 doctor failure, got %d", code)
		}
		if strings.Contains(outFail.String(), "bad-perses-secret") {
			t.Fatalf("perses secret leaked in doctor output:\n%s", outFail.String())
		}
		if !strings.Contains(outFail.String(), "[REDACTED]") || !strings.Contains(outFail.String(), "glob matched 0 files") {
			t.Fatalf("expected redacted Perses FAIL + missing SLO glob FAIL:\n%s", outFail.String())
		}
	})

	t.Run("analyze --format html --out --metrics-out and --pushgateway-url (including on regret)", func(t *testing.T) {
		var pushedPath, pushedBody string
		pushSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				http.Error(w, "want PUT", http.StatusMethodNotAllowed)
				return
			}
			pushedPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			pushedBody = string(b)
			w.WriteHeader(http.StatusOK)
		}))
		defer pushSrv.Close()

		htmlOut := filepath.Join(tmpDir, "audit.html")
		metricsOut := filepath.Join(tmpDir, "trimtab.prom")

		// Normal run writes HTML + Prometheus textfile + pushes to Pushgateway
		var outOK, errOK bytes.Buffer
		code := run([]string{
			"analyze",
			"--config", demoCfg,
			"--format", "html",
			"--out", htmlOut,
			"--metrics-out", metricsOut,
			"--pushgateway-url", pushSrv.URL,
			"--pushgateway-job", "trimtab-ci",
			"--no-state-write",
			"--state", filepath.Join(tmpDir, "missing-html.json"),
			"--now", nowFlag,
		}, &outOK, &errOK)
		if code != 0 {
			t.Fatalf("analyze html/metrics failed: code=%d stderr=%s", code, errOK.String())
		}
		if !strings.Contains(outOK.String(), "Wrote html report -> "+htmlOut) {
			t.Fatalf("unexpected stdout: %s", outOK.String())
		}
		htmlBytes, err := os.ReadFile(htmlOut)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(htmlBytes), "<!DOCTYPE html>") || !strings.Contains(string(htmlBytes), "checkout_legacy_cart_gauge") {
			t.Fatalf("unexpected HTML file output:\n%s", string(htmlBytes))
		}
		promBytes, err := os.ReadFile(metricsOut)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(promBytes), `trimtab_active_series{name="prod-metrics",tenant="default"} 391000`) {
			t.Fatalf("unexpected Prometheus metrics output:\n%s", string(promBytes))
		}
		if pushedPath != "/metrics/job/trimtab-ci/instance/prod-metrics" || pushedBody != string(promBytes) {
			t.Fatalf("unexpected Pushgateway push: path=%q len=%d", pushedPath, len(pushedBody))
		}

		// Regret run MUST emit metrics BEFORE exiting with code 3
		regState := filepath.Join(tmpDir, "state-regret-metrics.json")
		l := state.NewLedger()
		l.RecordTrancheActivation(1, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), 7*24*time.Hour, true, []metrics.Recommendation{
			{FamilyName: "http_requests_total", Action: metrics.ActionDropFamily, Series: 74200},
		})
		if err := l.Save(regState); err != nil {
			t.Fatal(err)
		}
		regMetricsOut := filepath.Join(tmpDir, "regret.prom")
		var outReg, errReg bytes.Buffer
		regCode := run([]string{
			"analyze",
			"--config", demoCfg,
			"--format", "html",
			"--out", filepath.Join(tmpDir, "regret.html"),
			"--metrics-out", regMetricsOut,
			"--pushgateway-url", pushSrv.URL,
			"--state", regState,
			"--now", nowFlag,
		}, &outReg, &errReg)
		if regCode != exitCodeRegret {
			t.Fatalf("expected exitCodeRegret=%d, got %d (stderr=%s)", exitCodeRegret, regCode, errReg.String())
		}
		regProm, err := os.ReadFile(regMetricsOut)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(regProm), `trimtab_regret_events_total{name="prod-metrics",tenant="default"} 1`) {
			t.Fatalf("expected regret counter emitted on exit 3:\n%s", string(regProm))
		}
	})
}
