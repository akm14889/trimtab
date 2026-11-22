# Trimtab — Living Iterative Implementation Plan

> **Purpose:** Single evergreen implementation plan for Trimtab across all phases.
> **Progress Tracking:** Pair this plan with [docs/CHECKPOINT.md](CHECKPOINT.md) (compact live handoff & milestone status).
> **Read alongside:** [DESIGN.md](DESIGN.md) | [CHECKPOINT.md](CHECKPOINT.md) | [../AGENT.md](../AGENT.md)

---

## 1. Living Plan Convention & Iterative Workflow

1. **Single Plan File Rule:** **Never create per-phase plan files** (`PHASE2_PLAN.md`, `PHASE3_PLAN.md`, etc.). All future planning, scope refinements, and new phases MUST be appended to or updated in-place within this file (`docs/PLAN.md`), while [docs/CHECKPOINT.md](CHECKPOINT.md) stays the compact session checkpoint and handoff tracker.
2. **Iterative Development Loop (Per Sub-Phase):**
   1. **Refine:** Tighten the target sub-phase specification below (types, config/CRD schema, edge cases, verification plan) before coding.
   2. **Build & Test:** Implement the sub-phase alongside deterministic offline tests (`httptest.NewServer` / fixtures), golden tests (for synthesizers), and regression coverage for all 11 Safety Invariants in [AGENT.md](../AGENT.md).
   3. **Verify:** Run `go test -count=1 -race ./... && make verify` plus applicable `make demo*` targets.
   4. **Checkpoint:** Mark the completed sub-phase `[x]` in the Master Phase Tracker below and update [docs/CHECKPOINT.md](CHECKPOINT.md) (plus `AGENT.md` / `README.md` if CLI/config/targets changed).

---

## 2. Master Phase Tracker

| Phase | Sub-Phase | Name | Primary Packages | Status |
| :--- | :--- | :--- | :--- | :--- |
| **Phase 1** | **P1.0** | Metrics CLI MVP + Grafana Alloy Synthesizer | `cmd/trimtab/`, `pkg/config/`, `pkg/metrics/`, `pkg/state/`, `pkg/inventory/prometheus/`, `pkg/usage/*`, `pkg/engine/`, `pkg/synthesizer/alloy/`, `pkg/report/` | `[x]` Completed |
| **Phase 1.1** | **P1.1** | OTel Collector Synthesizer + Loki Query-Log Source | `pkg/synthesizer/otel/`, `pkg/usage/querylog/loki.go` | `[x]` Completed |
| **Phase 2** | **P2.1** | Prometheus `remote_write` Synthesizer | `pkg/synthesizer/relabel.go`, `pkg/synthesizer/promrw/` | `[x]` Completed |
| **Phase 2** | **P2.2** | Critical Infrastructure & Ruler Usage Sources | `pkg/usage/ruler/`, `pkg/usage/k8sconsumers/` | `[ ]` Next |
| **Phase 2** | **P2.3** | Observability Ecosystem Static Consumers | `pkg/usage/alertmanager/`, `pkg/usage/perses/`, `pkg/usage/slo/` | `[ ]` Planned |
| **Phase 2** | **P2.4** | Per-Team Attribution & FinOps Exporter | `pkg/attribution/`, `pkg/exporter/` | `[ ]` Planned |
| **Phase 2** | **P2.5** | GitOps PR Actuator | `pkg/actuator/`, `pkg/actuator/gitops/` | `[ ]` Planned |
| **Phase 2** | **P2.6** | Kubernetes Operator & CRDs | `api/v1alpha1/`, `internal/controller/`, `cmd/trimtab-operator/` | `[ ]` Planned |
| **Phase 2** | **P2.7** | Multi-Cluster & Per-Selector Drop Scoping | `pkg/scope/`, `pkg/state/`, `pkg/engine/` | `[ ]` Planned |
| **Phase 3** | **P3.1** | Shard-Safe Aggregation Synthesizer | `pkg/synthesizer/aggregate/` | `[ ]` Future |
| **Phase 3** | **P3.2** | Direct Alloy Config Import Add-on | `pkg/discovery/alloy/` | `[ ]` Future |
| **Phase 3** | **P3.3** | Logs Signal Engine (Loki / LogQL) | `pkg/logs/` | `[ ]` Future |
| **Phase 3** | **P3.4** | Traces Signal Engine (Tail Sampling) | `pkg/traces/` | `[ ]` Future |
| **Phase 3** | **P3.5** | OpAMP Break-Glass Actuator | `pkg/actuator/opamp/` | `[ ]` Future |

### Dependency Graph

```mermaid
flowchart LR
    P1["Phase 1 + 1.1<br/>CLI MVP, Alloy & OTel, Loki"]
    P21["P2.1: Prometheus remote_write<br/>(pkg/synthesizer/promrw)"]
    P22["P2.2: Ruler APIs & K8s Consumers<br/>(KEDA, HPA, Rollouts, Flagger)"]
    P23["P2.3: Ecosystem Consumers<br/>(Alertmanager, Perses, Sloth/Pyrra)"]
    P24["P2.4: Team Attribution & Budgets<br/>(OpenCost-style /metrics)"]
    P25["P2.5: GitOps PR Actuator<br/>(GitHub / GitLab / Regret PRs)"]
    P26["P2.6: K8s Operator & CRDs<br/>(trimtab.io/v1alpha1)"]
    P27["P2.7: Multi-Cluster Scoping<br/>(Per-selector drops)"]
    P31["P3.1: Shard-Safe Aggregation<br/>(pkg/synthesizer/aggregate)"]
    P32["P3.2: Direct Alloy Import<br/>(pkg/discovery/alloy)"]
    P33["P3.3–P3.5: Logs, Traces & OpAMP<br/>(Multi-signal expansion)"]

    P1 --> P21
    P1 --> P22
    P22 --> P23
    P22 --> P24
    P21 --> P25
    P23 --> P25
    P24 --> P26
    P25 --> P26
    P22 --> P26
    P21 --> P27
    P26 --> P27
    P27 --> P31
    P21 --> P32
    P32 --> P31
    P26 --> P33
```

---

## 3. Safety-Invariant Continuity Matrix

All 11 Non-Negotiable Safety Invariants in [AGENT.md](../AGENT.md#L30-L49) MUST remain green across every iteration:

| Invariant | Cross-Phase Guardrail |
| :--- | :--- |
| **#1 Mandatory Query Log & Cold-Start** | P2.2/P2.3 static scanners NEVER authorize drops without runtime query logs covering `>= minObservationWindow`. |
| **#2 Query Log Origin Classification** | Recording rules returned by Mimir/Thanos Ruler APIs (P2.2) feed ONLY `ResolveRecordingRuleDAG`, never live sinks. |
| **#3 Tranched Rollout + Sanity Ceiling** | P2.1 obeys `ActiveDrops` (never un-tranched); P2.5 opens one PR per active tranche; P2.6 respects `soakWindow` and `approvedTranche`. |
| **#4 Baseline Drift Circuit Breaker** | P2.2 and P2.3 extend `state.BaselineCounts` and `engine.SourceCounts` so a partial API response from Ruler/Perses/K8s aborts synthesis. |
| **#5 Observation Window != Head Block** | P2.6 stores `StateLedger` persistently in ConfigMap/PVC so Pod restarts do not reset `first_seen_at`. |
| **#6 Default `neverDrop` & Wildcards** | Wildcard detection & `excludeSources` apply uniformly to Perses dashboards (P2.3) and K8s consumers (P2.2). |
| **#7 Family Grouping & Capabilities** | `promrw` (P2.1) sets `CanDropHistogramBucketsOnly: true` via shared relabel helper; P2.7 refuses scoped OTel drops without `scopeAttributeMap`. |
| **#8 Full Label-Usage & Dead-Chain DAG** | Alertmanager `group_by`/matchers/templates (P2.3) protect labels via `LabelUsage`; P2.5 dead-rule cleanup PRs use `yaml.Node` AST editing. |
| **#9 `DROP_DIMENSION` Advisory-Only (until P3.1)** | P2.4/P2.7 keep `DROP_DIMENSION` read-only; P3.1 introduces shard-safe aggregation only when collector shard identity is verified. |
| **#10 Forward-Only Regret Detection** | P2.5 opens `[URGENT]` Regret Revert PRs immediately; P2.6 surfaces active regrets + `breakGlassUntil` override on `TelemetryRecommendation`. |
| **#11 Don't Over-Abstract Metrics** | `pkg/actuator` (P2.5) and `pkg/state` remain signal-agnostic; `pkg/metrics`, `pkg/attribution`, `pkg/scope`, `pkg/logs` (P3.3), and `pkg/traces` (P3.4) stay idiomatic per signal. |

---

## 4. Phase 1 & Phase 1.1 — Foundation *(Completed)*

- **CLI & Core Engine:** `cmd/trimtab` (`analyze`, `synth`, `version`), `pkg/config`, `pkg/metrics`, `pkg/state` (atomic `.trimtab-state.json` ledger), `pkg/engine` (`family.go`, `dag.go`, `safety.go`, `regret.go`, `analyzer.go`), and `pkg/report` (`table`, `markdown`, `json`).
- **Discovery & Usage Sources:** Prometheus/Mimir HTTP + offline fixtures + `without` collapse (`pkg/inventory/prometheus`), PromQL AST walker (`pkg/usage/promql`), rotated JSONL / `prom-analytics-proxy` / Loki query logs (`pkg/usage/querylog`), Grafana dashboards + managed alerts (`pkg/usage/grafana`), and Prometheus rules YAML (`pkg/usage/promrules`).
- **Synthesizers:** Grafana Alloy `import.file` (`pkg/synthesizer/alloy`) and OpenTelemetry Collector `filter/trimtab` (`pkg/synthesizer/otel`) with golden tests + 10/10 Mandatory Safety Test Matrix (`pkg/engine/safety_matrix_test.go`).

---

## 5. Phase 2 — "OpenCost for Telemetry", K8s Operator, GitOps & Expanded Consumers

### P2.1 — Prometheus `remote_write` Synthesizer *(Completed)*

- **Goal:** Provide a drop-in Prometheus `remote_write[].write_relabel_configs` synthesizer (`DESIGN §7.3`) so standalone Prometheus, Prometheus Agent, and Prometheus Operator deployments can enforce trimtab drops without Alloy or OTel Collector.
- **Packages & Key Types:**
  - `pkg/synthesizer/relabel.go`: shared Prometheus relabel primitives (`DefaultMaxTermsPerRule = 40`, `SortActiveDrops`, `BuildRelabelRegexTerm`, `ChunkRelabelRegexes`, `CollectRelabelTerms`) shared identically by `pkg/synthesizer/alloy` and `pkg/synthesizer/promrw`.
  - `pkg/synthesizer/promrw/promrw.go`: `Synthesizer`, `Mode` (`relabel_configs`, `remote_write`, `prometheus_operator_patch`), `Options`.
    - `relabel_configs` (default): emits top-level `write_relabel_configs` (`write_relabel_configs: []` on 0 drops) in `trimtab-remote-write.yaml`.
    - `remote_write`: emits `remote_write:` entry with `name`, `url`, and `write_relabel_configs`.
    - `prometheus_operator_patch`: emits camelCase `monitoring.coreos.com/v1` `Prometheus` / `PrometheusAgent` strategic-merge patch (`spec.remoteWrite[].writeRelabelConfigs[].sourceLabels`) in `trimtab-prometheus-operator-patch.yaml`.
    - `Capabilities()`: `CanDropFamily: true`, `CanDropHistogramBucketsOnly: true`.
- **Config Schema Additions (`pkg/config/config.go`):**
  - `synthesizer.target` accepts `"alloy"`, `"otel"`, `"promrw"`, `"prometheus-remote-write"`.
  - Optional `synthesizer.promRW` block (`mode`, `remoteWriteName`, `remoteWriteURL`, `operatorKind`, `operatorName`, `operatorNamespace`, `maxTermsPerRule`).
- **CLI, Report & Demo:**
  - `trimtab synth --target promrw` / `trimtab analyze --target promrw`.
  - Report footer label `"Prometheus remote_write rules"` in `pkg/report/report.go`.
  - Demo config `examples/demo/trimtab-promrw.yaml`, human-owned insertion example `examples/demo/prometheus-remote-write.yaml.example`, `make demo-promrw`, and `Tiltfile` `synth-promrw-preview`.
- **Verification & Exit Criteria:**
  - Byte-for-byte Alloy golden test parity after extracting `pkg/synthesizer/relabel.go`.
  - 5 `promrw` golden tests (`normal`, `noop`, `bucket_only`, `remote_write`, `operator_patch`).
  - Real `github.com/prometheus/prometheus/model/relabel` validation (`cfg.Validate(model.UTF8Validation)`) and runtime `relabel.ProcessBuilder(...)` execution tests proving family drops, bucket-only drops, summary drops, dotted UTF-8 names, and multi-rule chunking behave accurately.
  - CLI E2E test + `make verify && make demo && make demo-otel && make demo-promrw` green.

---

### P2.2 — Critical Infrastructure & Ruler Usage Sources *(Next Build Target)*

- **Goal:** Close the highest-severity static visibility gaps called out in `DESIGN §3.2`: Mimir/Thanos Ruler APIs and Kubernetes autoscaling/progressive-delivery consumers whose metrics must never be mistakenly flagged unused if query logs have gaps or during cold start.
- **Packages & Key Types:**
  - `pkg/metrics/types.go`: add `UsageOrigin` constants (`OriginRulerAlert`, `OriginKEDA`, `OriginPromAdapter`, `OriginArgoRollouts`, `OriginFlagger`); all return `true` from `IsLiveSink()`.
  - `pkg/usage/ruler/`: HTTP scanner supporting:
    - Cortex/Mimir Ruler (`/prometheus/config/v1/rules`, optional `X-Scope-OrgID` header, multi-namespace YAML rule-group response).
    - Prometheus/Thanos Ruler (`/api/v1/rules`, JSON groups response).
    - Emits `AlertRefs` (`OriginRulerAlert`) and `RecordingRules` (`promrules.RecordingRule`, fed directly into existing `engine.ResolveRecordingRuleDAG`).
  - `pkg/usage/k8sconsumers/`: multi-document YAML glob/directory scanner (plus a small `ManifestSource` interface so P2.6's operator can feed live cluster objects into the same parser) covering:
    - **KEDA** `ScaledObject` / `ScaledJob` (`keda.sh/v1alpha1`): extracts `spec.triggers[type=prometheus].metadata.query` (sanitizing `{{...}}` template tokens before `promql.Extract`).
    - **`prometheus-adapter`** ConfigMap (`rules.yaml` / `adapter-config.yaml`): extracts `seriesQuery`, `resources.overrides`, and `metricsQuery` (substituting `<<.Series>>`, `<<.LabelMatchers>>`, `<<.GroupBy>>` safely before `promql.Extract`).
    - **Argo Rollouts** `AnalysisTemplate` / `ClusterAnalysisTemplate` (`argoproj.io/v1alpha1`): extracts `spec.metrics[].provider.prometheus.query` (substituting `{{args.*}}` placeholders).
    - **Flagger** `MetricTemplate` / `Canary` (`flagger.app/v1beta1`): extracts `spec.query` (substituting `{{ namespace }}`, `{{ target }}`, `{{ interval }}`).
- **Config & State Additions:**
  - `usage.rulers[]`: `{ type: "mimir"|"prometheus"|"thanos", url, tenantID, bearerToken, headers, timeout }`.
  - `usage.k8sConsumers`: `{ globs: []string, enableKEDA: *bool, enablePromAdapter: *bool, enableArgoRollouts: *bool, enableFlagger: *bool }`.
  - `state.BaselineCounts` + `engine.SourceCounts`: add `*int` fields `RulerRules`, `KEDATriggers`, `PromAdapterRules`, `RolloutTemplates`, `FlaggerTemplates` checked by `engine.CheckBaselineDrift` (Invariant #4).
- **Verification & Exit Criteria:**
  - `httptest.NewServer` tests for Mimir YAML and Prometheus/Thanos JSON ruler APIs (including HTTP error fail-closed & baseline drift abort).
  - Fixture tests for KEDA, `prometheus-adapter`, Argo Rollouts, and Flagger verifying PromQL metric names + labels (`by`/`without`/matchers) are marked live-used.
  - Safety Matrix regression check confirming static consumers alone without query logs still keep Trimtab in Report-Only mode (Invariant #1).

---

### P2.3 — Observability Ecosystem Static Consumers

- **Goal:** Protect labels needed for alert routing/templating and ingest usage from Perses dashboards and Sloth/Pyrra SLO definitions.
- **Packages & Key Types:**
  - `pkg/usage/alertmanager/`: parses Alertmanager YAML (`route` tree including nested `routes`, `match`, `match_re`, `matchers` string syntax `foo=~"bar"`, `group_by`, `inhibit_rules`, and receiver Go templates `.CommonLabels.<lbl>` / `.GroupLabels.<lbl>`).
    - **Semantics note:** Alertmanager does not reference metric names; it references **alert labels** carried forward from alerting rules. `engine.Analyze` merges Alertmanager-referenced labels onto every metric family referenced by alerting rules (`OriginPromAlert`, `OriginGrafanaAlert`, `OriginRulerAlert`) so `LabelUsage` never marks routing/grouping labels unused (Invariant #8).
  - `pkg/usage/perses/`: scans local Perses dashboard JSON/YAML directories and Perses REST API (`/api/v1/projects`, `/api/v1/projects/{project}/dashboards`), extracting `PrometheusTimeSeriesQuery` PromQL + `seriesNameFormat` legend templates and `PrometheusLabelValuesVariable` / `PrometheusPromQLVariable` definitions into `OriginPersesDashboard` (`IsLiveSink() == true`).
  - `pkg/usage/slo/`: scans Sloth (`sloth.slok.dev/v1` `PrometheusServiceLevel`, `rawInterval`/`window` Go-template substitution) and Pyrra (`pyrra.dev/v1alpha1` `ServiceLevelObjective`, `indicator.ratio`/`latency`/`bool_gauge`) YAML specs into `OriginSLO` (`IsLiveSink() == true`).
- **Config & State Additions:**
  - `usage.alertmanager`: `{ configPaths: []string, url, bearerToken, timeout }`.
  - `usage.perses`: `{ url, apiKey, headers, dashboardsDir, projects: []string, timeout }`.
  - `usage.slo`: `{ globs: []string }`.
  - `state.BaselineCounts` + `engine.SourceCounts`: add `AlertmanagerMatchers`, `PersesDashboards`, `SLOs`.
- **Verification & Exit Criteria:**
  - Alertmanager test proving a label appearing ONLY in `group_by` or `route.matchers` is protected on alerted metrics and omitted from `LabelCollapseAdvisories`.
  - Perses local-dir + `httptest.NewServer` API tests + wildcard protector test for catch-all Perses dashboards.
  - Sloth & Pyrra fixture tests verifying SLI error/total/latency bucket metrics and grouping labels are protected.

---

### P2.4 — Per-Team Attribution & "OpenCost for Telemetry" FinOps Exporter

- **Goal:** Answer *"which team owns these active/droppable series and how much do they cost?"*, evaluate per-team budgets, and expose OpenCost-compatible Prometheus metrics (`DESIGN §2, §8`).
- **Packages & Key Types:**
  - `pkg/attribution/`:
    - `TeamMapping`, `CostModel`, `TeamSummary`, `BudgetStatus`, `AttributionReport`.
    - **Series-weighted attribution:** Using Prometheus instant query `sum by (<attribution_labels>) (__name__=~"...")` (bounded/batched, with offline fixture fallback via `SampleLabelSets` in `SeriesStats`) to apportion each family's active and droppable series accurately across teams instead of assigning an entire shared metric to one team.
    - **Deterministic Resolver Chain** (first match wins):
      1. Metric label rule (ordered list e.g. `team`, `owner`, `app_team` with optional regex map).
      2. Kubernetes `namespace` / workload (`service`, `app_kubernetes_io_name`) static mapping or regex rule.
      3. Backstage `catalog-info.yaml` loader (`metadata.name` / `metadata.annotations["prometheus.io/..."]` -> `spec.owner`, stripping `group:`/`team:` prefixes deterministically).
      4. Fallback bucket: `"unattributed"` (explicitly reported so platform teams see un-tagged telemetry).
    - **Budget Evaluation:** Evaluates configured `Budgets` (`team`, `maxActiveSeries`, `maxMonthlyCostUSD`, `warnThresholdPercent`) -> `WithinBudget`, `Warning`, `Exceeded`.
    - **Pricing:** `costPerThousandSeriesMonthlyUSD` (default `0`: dollar metrics/columns only render when pricing > 0 so Trimtab never fabricates vendor billing rates).
  - `pkg/metrics/types.go`: add optional `Attribution *AttributionReport` field on `AuditReport` (rendered in `pkg/report` table/markdown/json when present).
  - `pkg/exporter/`: `Server` wrapping `prometheus/client_golang` (`prometheus.NewRegistry()`, avoiding global registry pollution) exposed via `trimtab serve --config ... --listen :9190 --interval 15m` with `/metrics`, `/healthz`, `/readyz` and background scrape loop (strictly out of the data path).
    - Exported metrics:
      - `trimtab_team_active_series{team,tenant}`
      - `trimtab_team_droppable_series{team,tenant}`
      - `trimtab_team_synthesized_drop_series{team,tenant}`
      - `trimtab_team_monthly_cost_dollars{team,tenant}`
      - `trimtab_team_potential_monthly_savings_dollars{team,tenant}`
      - `trimtab_team_budget_utilization_ratio{team,tenant}`
      - `trimtab_query_log_coverage_days{tenant}`
      - `trimtab_safety_gate_blocked{tenant,reason}`
      - `trimtab_active_tranche{tenant}`
      - `trimtab_regret_events_total{tenant}`
- **Verification & Exit Criteria:**
  - Resolver chain unit tests (priority precedence, Backstage YAML parsing, `"unattributed"` fallback, budget warning/exceeded thresholds).
  - `httptest` scrape test parsing Prometheus text exposition output from `pkg/exporter` via `expfmt` / `dto` and asserting all gauge/counter values match `AuditReport`.
  - Clean `trimtab serve` shutdown on `SIGINT`/`SIGTERM`.

---

### P2.5 — GitOps PR Actuator

- **Goal:** Automate safe, auditable GitOps pull/merge requests for tranched drops, dead recording-rule cleanup, and urgent Regret Reverts (`DESIGN §2, §3.10, §5`).
- **Packages & Key Types:**
  - `pkg/actuator/actuator.go`: signal-agnostic interface (`DESIGN §5`):
    ```go
    type ActuationRequest struct {
        Report          *metrics.AuditReport
        Artifact        *synthesizer.GeneratedArtifact
        RuleEdits       []RuleDeletion
        StateFileUpdate []byte
    }
    type ActuationResult struct {
        Mode       string
        PRURL      string
        PRNumber   int
        Branch     string
        ActionType string // "drop_tranche", "regret_revert", "dead_rule_cleanup", "noop"
    }
    type Actuator interface {
        Name() string
        Actuate(ctx context.Context, req ActuationRequest) (*ActuationResult, error)
    }
    ```
  - `pkg/actuator/gitops/`:
    - `GitProvider` interface (`EnsureBranch`, `GetFile`, `UpsertFiles`, `OpenOrUpdatePR`) with three implementations:
      1. `github` (GitHub REST API v3; token via env var e.g. `GITHUB_TOKEN`, never stored in plaintext config).
      2. `gitlab` (GitLab REST API v4; token via env var e.g. `GITLAB_TOKEN`).
      3. `local` / `dryrun` (writes unified diffs & PR markdown preview to disk/stdout for offline testing & CI).
    - **Workflow 1 — Regret Revert PR (Highest Priority):** When `len(Report.Regrets) > 0`, immediately opens/updates `trimtab/regret-revert-<name>` titled `[URGENT][Trimtab] Revert drop for <families> (regret detected)`, committing the re-synthesized artifact (regretted metrics removed) + updated state ledger (or `neverDrop` override), with the exact triggering `api-user` query, timestamp, and hit count in the PR body.
    - **Workflow 2 — Tranched Drop PR:** When `!Report.Safety.ReportOnly && !Report.Safety.Blocked` and `Artifact.DroppedFamilies > 0`, opens/updates `trimtab/tranche-<N>-<name>` writing `Artifact.Content` to the target git path and setting PR body from `report.Render(rep, report.FormatMarkdown)`. Refuses to open drop PRs in Report-Only or Blocked states.
    - **Workflow 3 — Dead Recording-Rule Cleanup PR (Opt-in):** When `actuator.cleanupDeadRules: true`, parses target rule YAML files into `gopkg.in/yaml.v3` `yaml.Node` trees and removes only exact `(group, ruleName)` nodes identified in `Report.DeadRecordingRules`, preserving surrounding comments, anchors, and formatting, and highlighting any `HasOrVectorZero` rules in the PR description.
- **CLI Additions (`cmd/trimtab/main.go`):**
  - `trimtab actuate --config trimtab.yaml [--dry-run]`: runs `Analyze` + `Synthesize` + `Actuator.Actuate`.
- **Verification & Exit Criteria:**
  - `httptest.NewServer` mock tests for GitHub and GitLab providers (idempotent branch/commit/PR update, auth headers, non-2xx error handling).
  - YAML `yaml.Node` comment-preserving deletion tests (single rule deleted from multi-rule group without mangling sibling rules/comments; empty group safely handled).
  - Regret Revert PR integration test verifying `[URGENT]` PR fires even if tranche rollout is blocked.

---

### P2.6 — Kubernetes Operator & CRDs

- **Goal:** Provide Kubernetes-native lifecycle management, scheduled audits, persistent in-cluster ledger storage, `TelemetryBudget` status enforcement, and emergency `breakGlassUntil` overrides (`DESIGN §8`).
- **Packages & Layout:**
  - `api/v1alpha1/`: `groupversion_info.go` (`trimtab.io/v1alpha1`) and CRD Go types:
    - `TelemetryAuditPolicy`: `spec` mirrors `config.Config` (`inventory`, `usage`, `policy`, `synthesizer`, `attribution`, `actuator`, `scheduleInterval`, `ledgerRef`), plus `inClusterDiscovery` toggles for `ScaledObject`, `AnalysisTemplate`, `PrometheusRule`; `status` tracks `lastAuditTime`, `queryLogCoveredDays`, `activeTranche`, `totalTranches`, `safetyBlocked`, `conditions`.
    - `TelemetryBudget`: `spec` (`team`, `policyRef`, `maxActiveSeries`, `maxMonthlyCostUSD`, `warnThresholdPercent`); `status` (`activeSeries`, `droppableSeries`, `monthlyCostUSD`, `utilizationPercent`, `state: WithinBudget|Warning|Exceeded`, `conditions`).
    - `TelemetryRecommendation`: materialized by the controller per policy (`spec.approvalMode: auto|manual`, `spec.approvedTranche`, `spec.breakGlassUntil`, `spec.acknowledgedBaselineShift`); `status` mirrors `ActiveDrops`, `ProjectedDrops`, `DeadRecordingRules`, `LabelCollapseAdvisories`, `Regrets`, and synthesized ConfigMap/Secret reference.
  - `internal/controller/`: `controller-runtime` reconcilers for all 3 CRDs + ConfigMap/PVC `StateLedger` backend (`pkg/state` `Store` interface implemented for file, ConfigMap, and in-memory).
  - `cmd/trimtab-operator/`: separate operator binary entrypoint so `cmd/trimtab` CLI stays fast and dependency-light for non-K8s users; includes `/metrics` (merged with `pkg/exporter`), `/healthz`, `/readyz`, and leader election.
  - `deploy/operator/`: generated CRD YAML (`controller-gen`), RBAC (`ClusterRole` read-only on `ScaledObject`/`PrometheusRule`/`AnalysisTemplate`/`Canary` + read/write on Trimtab CRDs and output ConfigMaps), and `Deployment`.
  - `Tiltfile` & `scripts/local-k8s.sh`: add optional operator dev deployment profile in Kind.
- **Safety Guarantees:**
  - **`breakGlassUntil`:** When `now < recommendation.Spec.BreakGlassUntil`, the controller immediately sets `ReportOnly`/override mode and writes a zero-drop pass-through/no-op artifact to the output ConfigMap (and triggers Regret/PR if GitOps enabled), **without** permanently polluting `neverDrop`; once `BreakGlassUntil` expires, normal state resumes.
- **Verification & Exit Criteria:**
  - `envtest` (or mocked `sigs.k8s.io/controller-runtime/pkg/client/fake`) reconciler tests for: initial Report-Only cold-start status, tranche advancement after `soakWindow`, manual approval gate, `breakGlassUntil` immediate pass-through, and `TelemetryBudget` status transitions.
  - Kind cluster smoke test applying sample CRs and verifying generated ConfigMap artifact.

---

### P2.7 — Multi-Cluster & Per-Selector Drop Scoping

- **Goal:** Allow Trimtab to drop a metric only in specific clusters/environments (e.g., drop `debug_cache_hits_total` in `cluster="staging"` while keeping it in `cluster="prod"`) without breaking single-tenant safety guarantees.
- **Packages & Key Types:**
  - `pkg/scope/`:
    - `ScopeKey` (canonical sorted `label=value` tuple, e.g., `cluster=staging,env=dev`, or `""` for global scope).
    - `ExtractQueryScopes(expr parser.Expr, scopeLabels []string)`: walks PromQL `VectorSelector` matchers (`=`, `!=`, `=~`, `!~`) for configured `policy.scopeLabels`.
    - **Global Query Safety Invariant (Hard Requirement):** If a live query, dashboard, alert, or K8s consumer omits a matcher on a `scopeLabel` (an **unscoped query**), or uses a regex matcher that cannot be bounded, it MUST protect the metric family across **ALL scopes** (never drop in any scope on an unverified unscoped query).
  - `pkg/state/ledger.go`: bump `CurrentSchemaVersion = 2` with automatic, lossless migration from v1 (`SeenFamilies` / `ActiveDrops` migrate under global scope `""` on `state.Load`).
  - `pkg/engine/`: scope-aware `UsageIndex` + per-(family, scope) tranche planning and regret detection. If an unscoped `api-user` query hits a metric dropped in *any* scope, Regret fires and restores all dropped scopes for that family.
  - `pkg/synthesizer/` (`alloy`, `promrw`, `otel`):
    - **Alloy & Prometheus `remote_write`:** Emit multi-label relabel rules when drops are scoped, e.g. `source_labels = ["__name__", "cluster"]`, `separator = ";"`, `regex = "(metric_a|metric_b);staging"`, `action = "drop"`.
    - **OTel Collector:** Emit scoped OTTL predicates (`'name == "metric_a" and resource.attributes["k8s.cluster.name"] == "staging"'`) **ONLY** when an explicit `synthesizer.otel.scopeAttributeMap` (`cluster -> resource.attributes["..."]` or `attributes["..."]`) is configured; otherwise refuse scoped OTel drop with a capability advisory warning (preserving Invariant #7's zero-guessing rule).
- **Verification & Exit Criteria:**
  - Safety Matrix tests for: (1) scoped query protects only `prod` and allows dropping `staging`, (2) unscoped query protects BOTH `prod` and `staging`, (3) regex matcher `{cluster=~"prod-.*"}` protects matching scopes conservatively, (4) v1 -> v2 ledger migration preserves `first_seen_at`, `active_drops`, and `pinned_never_drop`, (5) real Prometheus `relabel.ProcessBuilder` test confirming `source_labels: [__name__, cluster]` drops `metric_a` in `staging` while keeping `metric_a` in `prod`.

---

## 6. Phase 3 — Shard-Safe Aggregation, Multi-Signal & OpAMP *(Future Roadmap)*

*(Refine each sub-phase here in `docs/PLAN.md` before building; never create a separate `PHASE3_PLAN.md`.)*

### P3.1 — Shard-Safe Aggregation Synthesizer (`pkg/synthesizer/aggregate/`)

- Graduate `DROP_DIMENSION` from advisory-only (`DESIGN §3.9`) to safe synthesized in-flight aggregation rules when—and **only** when—collector topology & shard identity (`node`, `replica`, or `collector_id` label preservation) are verified so cross-node collisions and duplicate-sample rejections cannot occur.
- Supports partial-sum / pre-aggregation targets where collector architecture permits, while keeping `le`, `quantile`, and `__name__` hard-protected.

### P3.2 — Direct Alloy Config Import Add-on (`pkg/discovery/alloy/`)

- Optional parser for existing Grafana AlloyRiver/HCL configs (`DESIGN §7.4`) to discover upstream/downstream pipeline topology, DaemonSet vs. Deployment sharding (feeding P3.1), and existing manual `prometheus.relabel` drop rules (for deduplication).

### P3.3 — Logs Signal Engine (`pkg/logs/`)

- Idiomatic Logs domain engine (`DESIGN §5, §8`): reads Loki index/volume stats and LogQL query-log patterns to identify high-volume noisy streams (e.g., unqueried `debug`/`health` logs) and synthesize drop or cold-tier object-storage routing rules, reusing `pkg/state`, `pkg/actuator`, and `pkg/report`.

### P3.4 — Traces Signal Engine (`pkg/traces/`)

- Idiomatic Traces domain engine (`DESIGN §5, §8`): audits span volume by service/operation/status against trace queries and synthesizes OTel tail-sampling policies (`probabilistic` / `status_code` / `latency`) without distorting the metrics engine.

### P3.5 — OpAMP Break-Glass Actuator (`pkg/actuator/opamp/`)

- Optional OpAMP control-plane actuator (`DESIGN §8`) enabling instant temporary overrides during SEV-1 incidents across fleet-managed OTel Collectors and Alloy agents alongside GitOps PR reconciliation.
