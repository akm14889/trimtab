# Trimtab E2E Findings (`v0.0.1-beta.3`)

Findings discovered by running Trimtab `v0.0.1-beta.3` (commit `c0474bc`, product code unmodified) inside a real Kubernetes (`kind`) LGTM stack (`Mimir 2.17.11`, `Loki 3.7.10`, `Grafana 12.3.1`, `Alloy v1.22.0`).

Ranked by severity: **anything that can cause an unsafe drop comes first**.

---

## F1 — CRITICAL (Unsafe Drop): Unquoted Single-Token PromQL (`param_query=<metric>`) Is Silently Dropped in Mimir Logfmt Parsing

- **Affected Code:** [`pkg/usage/querylog/loki.go`](../../pkg/usage/querylog/loki.go#L32-L34), `ingestMimirLogfmtLine` (lines 190–208)
- **Impact:** Any simple PromQL query containing a bare metric name or character sequence without whitespace (`e.g. e2e_autoscaler_metric` or `count(e2e:rr_live:rate5m)`, commonly used by KEDA `ScaledObject`s, autoscalers, and health scripts) is logged **unquoted** by Mimir's go-kit `logfmt` logger (`param_query=e2e_autoscaler_metric`). Because `logfmtQuotedQueryRe` requires surrounding double quotes (`param_query="..."`), Trimtab skips the line completely. After `minObservationWindow` passes, the actively queried metric is classified as unused (`0 live consumers`) and synthesized for `DROP_FAMILY`.
- **Empirical Mimir Log Line (Loki `{namespace="monitoring", component="query-frontend"}`):**
  ```text
  ts=2026-11-25T18:00:42.135061902Z caller=handler.go:430 level=info user=e2e msg="query stats" component=query-frontend method=GET path=/prometheus/api/v1/query route_name=prometheus_api_v1_query user_agent=autoscaler/keda-standin-1.0 status_code=200 response_time=442.468µs response_size_bytes=4457 query_wall_time_seconds=0.000256558 fetched_series_count=15 ... param_query=e2e_autoscaler_metric ... header_user_agent=autoscaler/keda-standin-1.0 header_x_scope_orgid=e2e status=success
  ```
- **Minimal Reproduction:**
  1. Run `autoscaler/keda-standin-1.0` querying `GET /prometheus/api/v1/query?query=e2e_autoscaler_metric` every 20s against Mimir.
  2. Wait for `minObservationWindow: 20m` to pass and run `trimtab analyze --config /etc/trimtab/trimtab.yaml --format json`.
- **Observed Report JSON Excerpt (`20261125T193010Z/S2/report.json`):**
  ```json
  {
    "familyName": "e2e_autoscaler_metric",
    "metricType": "gauge",
    "action": "DROP_FAMILY",
    "confidence": "HIGH",
    "series": 15,
    "reason": "0 live consumers (observed 0d)",
    "observedDays": 0,
    "trancheNumber": 2,
    "synthesized": false,
    "members": ["e2e_autoscaler_metric"],
    "namingMode": "prometheus_receiver"
  }
  ```
  And after Tranche 2 activation (`20261125T193010Z/S7/ledger-after.json`), `e2e_autoscaler_metric` was added to `active_drops` and synthesized into `trimtab.alloy`.
- **Why Unit Tests Missed It:** All mock Mimir logfmt unit fixtures wrapped `param_query="..."` in quotes; Mimir's real `go-kit/log/logfmt` encoder omits quotes whenever a value has no whitespace, quotes, or `=`.

---

## F2 — HIGH (Safety Gate Bypass): Cold-Start Coverage Uses UTC Calendar Days, Bypassing Sub-Day and `1d` Observation Windows

- **Affected Code:** [`pkg/state/ledger.go`](../../pkg/state/ledger.go#L258-L282) (`UpdateQueryLogCoverage`), [`pkg/engine/analyzer.go`](../../pkg/engine/analyzer.go#L197)
- **Impact:** `UpdateQueryLogCoverage` counts distinct UTC calendar dates (`YYYY-MM-DD`) inside `CoveredDates` that have `cnt > 0`. On minute 0 of deployment (with `< 1 minute` of query logs), `CoveredDates` has `1` entry (`"2026-11-25": 83`), so `query_log_coverage.covered_days` immediately becomes `1.0`.
  - For any sub-day `minObservationWindow` (`20m` in E2E: `0.01389` days), `1.0 >= 0.01389` clears the cold-start gate immediately on the first run (`reportOnly: false`, `confidence: HIGH`) instead of waiting for `20m` of query-log coverage.
  - Worse: even with the chart's non-prod profile [`charts/trimtab/examples/values-nonprod.yaml`](../../charts/trimtab/examples/values-nonprod.yaml) (`minObservationWindow: 1d`), `covered_days = 1.0 >= 1.0` clears the query-log gate after **one single query-log line on day 1** (relying solely on `f.ObservedDuration < minObsDur` per family rather than query-log coverage!).
- **Minimal Reproduction:**
  1. Wipe `.trimtab-state.json`, seed fresh workloads + Loki logs, and immediately run `trimtab analyze --format json` (Scenario S1).
- **Observed Report JSON Excerpt (`20261125T193010Z/S1/report.json`):**
  ```json
  "safety": {
    "reportOnly": false,
    "blocked": false,
    "queryLogConfigured": true,
    "queryLogCoveredDays": 1,
    "requiredWindowDays": 0.013888888888888888,
    "apiUserQueries": 83,
    "excludedQueries": 2
  },
  "summary": {
    "confidence": "HIGH",
    "protectedFamilies": 218
  },
  "projectedDrops": null
  ```
- **Secondary Related Finding:** Even when families have not yet met `minObservationWindow`, `reportOnly: true` vs `false` only checks `CoveredDays` and `projectedDrops` remains `null` (`0` items) because families with `ObservedDuration < minObsDur` are returned to `protectedReasons` before candidate collection ([`pkg/engine/analyzer.go#L408-L412`](../../pkg/engine/analyzer.go#L408-L412)). Operators testing cold start see `0` projected savings until the full observation window finishes.

---

## F3 — HIGH (Unsafe Self-Query Feedback Loop & Wildcard Exclusion Failure): Mimir Logfmt Field Mapping Gaps

Three compounding issues exist in [`pkg/usage/querylog/loki.go`](../../pkg/usage/querylog/loki.go) when ingesting real Mimir `query-frontend` logfmt lines:

### F3a. Mimir `slow query detected` omits `user_agent=`, breaking self-query exclusion (`Trimtab` marks the entire cluster live!)
- **Affected Code:** [`pkg/usage/querylog/loki.go#L36,L215-L234`](../../pkg/usage/querylog/loki.go#L215-L234)
- **Trigger:** When Mimir's `frontend.log_queries_longer_than` is set to a small non-zero value (e.g. `1ns` or `10ms`), Mimir emits **two** log lines per query:
  1. `caller=handler.go:430 msg="query stats" ... user_agent=trimtab/v0.0.1-beta.3-e2e ... header_user_agent=trimtab/v0.0.1-beta.3-e2e`
  2. `caller=handler.go:315 msg="slow query detected" ... header_user_agent=trimtab/v0.0.1-beta.3-e2e` (**no `user_agent=` field!**)
- **Observed Failure (`20261125T192541Z/S2`):** Because Mimir does not expose `/api/v1/status/tsdb` (returns 404), Trimtab runs batched fallback queries `count by (__name__) ({__name__=~"..."})`. Every `"slow query detected"` log line generated by Trimtab's own inventory or `without`-collapse queries lacked `user_agent=`, bypassed `isSelfTrimtabCaller`, and was recorded as `OriginAPIUser` — updating `last_queried_at` on `e2e_unused_big_gauge` (`19:24:01Z`) and `e2e_unused_legacy_gauge`, poisoning all 218 families as `"actively consumed"` (`droppableFamilies: 0`). In active-drop production clusters, slow Trimtab collapse/probe queries against recently active metrics or wildcard fallbacks would also trigger false Regrets.
- **Sample Log Line:**
  ```text
  ts=2026-11-25T19:24:01.829320216Z caller=handler.go:315 level=info user=e2e msg="slow query detected" method=GET host=mimir.monitoring.svc.cluster.local:8080 path=/prometheus/api/v1/query time_taken=3.146871ms param_query="count(count without (shard) (e2e_unused_big_gauge))" header_cache_control= header_user_agent=trimtab/v0.0.1-beta.3-e2e header_x_scope_orgid=e2e
  ```
- **Workaround in E2E:** Setting `log_queries_longer_than: 0s` alongside `query_stats_enabled: true` suppresses `"slow query detected"` while still emitting `"query stats"` with `user_agent=`.

### F3b. `caller=handler.go:430` shadows `user_agent=...`, breaking `query_caller` and `excludeRegretCallers` on Mimir
- **Affected Code:** [`pkg/usage/querylog/loki.go#L35,L210-L232`](../../pkg/usage/querylog/loki.go#L210-L232)
- **Impact:** Mimir's go-kit logger writes `caller=handler.go:430` (Go source location) on every line. `logfmtCallerRe` extracts `"handler.go:430"` into `caller`, so `if caller == "" && userAgent != ""` never triggers, and `matcher.isExcluded(caller, "", "", query)` only checks `caller` (never `userAgent`). Therefore:
  - `policy.excludeSources: [{ type: query_caller, value: explorer }]` fails completely on Mimir logs.
  - `policy.excludeRegretCallers` is likewise non-functional against Mimir `user_agent`.
- **Minimal Reproduction (`20261125T193010Z/S4/report-caller-only.json`):** Configure `excludeSources` with `query_caller: explorer` (without `query_regex`):
  ```json
  "wildcardSources": [
    {
      "sourceID": "querylog:expr:count by (__name__) ({__name__=~\".+\"})",
      "origin": "api-user",
      "matchedFamilies": 218,
      "matchPercent": 100,
      "excluded": false
    }
  ],
  "bigProtected": "actively consumed"
  ```
  All 218 families become `"actively consumed"` (`droppableFamilies: 0`).

### F3c. `header_x_dashboard_uid=...` is ignored in Mimir logfmt, so `dashboard_uid` exclusions do not suppress live Grafana queries
- **Affected Code:** [`pkg/usage/querylog/loki.go#L190-L235`](../../pkg/usage/querylog/loki.go#L190-L235)
- **Impact:** When a human opens an excluded Grafana dashboard (`dashboard_uid: e2e-cardinality-explorer`), Grafana forwards `X-Dashboard-UID: e2e-cardinality-explorer` and Mimir logs `header_x_dashboard_uid=e2e-cardinality-explorer` (when `log_query_request_headers` includes `X-Dashboard-UID`). `ingestMimirLogfmtLine` passes `""` for `dashboardUID` to both `matcher.isExcluded` and `recordEntry`, so the live query in Loki is NOT excluded and marks the entire estate live for 20m (`20261125T184039Z/S4`).
- **Sample Log Line (`19:01:43Z`):**
  ```text
  ts=2026-11-25T19:01:43.124225382Z caller=handler.go:430 level=info user=e2e msg="query stats" ... user_agent=Grafana/12.3.1 ... param_query="topk(20, count by (__name__) ({__name__=~\".+\"}))" ... header_user_agent=Grafana/12.3.1 header_x_dashboard_uid=e2e-cardinality-explorer header_x_grafana_user=admin header_x_scope_orgid=e2e status=success
  ```

---

## F4 — MEDIUM (Lifecycle / Alerting): `trimtab analyze` Exits `0` on Regret & Lacks a Post-Revert Regret Resolution Flow

- **Affected Code:** [`cmd/trimtab/main.go#L332-L335`](../../cmd/trimtab/main.go#L332-L335), [`pkg/state/ledger.go#L334-L343`](../../pkg/state/ledger.go#L334-L343), [`pkg/engine/analyzer.go#L543-L548`](../../pkg/engine/analyzer.go#L543-L548)
- **Impact:**
  1. In Scenario S8 (`20261125T193010Z/S8`), when `RegretEvent` fired for `e2e_regret_victim_gauge`, `trimtab analyze` printed the regret in the report and pinned it to `pinned_never_drop`, **but exited with code `0`** (`main.go` only returns `1` on `len(hardErrors) > 0`). CronJobs / CI automation watching exit codes get no failure signal.
  2. The ledger records `tranches[-1].status = "blocked_regret"` and `unresolved_regrets: 1`, with **no `revert_pending` state** tracking whether the re-synthesized artifact removing the dropped family has actually been applied to the collector, and **no CLI mechanism or automatic inventory check** to clear `blocked_regret` once the reverted series (`e2e_regret_victim_gauge`) reappears in Mimir. Even after applying the reverted `trimtab.alloy` and confirming series returned (`20261125T193010Z/S8/report-post-revert.json`), the state remained permanently stuck at `activeTrancheStatus: "blocked_regret"`, `unresolvedRegrets: 1`.

---

## F5 — MEDIUM (Circuit Breaker False-Positive): Narrowing `loki.window` Below `minObservationWindow` Trips Query-Log Baseline Drift

- **Affected Code:** [`pkg/engine/analyzer.go#L728-L754`](../../pkg/engine/analyzer.go#L728-L754), [`pkg/engine/safety.go#L73-L86`](../../pkg/engine/safety.go#L73-L86)
- **Impact:** `collectSourceCounts` computes `QueryLogDailyRate` by dividing `in.QueryLog.TotalEntries` returned in `loki.window` by the count of UTC days in `ObservedDates`. When `loki.window` is set shorter than `minObservationWindow` (Scenario S9: `20m -> 5m` on a single UTC day), query volume drops proportionally (~75%–95%) within the same 1 UTC day divisor, tripping `baseline drift guard: query_log_daily_rate dropped ...` (`baselineDriftDetected: true`, `blocked: true`). Although `e2e_rotation_gauge` stays protected in `protectedReasons` via `ledger.seen_families[].last_queried_at`, drop synthesis is blocked until `--accept-baseline-shift` resets the rate baseline.

---

## F6 — MEDIUM (Helm / Kubernetes UX): `helm upgrade --install --wait` Blocks Indefinitely on `WaitForFirstConsumer` PVCs

- **Affected Chart Template:** [`charts/trimtab/templates/pvc.yaml`](../../charts/trimtab/templates/pvc.yaml), [`charts/trimtab/templates/cronjob.yaml`](../../charts/trimtab/templates/cronjob.yaml)
- **Impact:** In Kind (`rancher.io/local-path` StorageClass) and managed clouds using `volumeBindingMode: WaitForFirstConsumer` (e.g., AWS `ebs-sc`, GKE `standard-rwo`), PVCs remain `Pending` until a Pod mounts them. Because the Helm chart only installs a `CronJob` (whose Pod does not start until scheduled or manually triggered via `kubectl create job --from=cronjob/trimtab`), running `helm upgrade --install trimtab ./charts/trimtab --wait` (especially under Helm v4, tested here with `v4.3.0`) stalls on `PersistentVolumeClaim/trimtab-state` (`WaitForFirstConsumer`) until `--timeout` fails (`context deadline exceeded` / `pending-install`).
- **Observed Event:**
  ```text
  LAST SEEN   TYPE     REASON                 OBJECT                                MESSAGE
  9s          Normal   WaitForFirstConsumer   persistentvolumeclaim/trimtab-state   waiting for first consumer to be created before binding
  ```

---

## F7 — LOW (Docs vs. Code & Backend Behavior Discrepancies)

1. **`docs/DESIGN.md §3.2` Mimir query-logging flag semantics:**
   - **Doc claim ([`docs/DESIGN.md#L64`](../../docs/DESIGN.md#L64)):** *"Mimir's `query-frontend` logs all API queries when `log_queries_longer_than: 0s`"*
   - **Observed Mimir `2.17.11` behavior:** `log_queries_longer_than: 0s` *disables* slow-query logging (`caller=handler.go:315 msg="slow query detected"`). What actually logs every query with `param_query=...` and `user_agent=...` is `frontend.query_stats_enabled: true` (`caller=handler.go:430 msg="query stats"`). Ironically, enabling `log_queries_longer_than` triggers the F3a bug above.
2. **Mimir `/api/v1/status/tsdb` returns HTTP 404:**
   - `pkg/inventory/prometheus/client.go` calls `GET /api/v1/status/tsdb?limit=10000`, which Mimir `2.17.11` does not implement (`HTTP 404: 404 page not found`). Trimtab handles this cleanly by emitting a non-fatal warning and falling back to batched `count by (__name__)`, accurately counting all families (`4,825` series across `218` families).
3. **Helm `helm test` works with Grafana `Viewer` Role, but Mimir Ruler reads empty-tenant state cleanly:**
   - Contrary to concern that Grafana `Viewer` service-account tokens might 403 on `/api/v1/provisioning/alert-rules`, Grafana `12.3.1` allowed both `/api/search?type=dash-db&limit=1` and `/api/v1/provisioning/alert-rules` with a `Viewer` SA token during `S0` pre-seed and post-seed `helm test` checks.
4. **Prometheus `scrape` `pod` label collision (`exported_pod`):**
   - When an application exposes its own `pod="..."` label and Alloy's `discovery.relabel` also sets `target_label = "pod"`, Prometheus defaults to `honor_labels: false`, renaming the target label to `exported_pod` (`20261125T180104Z/S2` collapse advisory listed both `exported_pod` and `pod`).
5. **Zero-series historical metrics from `/api/v1/label/__name__/values` enter Drop Tranches:**
   - When a metric appeared briefly earlier in the `minObservationWindow` (`e.g. e2e_new_metric` replayed from Alloy WAL before reset, leaving `0` current active series), Mimir `/api/v1/label/__name__/values?start=...&end=...` still returns it while batched `count by (__name__)` omits it (`series: 0`). Trimtab (`20261125T201713Z/S2/report.json`) scheduled `e2e_new_metric` (`series: 0`) into `tranches[0]` with `reason: "0 live consumers (observed 0d)"`.
6. **Harness S12 Note in `20261125T201713Z`:**
   - Because `cmd_reset` originally restarted Alloy *after* Mimir rather than scaling Alloy to 0 first, Alloy's WAL replayed a prior `e2e_new_metric` sample into fresh Mimir at `20:17:25Z`, making its `first_seen_at` already `> 20m` old by `S12` (`20:39Z`). Fixed `cmd_reset` in `e2e.sh` to scale Alloy to 0 while flushing Mimir/Loki; on a clean `make e2e-all`, `e2e_new_metric` does not exist prior to S12.

