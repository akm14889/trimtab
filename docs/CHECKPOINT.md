# Trimtab — Session Checkpoint & Progress Tracker

> **Purpose:** Compact living handoff & progress tracker so any resumed or new session sees what is built, verified, and next.
> **Single Plan Rule:** All detailed phase/sub-phase plans live in [docs/PLAN.md](PLAN.md) — **never** create separate `PHASE2_PLAN.md` / `PHASE3_PLAN.md` files; append/refine specs in `docs/PLAN.md` and keep this checkpoint updated.
> **Read alongside:** [AGENT.md](../AGENT.md) | [docs/DESIGN.md](DESIGN.md) | [docs/BACKENDS_AND_AUTH.md](BACKENDS_AND_AUTH.md) | [docs/PLAN.md](PLAN.md) | [../agent_work.md](../agent_work.md) | [README.md](../README.md)

---

## Current Status

- **Completed Phases:** **Phase 1 (Metrics CLI MVP + Grafana Alloy)**, **Phase 1.1 (OpenTelemetry Collector Synthesizer + Loki Query-Log Source)**, **Phase 2.1 (Prometheus `remote_write` Synthesizer)**, **Phase 2.1a (GitHub PR CI, Release Pipeline & Toolchain Hygiene)**, **Phase 2.1b (Safety-Critical Hardening)**, **Phase 2.1c (Unified Backend Auth, Live-Scale Hardening & `trimtab doctor`)**, **Phase 2.1d (Lean Helm Chart & Non-Prod Quickstart)**, **Phase 2.2 (Critical Infrastructure & Ruler Usage Sources)**, AND **Phase 2.3 (Observability Ecosystem Static Consumers)**
- **Overall State:** Verified & Green — ready to execute **Phase 2.4 (Live-Scale Hardening & Incremental Query-Log Ingestion)** (see [docs/PLAN.md](PLAN.md))
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
- [x] **P2.1b — Safety-Critical Hardening:** Log-rotation `LastQueriedAt`/`LastBucketQueriedAt` survival + trailing-window `CoveredDates` bitmap, observation-only `analyze` + explicit `synth --activate` / `--preview-next` + confirmed soak (`pending_confirmation -> soaking`), per-expression query-log wildcard detection, PromQL scalar binary label precision, HWM baselines + daily query-log rate, Safety Matrix #11–#13.
- [x] **P2.1c — Unified Backend Auth (`pkg/httpclient`), Live-Scale Hardening & `trimtab doctor`:** Lean stdlib `HTTPAuthConfig` (No Auth, Basic Auth, Bearer, Custom Header Auth `*Env`/`*File`, `X-Scope-OrgID`, TLS/mTLS, secret redaction, `User-Agent: trimtab/` self-query auto-exclusion), `/status/tsdb?limit` + `selector`, Loki forward pagination + `param_query`/`param_match[]` + streaming dedup, Grafana non-Prometheus datasource filter + `/api/search` pagination + `/api/ruler/grafana/api/v1/rules`, Alloy `module|relabel_rules|configmap` modes, `deploy/k8s/` CronJob, `trimtab doctor`.
- [x] **P2.1d — Lean Helm Chart (`charts/trimtab/`) & Non-Prod Quickstart:** Zero-CRD Helm chart deploying `trimtab` as a read-only `CronJob` + PVC ledger + `envFrom` Secrets + `helm test` (`trimtab doctor`), `values.schema.json`, standalone `charts/trimtab/README.md`, `examples/values-nonprod.yaml` (`1d` window / `15m` soak / `probeFirstSeen`) + `values-prod.yaml` / `values-otel.yaml` / `values-promrw.yaml`, CI `helm lint`/`helm template` + GHCR OCI chart publishing (`pkg/config/helm_chart_test.go`).
- [x] **P2.2 — Critical Infrastructure & Ruler Usage Sources:** Mimir/Cortex (`/prometheus/config/v1/rules`) and Prometheus/Thanos (`/api/v1/rules`) HTTP Ruler scanners (`pkg/usage/ruler/`) + multi-doc YAML `ManifestSource` scanner (`pkg/usage/k8sconsumers/`) covering KEDA, `prometheus-adapter`, Argo Rollouts, and Flagger, plus unified auth/doctor checks and `BaselineCounts` HWM drift protection (`TestSafetyMatrix_P22_RulersAndK8sConsumers`).
- [x] **P2.3 — Observability Ecosystem Static Consumers:** Alertmanager YAML/CRD/`/api/v2/status` label scanner (`pkg/usage/alertmanager/` — scoping protected routing/grouping/inhibit/template labels exclusively to alerted metrics), Perses local dir + REST API dashboard scanner (`pkg/usage/perses/` — `PrometheusTimeSeriesQuery` + variables + `dashboard_uid` wildcard exclusion), and Sloth/Pyrra/OpenSLO specification scanner (`pkg/usage/slo/`), plus `BaselineCounts` HWM drift guards (`TestSafetyMatrix_P23_AlertmanagerPersesAndSLO`).
- [ ] **P2.4 — Live-Scale Hardening & Incremental Query-Log Ingestion:** Incremental query-log cursors (`LastScannedAt` + overlap), `series == 0` remnant filtering, live-scale batching, Helm PVC/Day-2 docs, multi-target E2E profiles (`promrw`, `otel`).
- [ ] **P2.5 — GitOps PR Actuator & Operational Alerting:** GitHub/GitLab tranched drop PRs, YAML-AST dead-rule cleanup PRs, automated `[URGENT]` Regret Revert PRs, `pkg/notify` webhooks + optional status metrics.
- [ ] **P2.6 — Query-Log Genericity & Lightweight In-Cluster K8s Discovery:** Live `prom-analytics-proxy` API, Thanos/VictoriaMetrics log formats, Elasticsearch/OpenSearch + ClickHouse/VictoriaLogs adapters, read-only `inCluster` `ManifestSource`.
- [ ] **P2.7 — Per-Team Attribution & OpenCost-Style FinOps Exporter:** Team resolver chain, `TelemetryBudget` evaluation, `trimtab serve` `/metrics` exporter.
- [ ] **P2.8 — Multi-Cluster & Per-Selector Drop Scoping:** `pkg/scope/`, Ledger v2 migration, global-query safety invariant.
- [ ] **P2.9 — Kubernetes Operator & CRDs *(Deferred / Opt-In)*:** `TelemetryAuditPolicy`, `TelemetryBudget`, `TelemetryRecommendation` (with `breakGlassUntil`).

---

## Mandatory Safety Test Matrix Status (13/13 Green)

All 13 invariants live in [pkg/engine/safety_matrix_test.go](../pkg/engine/safety_matrix_test.go):

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
11. [x] `TestSafetyMatrix_11_LogRotationSurvivalAndTrailingBitmap` — Day-10 family & `_bucket` queries rotated off disk stay protected at Day-30 via ledger timestamps (`LastQueriedAt`/`LastBucketQueriedAt`), expire at Day-45, and a 3-week middle gap in `CoveredDates` forces Report-Only mode.
12. [x] `TestSafetyMatrix_12_ObservationOnlyAnalyzeAndConfirmedSoak` — Observation-only `analyze` never activates tranches; `synth --preview-next` previews without mutating ledger; `synth --activate` enters `pending_confirmation`, warns after `>48h`, and starts soak only once dropped series (including `_bucket`-only drops) reach `0` or `--confirm-tranche` is passed.
13. [x] `TestSafetyMatrix_13_PerExpressionQueryLogWildcards` — Diverse exact queries from a shared caller IP/`User-Agent` across `>50%` of inventory are NOT flagged as wildcards, while a single `{__name__=~".+"}` query IS flagged and can be excluded via `query_regex`/`dashboard_uid` without dropping other queries.
- [x] Bonus: `TestBaselineHWMAndDailyQueryLogRate`, `TestCapabilityNegotiationAndNativeHistogram`, `TestWildcardProtectorAndSoakProgression`, plus Alloy, OTel & PromRW Golden Tests (`pkg/synthesizer/alloy/alloy_test.go`, `pkg/synthesizer/otel/otel_test.go`, `pkg/synthesizer/promrw/promrw_test.go`) and CLI E2E Tests (`cmd/trimtab/main_test.go`).

---

## Implemented Package Map

| Package / Path | Status | Key Tests |
| :--- | :--- | :--- |
| `cmd/trimtab/` | Done | `main_test.go` (Alloy, OTel, JSON, Regret, `doctor` E2E) |
| `pkg/config/` | Done | `config_test.go` (auth/TLS mutual exclusion, Alloy modes, policy hardening) |
| `pkg/httpclient/` | Done | `client_test.go` (Basic, Bearer, Custom Headers `*Env`/`*File` hot-reload, TLS/mTLS, redaction, `429`/`503` retries, `401`/`403` fail-closed) |
| `pkg/metrics/` | Done | `types_test.go` |
| `pkg/state/` | Done | `ledger_test.go` |
| `pkg/inventory/prometheus/` | Done | `client_test.go` (HTTP + short retention warn + fixture `without`) |
| `pkg/usage/promql/` | Done | `extractor_test.go` |
| `pkg/usage/querylog/` | Done | `reader_test.go` (glob, `prom-analytics-proxy`, Loki) |
| `pkg/usage/grafana/` | Done | `scanner_test.go` (HTTP + local dir + managed alerts) |
| `pkg/usage/promrules/` | Done | `scanner_test.go` (alerts, recording rules, Go templates, `or vector(0)`) |
| `pkg/usage/ruler/` | Done | `ruler_test.go` (Mimir YAML `/prometheus/config/v1/rules`, Prometheus/Thanos JSON `/api/v1/rules`, HTTP fail-closed & secret redaction) |
| `pkg/usage/k8sconsumers/` | Done | `scanner_test.go` (`ManifestSource`, KEDA, `prometheus-adapter`, Argo Rollouts, Flagger, `List` unpacking, toggle guards) |
| `pkg/usage/alertmanager/` | Done | `scanner_test.go` (YAML/CRD/`/api/v2/status` routing, inhibit, `group_by '...'`, receiver templates, HTTP 401 redaction) |
| `pkg/usage/perses/` | Done | `scanner_test.go` (local JSON/YAML dir + REST API `/api/v1/projects/...`, variables, `PrometheusTimeSeriesQuery`, HTTP 403 redaction) |
| `pkg/usage/slo/` | Done | `scanner_test.go` (Sloth CRD & standalone, Pyrra `ratio`/`latency`/`bool_gauge` + `grouping`, OpenSLO) |
| `pkg/engine/` | Done | `safety_matrix_test.go` (13/13 invariants + P2.2/P2.3 regression + HWM/rate + capabilities + confirmed soak) |
| `pkg/synthesizer/` | Done | `relabel_test.go` (shared relabel regex & chunking helpers) |
| `pkg/synthesizer/alloy/` | Done | `alloy_test.go` + 3 golden files |
| `pkg/synthesizer/otel/` | Done | `otel_test.go` + 2 golden files |
| `pkg/synthesizer/promrw/` | Done | `promrw_test.go` + 5 golden files + `model/relabel` execution tests |
| `pkg/report/` | Done | `report_test.go` (`table`, `markdown`, `json`) |
| `examples/demo/` | Done | Offline 391k-series demo + Alloy, OTel & Prometheus `remote_write` configs |
| `charts/trimtab/` | Done | Lean zero-CRD `CronJob` Helm chart + PVC ledger + `envFrom` Secrets + `helm test` (`trimtab doctor`), `values.schema.json`, standalone `README.md`, `examples/values-*.yaml` |
| `deploy/k8s/` | Done | Read-only CronJob + PVC ledger + Secret `envFrom` reference manifests |
| `.github/workflows/`, `.golangci.yml`, `.goreleaser.yaml`, `release-please-config.json`, `.release-please-manifest.json`, `Dockerfile` | Done | `golangci-lint` (0 issues), `goreleaser check` + snapshot build, `release-please` + rolling `main` GHCR config, Docker Buildx smoke & arm64 cross-compile |

---

## Architecture & Implementation Notes for Future Sessions

- **PromQL Parser API (Prometheus v0.316.0):** `parser.ParseExpr` no longer exists; always use `parser.NewParser(parser.Options{EnableExperimentalFunctions: true, EnableBinopFillModifiers: true, EnableUnquotedUTF8Names: true}).ParseExpr(...)` (already centralized in `pkg/usage/promql`).
- **Deterministic Clock:** CLI accepts `--now <RFC3339>` and `engine.AnalyzeInput.Now` accepts a `time.Time` so soak windows, observation windows, and CI/demo tests are 100% deterministic.
- **Minimal Dependencies:** Only 3 direct entries in `go.mod` (`github.com/prometheus/prometheus`, `github.com/prometheus/common` [already transitive, promoted for `relabel` validation tests], `gopkg.in/yaml.v3`) plus Go stdlib.
- **Scope Guardrails Honored:** No K8s Operator/CRDs, no OpenCost FinOps exporter, no GitOps PR actuator, no direct Alloy parser, and no Logs/Traces/OpAMP were added.

---

## How to Resume in a New Session (Phase 2 Handoff)

1. Read [AGENT.md](../AGENT.md), [docs/PLAN.md](PLAN.md), [docs/BACKENDS_AND_AUTH.md](BACKENDS_AND_AUTH.md), [agent_work.md](../agent_work.md), and this file (`docs/CHECKPOINT.md`).
2. Confirm green baseline:
   ```bash
   go test -race ./... && go build ./cmd/trimtab
   make demo && make demo-otel && make demo-promrw
   ```
3. **Next Build Target — Phase 2.4** (full specifications for all upcoming phases live in [docs/PLAN.md](PLAN.md); refine/append there rather than creating separate phase plan files, and update this checkpoint after each item):
   - **P2.4 — Live-Scale Hardening & Incremental Query-Log Ingestion (`pkg/state/`, `pkg/usage/querylog/`, `pkg/inventory/prometheus/`, `charts/trimtab/`, `test/e2e/`).**
   - **P2.5 — GitOps PR Actuator & Operational Alerting (`pkg/actuator/gitops/`, `pkg/notify/`).**
   - **P2.6 — Query-Log Genericity & Lightweight In-Cluster K8s Discovery (`pkg/usage/querylog/`, `pkg/usage/k8sconsumers/`).**
   - **P2.7 — Per-Team Attribution & OpenCost-Style FinOps Exporter (`pkg/attribution/`, `pkg/exporter/`).**
   - **P2.8 — Multi-Cluster / Per-Selector Drop Scoping (`pkg/scope/`).**
   - **P2.9 — Kubernetes Operator & CRDs *(Deferred / Opt-In)* (`api/v1alpha1/`, `internal/controller/`, `cmd/trimtab-operator/`).**
