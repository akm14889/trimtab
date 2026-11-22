# Trimtab — Session Checkpoint & Progress Tracker

> **Purpose:** Living handoff document so any resumed or new session can immediately see what is built, verified, and what to build next without scanning the whole repo.
> **Read alongside:** [AGENT.md](../AGENT.md) | [docs/DESIGN.md](DESIGN.md) | [README.md](../README.md)

---

## Current Status

- **Completed Phases:** **Phase 1 (Metrics CLI MVP + Grafana Alloy)** AND **Phase 1.1 (OpenTelemetry Collector Synthesizer + Loki Query-Log Source)**
- **Overall State:** Verified & Green — ready to dogfood locally or start **Phase 2**
- **Last Updated:** 2026-11-22
- **Go Toolchain:** Go `1.27.3` (`module trimtab.io/trimtab`)
- **Verification Command:** `go test -race ./... && go build ./cmd/trimtab`
- **Demo Commands:** `make demo` (Alloy) | `make demo-otel` (OTel Collector) | `make demo-markdown`

---

## Milestone Checklist

- [x] **M0 — Project Bootstrap:** `go.mod` (`trimtab.io/trimtab`), `.gitignore`, `Makefile`, `docs/CHECKPOINT.md`, verified Prometheus `v0.316.0` `promql/parser.NewParser` API.
- [x] **M1 — Core Foundations:** `pkg/config` (custom `Duration` with `d`/`w`, defaults, validation), `pkg/metrics` (family types, `LabelUsage` union-of-without math, report/capability structs), `pkg/state` (atomic `.trimtab-state.json` ledger).
- [x] **M2 — Discovery & Usage Sources:** `pkg/usage/promql` (official PromQL AST walker), `pkg/inventory/prometheus` (HTTP API + offline fixture loader + honest `without` collapse evaluator), `pkg/usage/querylog` (glob/gz JSONL, `prom-analytics-proxy`, origin classifier, + Phase 1.1 Loki `/loki/api/v1/query_range` reader), `pkg/usage/grafana` (HTTP API + local JSON dir: PromQL, `legendFormat`, links, overrides, transforms, template vars, managed alerts), `pkg/usage/promrules` (alerting/recording YAML, Go templates, `or vector(0)` detection).
- [x] **M3 — Core Engine:** `pkg/engine` (`family.go`, `dag.go`, `safety.go`, `regret.go`, `analyzer.go`) + 10/10 Mandatory Safety Test Matrix + Capability Negotiation + Wildcard & Soak Progression tests.
- [x] **M4 — Capability-Aware Synthesizers:** `pkg/synthesizer`, `pkg/synthesizer/alloy` (`trimtab.alloy` with `argument.forward_to.value` + golden tests), `pkg/synthesizer/otel` (`trimtab-rules.yaml` without `service:`, deterministic OTLP names only, `__trimtab_noop__` + golden tests).
- [x] **M5 — Reports, CLI, Demo & Docs:** `pkg/report` (`table`, `markdown`, `json`), `cmd/trimtab` (`analyze`, `synth`, `version` + end-to-end tests), `examples/demo/` (391k-series offline demo for Alloy & OTel + human-owned insertion-point examples), `README.md`, updated `AGENT.md`, `docs/DESIGN.md`, `docs/CHECKPOINT.md`.

---

## Mandatory Safety Test Matrix Status (10/10 Green)

All 10 invariants live in [pkg/engine/safety_matrix_test.go](../pkg/engine/safety_matrix_test.go):

1. [x] `TestSafetyMatrix_01_MissingOrShortQueryLogReportOnly` — Missing query log OR `query_log_coverage < minObservationWindow` ⇒ `0` synthesized drops (Report-Only with projected savings).
2. [x] `TestSafetyMatrix_02_RuleEvalDoesNotMarkLive` — `rule-eval` entries do NOT mark inputs live-used; `api-user` and alerting rules DO.
3. [x] `TestSafetyMatrix_03_TranchesAndSanityCeiling` — 47% droppable estate split into tranches `<= 15%` sorted descending by series without splitting histograms; `>80%` triggers sanity-abort.
4. [x] `TestSafetyMatrix_04_BaselineDriftGuard` — Grafana `100 -> 20` partial response aborts drop synthesis without overwriting baseline; `--accept-baseline-shift` unblocks and updates baseline.
5. [x] `TestSafetyMatrix_05_ObservationWindowProtection` — Metric family observed `< minObservationWindow` protected from drop.
6. [x] `TestSafetyMatrix_06_DefaultNeverDropProtected` — `up`, `ALERTS`, `ALERTS_FOR_STATE`, `scrape_*`, `prometheus_*`, `alloy_*`, `otelcol_*` always protected.
7. [x] `TestSafetyMatrix_07_RecordingRuleDAGDeadAndLiveChains` — Dead chain (`M -> R1 -> R2`) drops `M` at collector + emits `R1`/`R2` advisories (flagging `or vector(0)`); live chain protects all.
8. [x] `TestSafetyMatrix_08_09_LabelSemanticsAndHonestWithoutCollapse` — `sum without (pod)`, bare `rate(x[5m])`, `label_replace`, Grafana `legendFormat`, alert `{{ $labels.x }}` mark labels used.
9. [x] `TestSafetyMatrix_08_09_LabelSemanticsAndHonestWithoutCollapse` — `le`, `quantile`, `__name__` never suggested; single-label `without (pod_uid)` honestly shows `0.0%` collapse when `pod` remains.
10. [x] `TestSafetyMatrix_10_RegretDetectionAndTrancheBlocking` — 1 `api-user` hit on `active_drops` emits `RegretEvent`, un-drops & pins metric to `neverDrop`, blocks next tranche.
- [x] Bonus: `TestCapabilityNegotiationAndNativeHistogram`, `TestWildcardProtectorAndSoakProgression`, plus Alloy & OTel Golden Tests (`pkg/synthesizer/alloy/alloy_test.go`, `pkg/synthesizer/otel/otel_test.go`) and CLI E2E Tests (`cmd/trimtab/main_test.go`).

---

## Implemented Package Map

| Package / Path | Status | Key Tests |
| :--- | :--- | :--- |
| `cmd/trimtab/` | Done | `main_test.go` (Alloy, OTel, JSON, Regret E2E) |
| `pkg/config/` | Done | `config_test.go` |
| `pkg/metrics/` | Done | `types_test.go` |
| `pkg/state/` | Done | `ledger_test.go` |
| `pkg/inventory/prometheus/` | Done | `client_test.go` (HTTP + short retention warn + fixture `without`) |
| `pkg/usage/promql/` | Done | `extractor_test.go` |
| `pkg/usage/querylog/` | Done | `reader_test.go` (glob, `prom-analytics-proxy`, Loki) |
| `pkg/usage/grafana/` | Done | `scanner_test.go` (HTTP + local dir + managed alerts) |
| `pkg/usage/promrules/` | Done | `scanner_test.go` (alerts, recording rules, Go templates, `or vector(0)`) |
| `pkg/engine/` | Done | `safety_matrix_test.go` (10/10 invariants + capabilities + soak) |
| `pkg/synthesizer/alloy/` | Done | `alloy_test.go` + 3 golden files |
| `pkg/synthesizer/otel/` | Done | `otel_test.go` + 2 golden files |
| `pkg/report/` | Done | `report_test.go` (`table`, `markdown`, `json`) |
| `examples/demo/` | Done | Offline 391k-series demo + Alloy & OTel configs |

---

## Architecture & Implementation Notes for Future Sessions

- **PromQL Parser API (Prometheus v0.316.0):** `parser.ParseExpr` no longer exists; always use `parser.NewParser(parser.Options{EnableExperimentalFunctions: true, EnableBinopFillModifiers: true, EnableUnquotedUTF8Names: true}).ParseExpr(...)` (already centralized in `pkg/usage/promql`).
- **Deterministic Clock:** CLI accepts `--now <RFC3339>` and `engine.AnalyzeInput.Now` accepts a `time.Time` so soak windows, observation windows, and CI/demo tests are 100% deterministic.
- **Minimal Dependencies:** Only 2 direct third-party dependencies (`github.com/prometheus/prometheus`, `gopkg.in/yaml.v3`) plus Go stdlib.
- **Scope Guardrails Honored:** No K8s Operator/CRDs, no OpenCost FinOps exporter, no GitOps PR actuator, no direct Alloy parser, and no Logs/Traces/OpAMP were added in Phase 1.

---

## How to Resume in a New Session (Phase 2 Handoff)

1. Read [AGENT.md](../AGENT.md) and this file (`docs/CHECKPOINT.md`).
2. Confirm green baseline:
   ```bash
   go test -race ./... && go build ./cmd/trimtab
   make demo && make demo-otel
   ```
3. **Recommended Phase 2 Execution Order** (update this checkpoint after each item):
   - **P2.1 — Prometheus `remote_write` Synthesizer (`pkg/synthesizer/promrw/`):** Synthesize `remote_write[].write_relabel_configs` YAML overlay with golden tests.
   - **P2.2 — Expanded Static Usage Sources (`pkg/usage/`):** Mimir/Thanos Ruler API, KEDA `ScaledObject`, `prometheus-adapter`, Argo Rollouts / Flagger `AnalysisTemplate`, Alertmanager configs, Perses dashboards, Sloth/Pyrra SLOs (updating `BaselineCounts` in `pkg/state` for each new source).
   - **P2.3 — Per-Team Attribution & OpenCost-Style FinOps Exporter (`pkg/attribution/`, `pkg/exporter/`):** Label/namespace/Backstage team resolver + `trimtab_team_*` Prometheus `/metrics` exporter + `TelemetryBudget` evaluation.
   - **P2.4 — GitOps PR Actuator (`pkg/actuator/gitops/`):** GitHub/GitLab PR creation for tranched drops, dead recording-rule cleanup PRs, and **automated Regret Revert PRs**.
   - **P2.5 — Kubernetes Operator & CRDs (`api/v1alpha1/`, `internal/controller/`):** `TelemetryAuditPolicy`, `TelemetryBudget`, `TelemetryRecommendation` (with `breakGlassUntil`).
   - **P2.6 — Multi-Cluster / Per-Selector Drop Scoping.**
