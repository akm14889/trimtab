# AGENT.md — Trimtab Project Guide

> Read this file first in any new session, along with [docs/CHECKPOINT.md](docs/CHECKPOINT.md) for current build progress and resume instructions. It captures the architecture, non-negotiable safety invariants, synthesizer contracts, phased scope, and package map so you do not need to scan every file to get oriented.
> Full rationale, examples, and diagrams live in [docs/DESIGN.md](docs/DESIGN.md); compatible backends & authentication live in [docs/BACKENDS_AND_AUTH.md](docs/BACKENDS_AND_AUTH.md); reusable session-trigger prompts live in [agent_work.md](agent_work.md).

---

## 1. What Is Trimtab?

**Trimtab** (`trimtab.io`) is an open-source, CNCF-aligned **adaptive telemetry control plane** written in **Go**.
- Audits telemetry inventory + runtime query logs + static configs (Grafana dashboards/managed alerts, Prometheus rules), rolls out safe tranched drop rules behind a one-time collector insertion point, reports dead recording rules and high-cardinality label waste, and watches for post-drop regret.
- **Strictly complementary** to Prometheus, Mimir, Thanos, OpenTelemetry Collector, Grafana Alloy, Grafana, Perses, OpenCost, `prom-analytics-proxy`, and Argo CD / Flux. Never replaces them and never sits in the telemetry data path.
- **v1 Scope:** Single tenant, usage evaluated globally per **Metric Family**.

---

## 2. Current Status & Delivery Order

- **Current State:** **Phase 1 (Metrics CLI MVP + Grafana Alloy)**, **Phase 1.1 (OpenTelemetry Collector Synthesizer + Loki Query-Log Source)**, **Phase 2.1 (Prometheus `remote_write` Synthesizer)**, and **Phase 2.1a (GitHub PR CI, Release Pipeline & Toolchain Hygiene)** are **implemented and verified** (`go test -race ./... && go build ./cmd/trimtab` passes with 10/10 Mandatory Safety Test Matrix invariants and golden tests green, plus `golangci-lint` and GoReleaser/Docker checks). See [docs/CHECKPOINT.md](docs/CHECKPOINT.md) for exact package inventory and [docs/PLAN.md](docs/PLAN.md) for the single living implementation plan.
- **Phase 1 + 1.1 + 2.1 + 2.1a (Completed):**
  1. Core Metrics engine (family-level modeling) + PromQL AST extractor (`prometheus/prometheus/promql/parser` v0.316.0) + State Ledger (cold-start coverage, baselines, observation window, tranches, `active_drops`) + Regret Detection.
  2. Usage sources: **Globbed Prometheus query logs** (`+ prom-analytics-proxy` + **Loki-backed query-log source**), **Grafana dashboards + Grafana-managed alerts** (API + local JSON), **Prometheus rule files** (with recording-rule transitive DAG).
  3. **Synthesizers:** **Grafana Alloy (`import.file` module)** (`pkg/synthesizer/alloy`), **OpenTelemetry Collector (`filter/trimtab` via `--config` insertion point)** (`pkg/synthesizer/otel`), AND **Prometheus `remote_write` (`write_relabel_configs` / `remote_write` / Operator patch)** (`pkg/synthesizer/promrw`).
  4. Screenshot-able CLI report (`trimtab analyze`, `trimtab synth`, `trimtab version`) in `table`, `markdown`, and `json` + bundled offline demo fixtures (`examples/demo/`, `make demo`, `make demo-otel`, `make demo-promrw`).
  5. **CI, Release & Toolchain Hygiene:** `go 1.27` in `go.mod`, `.github/workflows/ci.yml` + `.golangci.yml` + `make lint`, and `.github/workflows/release.yml` + `.goreleaser.yaml` + multi-arch `Dockerfile` (static binaries, checksums, SPDX SBOM, GHCR multi-arch images, build provenance attestations).
- **Phase 2 (Remaining Build Targets — P2.1b–P2.7 in [docs/PLAN.md](docs/PLAN.md)):** Safety-critical hardening (`P2.1b` — log-rotation survival, observation-only `analyze` + confirmed soak, per-expression wildcards), unified backend auth & live-scale hardening (`P2.1c` — `pkg/httpclient`, pagination, `trimtab doctor`), additional usage sources (Mimir/Thanos Ruler API, KEDA, `prometheus-adapter`, Argo Rollouts/Flagger, Alertmanager, Perses, Sloth/Pyrra), per-team attribution & budgets ("OpenCost for telemetry"), GitOps PRs (tranched drops, dead rule cleanup, automated Regret Revert PRs), K8s Operator & CRDs (`trimtab.io/v1alpha1`), and multi-cluster scoping.
- **Phase 3 (Later):** Shard-safe aggregation synthesizer, direct Alloy config import add-on, Logs (`pkg/logs`), Traces (`pkg/traces`), OpAMP break-glass.

---

## 3. Non-Negotiable Safety Invariants (NEVER Violate)

Every code change and test MUST preserve these invariants:

1. **No Query Log / Incomplete Cold-Start Coverage ⇒ No `DROP` Rules.** Static analysis misses KEDA, HPA (`prometheus-adapter`), Argo Rollouts/Flagger canaries, SLOs, and scripts. Without a runtime query log covering `>= minObservationWindow` (tracked in ledger as `query_log_coverage`), Trimtab MUST stay in Report-Only mode (showing projected savings). Warn—do **not** hard-fail—if local Prometheus TSDB retention `< window` (remote-write setups keep 15d locally while ledger + logs span 30d).
2. **Query Log Origin Classification.** Split entries into `rule-eval` (`ruleGroup != nil || httpRequest == nil`), `api-user` (`httpRequest != nil`), and `excluded-explorer`. Recording-rule evals feed **only** the DAG (never live sinks); alerting-rule evals **are** live sinks.
3. **Tranched Rollout + Sanity Ceiling.** Cap each rollout tranche at `maxDropPercentPerTranche` (default `15%`, sorted descending by series savings, never splitting a metric family). Gate tranche `N+1` on a clean `soakWindow` with `0` unresolved regrets. Keep a separate sanity-abort ceiling (`maxTotalDroppablePercent`, default `80%`) to catch broken query-log wiring.
4. **Baseline Drift Circuit Breaker (Partial Results ≠ HTTP Errors).** Store per-source object counts in `pkg/state`. If any source's count drops `>20%` vs. previous run (e.g., Grafana token lost folder permissions and returned HTTP 200 with fewer dashboards), **abort drop synthesis** unless `--accept-baseline-shift` is passed. Any hard fetch error also aborts synthesis.
5. **Observation Window ≠ TSDB Head Block.** `/api/v1/status/tsdb` only sees ~2h (misses weekly CronJobs/batch metrics). Combine historical `/api/v1/label/__name__/values?start=...&end=...` with `first_seen_at` in the State Ledger; never drop a metric family seen for `< minObservationWindow`.
6. **Default `neverDrop` Allowlist & Wildcard Exclusions.** Always protect meta-monitoring metrics by default (`^up$`, `^ALERTS$`, `^ALERTS_FOR_STATE$`, `^scrape_.*`, `^prometheus_.*`, `^alloy_.*`, `^otelcol_.*`). Flag any single source matching `>50%` of inventory as a Wildcard Protector and allow excluding it via `excludeSources`.
7. **Metric Families & Synthesizer Capabilities.** Model classic histograms (`_bucket`/`_sum`/`_count`/`_created`) and summaries at the **family** level so `_sum`/`_count` are never orphaned. Recognize native histograms separately. Synthesizers declare `Capabilities` (`CanDropHistogramBucketsOnly`, etc.); engine only synthesizes actions the target supports.
8. **Full Label-Usage Semantics & Dead Recording-Rule Chains:**
   - `sum without (pod)` and bare `rate(x[5m])` preserve **all remaining labels** (mark them used!).
   - Parse string literal arguments in `label_replace` and `label_join`.
   - Scan non-PromQL label references: Grafana `legendFormat` (`{{pod}}`), links, overrides, transformations, template vars, AND Prometheus rule `annotations`/`labels` Go templates (`{{ $labels.pod }}`).
   - Run **transitive closure** on the recording-rule DAG. For a dead chain: drop the source at the collector (if no other live consumers) **AND** emit a "Dead Recording Rules to Delete" advisory (ruler bypasses collector; `or vector(0)` rules keep emitting zeros until deleted).
9. **`DROP_DIMENSION` Is Advisory-Only in v1.** Hard-protect `le`, `quantile`, and `__name__`. Estimate single-label and multi-label savings honestly via **`without`**: `count(count without (pod_uid) (m))` and `count(count without (pod, pod_uid, instance) (m))`.
10. **Forward-Only Regret Detection (Default Threshold = 1).** Track `active_drops` in `pkg/state`. Watch `api-user` query logs for queries matching dropped families. Default `regretThreshold: 1` (Prometheus query logs lack user identity and 1 on-call SRE hitting an empty graph matters; `minRegretHits` optional). On regret: emit `RegretEvent`, un-drop metric, pin to `neverDrop`, block next tranche. Document clearly that already-dropped samples are gone permanently.
11. **Don't Over-Abstract Metrics.** Keep `pkg/metrics` idiomatic and first-class; share generic abstractions only at the orchestration/lifecycle layer (`state`, `synthesizer`, `report`).

---

## 4. Synthesizer Insertion-Point Contracts

Never emit standalone snippets that cannot load in a real collector.

- **Grafana Alloy (`pkg/synthesizer/alloy` — Build 1st):**
  - Covers the **Prometheus-native path** (`prometheus.scrape` AND `otelcol.exporter.prometheus` → `trimtab.drop_unused` → `prometheus.remote_write`).
  - Human wires `import.file "trimtab"` + `trimtab.drop_unused "default" { forward_to = [...] }` once.
  - Trimtab owns only `trimtab.alloy` containing `declare "drop_unused"` with `argument "forward_to"`, `export "receiver"`, and `prometheus.relabel "trimtab_drop"` using **`forward_to = argument.forward_to.value`** (NOTE `.value`!).
  - Relabel regexes are implicitly anchored in Prometheus/Alloy (`^...$` unnecessary). When 0 families are dropped: omit `rule { ... }` blocks (valid pass-through).
  - Capabilities: `CanDropFamily: true`, `CanDropHistogramBucketsOnly: true`.
- **OpenTelemetry Collector (`pkg/synthesizer/otel` — Phase 1.1 CNCF Target):**
  - Human wires `filter/trimtab` into `service.pipelines.metrics.processors` once and runs `--config=file:base.yaml --config=file:trimtab-rules.yaml`.
  - Trimtab owns only `trimtab-rules.yaml` containing `processors.filter/trimtab` (**NEVER** emit a `service:` block—OTel config merge overwrites pipeline lists!).
  - **NEVER heuristically reverse-translate Prometheus names to OTLP names** (underscore escaping is lossy and ambiguous). Only emit OTel drops when names are deterministically verified (Prometheus receiver, Prometheus 3 `NoTranslation` UTF-8, or exact mapping); otherwise skip with advisory warning.
  - When 0 families are dropped: emit a valid non-matching OTTL predicate (`'name == "__trimtab_noop__"'`), never an empty `{}` map.
  - Capabilities: `CanDropFamily: true`, `CanDropHistogramBucketsOnly: false`.
- **Prometheus `remote_write` (`pkg/synthesizer/promrw` — Phase 2.1):**
  - Emits `write_relabel_configs` overlays (`relabel_configs` default, `remote_write` entry, or camelCase `monitoring.coreos.com/v1` `prometheus_operator_patch`) sharing Prometheus relabel regex generation via `pkg/synthesizer/relabel.go`.
  - When 0 families are dropped: emits `write_relabel_configs: []` (or `writeRelabelConfigs: []` in operator patch mode).
  - Capabilities: `CanDropFamily: true`, `CanDropHistogramBucketsOnly: true`.

---

## 5. Planned Package Map

| Path | Responsibility |
| :--- | :--- |
| `cmd/trimtab/` | CLI entrypoint (`analyze`, `synth`, `version`). |
| `pkg/config/` | `trimtab.yaml` schema, defaults (`neverDrop`, `maxDropPercentPerTranche`, `maxTotalDroppablePercent`, `minObservationWindow`, `soakWindow`, `excludeSources`). |
| `pkg/metrics/` | First-class Metrics domain types (`MetricFamily`, `SeriesStats`, `UsageIndex`, `LabelUsage`, `AuditReport`, `Recommendation`, `DeadRuleAdvisory`, `RegretEvent`). |
| `pkg/state/` | Persistent State Ledger (`.trimtab-state.json`): `baseline_counts`, `query_log_coverage`, `seen_families` (`first_seen_at`, `last_seen_at`, `last_queried_at`), `tranches`, `active_drops`. |
| `pkg/inventory/prometheus/` | Prometheus/Mimir HTTP API client (`/api/v1/metadata`, historical metric names, `/api/v1/status/tsdb`, bounded `without` collapse queries) + offline JSON fixture loader. |
| `pkg/usage/promql/` | Official `prometheus/prometheus/promql/parser` AST walker extracting families, regexes, `by`/`without`/bare preservation, `label_replace`/`label_join`, `on`/`ignoring`/`group_left`/`group_right`. |
| `pkg/usage/querylog/` | Glob/directory JSONL reader for Prometheus `query_log_file` (+ `prom-analytics-proxy` export) with `api-user` / `rule-eval` / `excluded-explorer` origin classifier. |
| `pkg/usage/grafana/` | Grafana dashboard scanner (HTTP API + local JSON dir: PromQL, `legendFormat`, template vars, links/overrides/transforms) + Grafana-managed Alert Rules API. |
| `pkg/usage/promrules/` | Prometheus alerting & recording rule YAML parser + Go template `{{ $labels.* }}` scanner. |
| `pkg/engine/` | `family.go` (classic/native histogram & summary grouper), `dag.go` (recording-rule transitive closure + dead-chain advisory), `analyzer.go` (correlator, cold-start gating, `without` collapse), `safety.go` (baselines, `neverDrop`, wildcards, tranche planner), `regret.go` (forward-only Regret Detection). |
| `pkg/synthesizer/alloy/` | Phase 1 (1st): Grafana Alloy `import.file` module generator (`trimtab.alloy`). |
| `pkg/synthesizer/otel/` | Phase 1.1 (CNCF Target): OTel Collector `filter/trimtab` generator (`trimtab-rules.yaml`). |
| `pkg/synthesizer/promrw/` | Phase 2.1: Prometheus `remote_write[].write_relabel_configs` & Operator patch generator. |
| `pkg/report/` | Screenshot-able terminal summary table, Markdown, and JSON renderers. |
| `examples/demo/` | Self-contained offline fixture dataset demonstrating tranched rollout, dead rule chain, `without` label collapse, and regret check. |

---

## 6. Coding & Testing Conventions

- **Go Style:** Idiomatic Go, minimal third-party dependencies. Always use `github.com/prometheus/prometheus/promql/parser` for PromQL parsing (never regex-hack PromQL).
- **Offline, Deterministic Tests:** Unit tests must never require live Prometheus or Grafana; use `httptest.NewServer` or fixture files.
- **Golden Tests:** Every synthesizer must have golden file tests covering (a) normal family drops, (b) 0-drop no-op/pass-through output, (c) bucket-only capability behavior, and (d) valid syntax (`argument.forward_to.value` in Alloy; no `service:` block in OTel).
- **Mandatory Safety Test Matrix (must stay green):**
  1. Missing query log OR `query_log_coverage < minObservationWindow` ⇒ `0` synthesized drops (report-only with projected savings).
  2. `rule-eval` entries in query log do NOT mark recording-rule inputs as live-used; `api-user` entries and alerting rules DO.
  3. Large droppable estate (e.g., 47%) ⇒ split into tranches ≤ `maxDropPercentPerTranche` (15%), sorted by series savings, never splitting a histogram family; `>80%` ⇒ sanity-abort.
  4. Simulated Grafana partial response (`100` → `20` dashboards) ⇒ synthesis aborted by baseline-drift guard.
  5. Metric family seen `< minObservationWindow` ⇒ protected from drop.
  6. Default `neverDrop` (`up`, `ALERTS`, `scrape_*`, `prometheus_*`, `alloy_*`, `otelcol_*`) ⇒ always protected.
  7. Dead recording-rule chain (`M -> R1 -> R2`) ⇒ `M` dropped at collector (if no other consumers) + `R1`/`R2` emitted in Dead Recording Rules advisory; live chain ⇒ all protected.
  8. `sum without (pod)`, bare `rate(x[5m])`, `label_replace`, Grafana `legendFormat`, alert `{{ $labels.x }}` ⇒ labels marked used.
  9. `le` and `quantile` ⇒ never suggested in advisory label drops; single-label `without (pod_uid)` honestly shows `0%` collapse when `pod` remains.
  10. `api-user` query hits a metric in `active_drops` ⇒ `RegretEvent` emitted, metric removed from drops & pinned to `neverDrop`, next tranche blocked.
  - *Queued in `P2.1b` (`#11–#13`):* log-rotation survival & trailing `CoveredDates` bitmap, observation-only `analyze` + confirmed soak, and per-expression query-log wildcard detection.
- **Living Plan & Checkpoint Convention:** Never create per-phase plan files (`PHASE2_PLAN.md`, `PHASE3_PLAN.md`, etc.). Append and refine all upcoming phases/sub-phases inside [docs/PLAN.md](docs/PLAN.md), and keep [docs/CHECKPOINT.md](docs/CHECKPOINT.md) updated after each completed sub-phase.
- **Verification Command:** `go test -race ./... && go build ./cmd/trimtab`

---

## 7. Scope Guardrails (What NOT to Build in Phase 1)

Do **not** add these until Phase 1 CLI MVP is complete and verified:
- Kubernetes Operator / `controller-runtime` / CRDs (Phase 2).
- Per-team attribution, budgets, and OpenCost FinOps exporter (Phase 2).
- GitHub/GitLab PR creation & multi-cluster per-selector scoping (Phase 2).
- Direct Alloy config import / parser (Phase 3 add-on).
- Shard-safe aggregation synthesizer, Logs/Loki signals, Traces, or OpAMP (Phase 3).
