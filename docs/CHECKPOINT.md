# Trimtab — Session Checkpoint & Progress Tracker

> **Purpose:** Compact living handoff & progress tracker so any resumed or new session sees what is built, verified, and next.
> **Single Plan Rule:** All detailed phase/sub-phase plans live in [docs/PLAN.md](PLAN.md) — **never** create separate `PHASE2_PLAN.md` / `PHASE3_PLAN.md` files; append/refine specs in `docs/PLAN.md` and keep this checkpoint updated.
> **Read alongside:** [AGENT.md](../AGENT.md) | [docs/DESIGN.md](DESIGN.md) | [docs/BACKENDS_AND_AUTH.md](BACKENDS_AND_AUTH.md) | [docs/PLAN.md](PLAN.md) | [../agent_work.md](../agent_work.md) | [README.md](../README.md)

---

## Current Status

- **Completed Phases:** **Phase 1 (Metrics CLI MVP + Grafana Alloy)**, **Phase 1.1 (OpenTelemetry Collector Synthesizer + Loki Query-Log Source)**, **Phase 2.1 (Prometheus `remote_write` Synthesizer)**, AND **Phase 2.1a (GitHub PR CI, Release Pipeline & Toolchain Hygiene)**
- **Overall State:** Verified & Green — ready to execute **Phase 2.1b (Safety-Critical Hardening)**, followed by **P2.1c** and **P2.2** (see [docs/PLAN.md](PLAN.md))
- **Last Updated:** 2026-11-23
- **Go Toolchain:** `go 1.27` in `go.mod` (`module trimtab.io/trimtab`, built/verified with Go `1.27.3`)
- **Verification Command:** `go test -race ./... && go build ./cmd/trimtab`
- **Demo Commands:** `make demo` (Alloy) | `make demo-otel` (OTel Collector) | `make demo-promrw` (Prometheus `remote_write`) | `make demo-markdown`

---

## Milestone Checklist

- [x] **M0 — Project Bootstrap:** `go.mod` (`trimtab.io/trimtab`), `.gitignore`, `Makefile`, `docs/CHECKPOINT.md`, verified Prometheus `v0.316.0` `promql/parser.NewParser` API.
- [x] **M1 — Core Foundations:** `pkg/config` (custom `Duration` with `d`/`w`, defaults, validation), `pkg/metrics` (family types, `LabelUsage` union-of-without math, report/capability structs), `pkg/state` (atomic `.trimtab-state.json` ledger).
- [x] **M2 — Discovery & Usage Sources:** `pkg/usage/promql` (official PromQL AST walker), `pkg/inventory/prometheus` (HTTP API + offline fixture loader + honest `without` collapse evaluator), `pkg/usage/querylog` (glob/gz JSONL, `prom-analytics-proxy`, origin classifier, + Phase 1.1 Loki `/loki/api/v1/query_range` reader), `pkg/usage/grafana` (HTTP API + local JSON dir: PromQL, `legendFormat`, links, overrides, transforms, template vars, managed alerts), `pkg/usage/promrules` (alerting/recording YAML, Go templates, `or vector(0)` detection).
- [x] **M3 — Core Engine:** `pkg/engine` (`family.go`, `dag.go`, `safety.go`, `regret.go`, `analyzer.go`) + 10/10 Mandatory Safety Test Matrix + Capability Negotiation + Wildcard & Soak Progression tests.
- [x] **M4 — Capability-Aware Synthesizers:** `pkg/synthesizer`, `pkg/synthesizer/alloy` (`trimtab.alloy` with `argument.forward_to.value` + golden tests), `pkg/synthesizer/otel` (`trimtab-rules.yaml` without `service:`, deterministic OTLP names only, `__trimtab_noop__` + golden tests).
- [x] **M5 — Reports, CLI, Demo & Docs:** `pkg/report` (`table`, `markdown`, `json`), `cmd/trimtab` (`analyze`, `synth`, `version` + end-to-end tests), `examples/demo/` (391k-series offline demo for Alloy & OTel + human-owned insertion-point examples), `README.md`, updated `AGENT.md`, `docs/DESIGN.md`, `docs/CHECKPOINT.md`.

### Phase 2 Milestone Checklist (See [docs/PLAN.md](PLAN.md))

- [x] **P2.1 — Prometheus `remote_write` Synthesizer (`pkg/synthesizer/promrw/`):** Shared relabel helper (`pkg/synthesizer/relabel.go`), `relabel_configs` / `remote_write` / `prometheus_operator_patch` modes, real `prometheus/model/relabel` validation + execution tests, golden tests, CLI/demo wiring (`make demo-promrw`).
- [x] **P2.1a — GitHub PR CI, Release Pipeline & Toolchain Hygiene:** `go 1.27` in `go.mod`, PR CI (`.github/workflows/ci.yml` + `.golangci.yml` + `make lint` running `go test -race`, `golangci-lint`, golden tests, `make demo*`, Docker build check, and rolling `main`/`sha-<short>` GHCR dev images), automated `release-please` version tagging & `CHANGELOG.md` (`.github/workflows/release-please.yml`, `release-please-config.json`, `.release-please-manifest.json`), and release pipeline (`.github/workflows/release.yml` via `workflow_call` + `v*.*.*` tags, `.goreleaser.yaml` with `mode: keep-existing`, multi-arch `Dockerfile` with `VERSION`/`COMMIT` ldflags injection publishing static binaries, checksums, SPDX SBOM, GHCR images, and build provenance attestations).
- [ ] **P2.1b — Safety-Critical Hardening:** Log-rotation `LastQueriedAt`/`LastBucketQueriedAt` survival + trailing-window `CoveredDates` bitmap, observation-only `analyze` + explicit `synth --activate` + confirmed soak (`pending_confirmation -> soaking`), per-expression query-log wildcard detection, PromQL scalar binary label precision, HWM baselines + daily query-log rate, Safety Matrix #11–#13.
- [ ] **P2.1c — Unified Backend Auth (`pkg/httpclient`), Live-Scale Hardening & `trimtab doctor`:** Lean stdlib `HTTPAuthConfig` (No Auth, Basic Auth, Bearer, Custom Header Auth `*Env`/`*File`, `X-Scope-OrgID`, TLS/mTLS, secret redaction, `User-Agent: trimtab/` self-query auto-exclusion), `/status/tsdb?limit` + `selector`, Loki forward pagination + `param_query` + streaming dedup, Grafana non-Prometheus datasource filter + `/api/search` pagination, Alloy `module|relabel_rules|configmap` modes, `deploy/k8s/` CronJob, `trimtab doctor`.
- [ ] **P2.2 — Critical Infrastructure & Ruler Usage Sources:** Mimir/Thanos Ruler APIs + KEDA, `prometheus-adapter`, Argo Rollouts, Flagger scanners.
- [ ] **P2.3 — Observability Ecosystem Static Consumers:** Alertmanager label protection, Perses dashboards, Sloth/Pyrra SLOs.
- [ ] **P2.4 — Per-Team Attribution & OpenCost-Style FinOps Exporter:** Team resolver chain, `TelemetryBudget` evaluation, `trimtab serve` `/metrics` exporter.
- [ ] **P2.5 — GitOps PR Actuator:** GitHub/GitLab tranched drop PRs, YAML-AST dead-rule cleanup PRs, automated Regret Revert PRs.
- [ ] **P2.6 — Kubernetes Operator & CRDs:** `TelemetryAuditPolicy`, `TelemetryBudget`, `TelemetryRecommendation` (with `breakGlassUntil`).
- [ ] **P2.7 — Multi-Cluster & Per-Selector Drop Scoping:** `pkg/scope/`, Ledger v2 migration, global-query safety invariant.

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
- [ ] Planned in **P2.1b** ([docs/PLAN.md](PLAN.md)): Safety Matrix tests `#11` (log-rotation survival + trailing coverage bitmap), `#12` (observation-only `analyze` + confirmed soak), and `#13` (per-expression query-log wildcard detection).

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
| `pkg/synthesizer/` | Done | `relabel_test.go` (shared relabel regex & chunking helpers) |
| `pkg/synthesizer/alloy/` | Done | `alloy_test.go` + 3 golden files |
| `pkg/synthesizer/otel/` | Done | `otel_test.go` + 2 golden files |
| `pkg/synthesizer/promrw/` | Done | `promrw_test.go` + 5 golden files + `model/relabel` execution tests |
| `pkg/report/` | Done | `report_test.go` (`table`, `markdown`, `json`) |
| `examples/demo/` | Done | Offline 391k-series demo + Alloy, OTel & Prometheus `remote_write` configs |
| `.github/workflows/`, `.golangci.yml`, `.goreleaser.yaml`, `release-please-config.json`, `.release-please-manifest.json`, `Dockerfile` | Done | `golangci-lint` (0 issues), `goreleaser check` + snapshot build, `release-please` + rolling `main` GHCR config, Docker Buildx smoke & arm64 cross-compile |

---

## Architecture & Implementation Notes for Future Sessions

- **PromQL Parser API (Prometheus v0.316.0):** `parser.ParseExpr` no longer exists; always use `parser.NewParser(parser.Options{EnableExperimentalFunctions: true, EnableBinopFillModifiers: true, EnableUnquotedUTF8Names: true}).ParseExpr(...)` (already centralized in `pkg/usage/promql`).
- **Deterministic Clock:** CLI accepts `--now <RFC3339>` and `engine.AnalyzeInput.Now` accepts a `time.Time` so soak windows, observation windows, and CI/demo tests are 100% deterministic.
- **Minimal Dependencies:** Only 3 direct entries in `go.mod` (`github.com/prometheus/prometheus`, `github.com/prometheus/common` [already transitive, promoted for `relabel` validation tests], `gopkg.in/yaml.v3`) plus Go stdlib.
- **Scope Guardrails Honored:** No K8s Operator/CRDs, no OpenCost FinOps exporter, no GitOps PR actuator, no direct Alloy parser, and no Logs/Traces/OpAMP were added in Phase 1.

---

## How to Resume in a New Session (Phase 2 Handoff)

1. Read [AGENT.md](../AGENT.md), [docs/PLAN.md](PLAN.md), [docs/BACKENDS_AND_AUTH.md](BACKENDS_AND_AUTH.md), [agent_work.md](../agent_work.md), and this file (`docs/CHECKPOINT.md`).
2. Confirm green baseline:
   ```bash
   go test -race ./... && go build ./cmd/trimtab
   make demo && make demo-otel && make demo-promrw
   ```
3. **Next Build Target — Phase 2.1b** (full specifications for all upcoming phases live in [docs/PLAN.md](PLAN.md); refine/append there rather than creating separate phase plan files, and update this checkpoint after each item):
   - **P2.1b — Safety-Critical Hardening:** Log-rotation survival & trailing `CoveredDates` bitmap, observation-only `analyze` + explicit `synth --activate` + confirmed soak, per-expression query-log wildcards, PromQL scalar binary precision, HWM baselines, Safety Matrix #11–#13.
   - **P2.1c — Unified Backend Auth (`pkg/httpclient`), Live-Scale Hardening & `trimtab doctor`:** Reusable Basic/Bearer/Custom-Header/TLS auth, `User-Agent: trimtab/` auto-exclusion, `/status/tsdb?limit` + `selector`, Loki pagination + `param_query` + streaming dedup, Grafana datasource filtering + search pagination, Alloy output modes, `trimtab doctor`.
   - **P2.2 — Critical Infrastructure & Ruler Usage Sources (`pkg/usage/ruler/`, `pkg/usage/k8sconsumers/`):** Mimir/Thanos Ruler API, KEDA `ScaledObject`/`ScaledJob`, `prometheus-adapter`, Argo Rollouts `AnalysisTemplate`, Flagger `MetricTemplate`/`Canary` (extending `BaselineCounts` in `pkg/state`).
   - **P2.3 — Observability Ecosystem Static Consumers (`pkg/usage/alertmanager/`, `pkg/usage/perses/`, `pkg/usage/slo/`).**
   - **P2.4 — Per-Team Attribution & OpenCost-Style FinOps Exporter (`pkg/attribution/`, `pkg/exporter/`).**
   - **P2.5 — GitOps PR Actuator (`pkg/actuator/gitops/`).**
   - **P2.6 — Kubernetes Operator & CRDs (`api/v1alpha1/`, `internal/controller/`, `cmd/trimtab-operator/`).**
   - **P2.7 — Multi-Cluster / Per-Selector Drop Scoping (`pkg/scope/`).**
