# Trimtab (`trimtab.io`)

> **Vendor-neutral, open-source adaptive telemetry control plane** written in Go.
> *Out of the data path. Fail-closed by default. Strictly complementary to Prometheus, Mimir, Thanos, OpenTelemetry Collector, Grafana Alloy, Grafana, OpenCost, and Argo CD / Flux.*

---

## What Trimtab Does

Trimtab audits real telemetry usage across **runtime query logs** (globbed Prometheus `query_log_file`, `prom-analytics-proxy`, and Loki-shipped query logs) + **static consumers** (Grafana dashboards & managed alerts, Prometheus YAML & Mimir/Thanos Ruler APIs, Alertmanager, Perses dashboards, Sloth/Pyrra/OpenSLO specs, KEDA, `prometheus-adapter`, Argo Rollouts, and Flagger), and safely rolls out tranched drop rules behind a one-time collector insertion point:

- **Metric-family modeling** — groups classic histograms (`_bucket`/`_sum`/`_count`/`_created`) and summaries as families so series are never orphaned; recognizes native histograms separately; negotiates target synthesizer capabilities (e.g., `_bucket`-only drops on Grafana Alloy vs. advisory-only on OTel Collector).
- **Mandatory runtime query-log gating & cold-start UX** — without query logs covering `>= minObservationWindow` (default `30d`), Trimtab stays in **Report-Only** mode showing projected savings.
- **Query-log origin classification** — splits `api-user`, `rule-eval`, and `excluded-explorer` so recording-rule evaluations never mask unused inputs.
- **Recording-rule transitive DAG** — traces `M -> R1 -> R2` chains backwards from live sinks; drops dead source metrics at the collector **and** lists dead recording rules (flagging `or vector(0)` rules) for deletion.
- **Safety circuit breakers** — Baseline Drift Guard (`>20%` drop in dashboards/rules/logs aborts synthesis), Tranched Rollout (`<=15%` series per tranche with `7d` soak window), Sanity Ceiling (`>80%` droppable aborts), default `neverDrop` meta-monitoring allowlist, and Wildcard-Protector detection/exclusion.
- **Honest `without`-based label-collapse advisories (read-only in v1)** — full PromQL AST semantics (`sum without`, bare `rate`, `label_replace`, `label_join`, Grafana `legendFormat`/links/overrides/transforms, alert Go templates) plus `count(count without (...) (m))` collapse estimates.
- **Forward-only Regret Detection (threshold = 1)** — watches `api-user` query logs against `active_drops`, immediately un-drops & pins regretted metrics to `neverDrop`, and blocks the next tranche.

---

## Try It Locally

Choose the fastest way to see Trimtab in action—see **[docs/LOCAL_EXPERIENCE.md](docs/LOCAL_EXPERIENCE.md)** for the full step-by-step walkthrough, UI credentials, and port-forwarding options:

- **Offline HTML Report (10 seconds, Go only):**
  ```bash
  make demo-html && xdg-open examples/demo/out/report.html   # macOS: open examples/demo/out/report.html
  ```
- **Live Kind Stack + Grafana Dashboards (`docker` + `kind` + `kubectl` + `helm`):**
  ```bash
  make e2e-dev
  # Open Grafana Control Plane: http://localhost:30300/d/trimtab-control-plane
  # Open HTML Audit Report:     test/e2e/artifacts/dev/report.html
  # Clean up when done:         make e2e-down
  ```

---

## 5-Second Offline Demo

Run Trimtab against the bundled offline fixture dataset (`examples/demo/`)—no live Prometheus or Grafana required:

```bash
# Build, run all unit/safety/golden tests with -race, and run the Alloy demo
make verify
make lint
make demo

# Try the OpenTelemetry Collector target (Phase 1.1)
make demo-otel

# Try the Prometheus remote_write target (Phase 2.1)
make demo-promrw

# Render Markdown report (great for CI / PR comments)
make demo-markdown

# Generate standalone HTML report + Prometheus operational metrics
make demo-html
```

---

## CLI Usage

```bash
# Observation-only audit: check safety gates, run regret check, and print report (never activates drops)
trimtab analyze --config trimtab.yaml

# Export Markdown, JSON, or standalone HTML reports
trimtab analyze --config trimtab.yaml --format markdown
trimtab analyze --config trimtab.yaml --format json
trimtab analyze --config trimtab.yaml --format html --out ./report.html

# Emit operational trimtab_* Prometheus metrics (textfile and/or Pushgateway)
trimtab analyze --config trimtab.yaml \
  --metrics-out ./trimtab.prom \
  --pushgateway-url http://pushgateway.monitoring.svc:9091 \
  --pushgateway-job trimtab

# Preview next eligible tranche rules without mutating State Ledger tranches
trimtab synth --config trimtab.yaml --preview-next --out -

# Explicitly activate next eligible tranche (enters pending_confirmation) and write collector rules
trimtab synth --config trimtab.yaml --activate --target alloy  --out ./out/trimtab.alloy
trimtab synth --config trimtab.yaml --activate --target otel   --out ./out/trimtab-rules.yaml
trimtab synth --config trimtab.yaml --activate --target promrw --out ./out/trimtab-remote-write.yaml

# Re-synthesize currently active drops (or manually confirm a pending_confirmation tranche)
trimtab synth --config trimtab.yaml --out ./out/trimtab.alloy
trimtab analyze --config trimtab.yaml --confirm-tranche

# Verify backend connectivity, TLS, auth, tenant headers, and read-only permissions before auditing
trimtab doctor --config trimtab.yaml

# Acknowledge an intentional >20% drop in dashboard/rule counts
trimtab analyze --config trimtab.yaml --accept-baseline-shift
```

---

## Kubernetes Quickstart (Lean Helm Chart)

Deploy Trimtab in any Kubernetes cluster as a zero-CRD, read-only `CronJob` backed by a persistent State Ledger PVC and `trimtab doctor` `helm test` verification:

```bash
helm upgrade --install trimtab oci://ghcr.io/trimtab-io/charts/trimtab \
  -n trimtab --create-namespace \
  -f ./charts/trimtab/examples/values-prod.yaml

helm test trimtab -n trimtab --logs
```

For fast non-production cluster validation (`*/15 * * * *` schedule, `1d` observation window, `15m` soak window, `probeFirstSeen: true`), use [`charts/trimtab/examples/values-nonprod.yaml`](charts/trimtab/examples/values-nonprod.yaml). See [`charts/trimtab/README.md`](charts/trimtab/README.md) for the standalone chart guide, TLS/secret setup, collector integration snippets, and full values reference.

---

## Visualizing Results (HTML Report & Grafana Dashboard)

1. **Standalone Single-File HTML Report (`--format html`):**
   Zero external CDN or JavaScript dependencies—open directly in a browser or archive as a CI artifact:
   ```bash
   # Offline demo (outputs examples/demo/out/report.html and examples/demo/out/trimtab.prom)
   make demo-html

   # Live or custom config
   trimtab analyze --config trimtab.yaml --format html --out ./report.html
   ```
2. **Operational Metrics (`trimtab_*`) & Grafana Dashboard:**
   Import [`deploy/dashboards/trimtab-overview.json`](deploy/dashboards/trimtab-overview.json) into Grafana, or install the Helm chart (`grafanaDashboard.enabled: true` and `cronjob.metricsOut: "/var/lib/trimtab/trimtab.prom"` are enabled by default) with optional Pushgateway delivery:
   ```bash
   helm upgrade --install trimtab ./charts/trimtab -n trimtab \
     -f my-values.yaml \
     --set cronjob.pushgatewayURL=http://pushgateway.monitoring.svc:9091
   ```
   To spin up a full local LGTM stack + `trimtab:dev` container + Grafana dashboards in Kind (without running the 40-minute E2E test suite), run `make e2e-dev` and `make e2e-dev-reload` (see [`test/e2e/README.md`](test/e2e/README.md)).

---

## One-Time Collector Insertion Points

Trimtab never edits human-owned pipeline configs. Wire the insertion point **once**, and let Trimtab manage only the generated drop file behind it.

### 1. Grafana Alloy (`import.file` Mode — Prometheus-Native Path)

**Human-owned `config.alloy` (wired once):**
```hcl
import.file "trimtab" {
  filename = "/etc/alloy/trimtab.alloy"
}

trimtab.drop_unused "default" {
  forward_to = [prometheus.remote_write.prod.receiver]
}

// Route upstream prometheus.scrape or otelcol.exporter.prometheus here:
// forward_to = [trimtab.drop_unused.default.receiver]
```
See [examples/demo/config.alloy.example](examples/demo/config.alloy.example).

### 2. OpenTelemetry Collector (`--config` Overlay — Flagship CNCF Target)

**Human-owned `otel-base.yaml` (wired once):**
```yaml
service:
  pipelines:
    metrics:
      receivers: [prometheus]
      processors: [filter/trimtab, batch]
      exporters: [prometheusremotewrite]
```
Run with:
```bash
otelcol --config=file:otel-base.yaml --config=file:trimtab-rules.yaml
```
Trimtab's `trimtab-rules.yaml` **never** emits a `service:` block, **never** heuristically reverse-translates Prometheus names to OTLP names, and emits `'name == "__trimtab_noop__"'` when 0 drops are active. See [examples/demo/otel-base.yaml.example](examples/demo/otel-base.yaml.example).

### 3. Prometheus `remote_write` (`write_relabel_configs` — Phase 2.1)

Trimtab synthesizes `write_relabel_configs` overlays (standalone `relabel_configs`, `remote_write` entries, or camelCase `monitoring.coreos.com/v1` Prometheus Operator strategic-merge patches) without modifying your base `prometheus.yml`:

```yaml
remote_write:
  - name: prod-mimir
    url: https://mimir.example.com/api/v1/push
    write_relabel_configs:
      - source_labels: [__name__]
        regex: "checkout_legacy_cart_gauge|orders_deprecated_latency_seconds_(bucket|sum|count|created)"
        action: drop
```
See [examples/demo/prometheus-remote-write.yaml.example](examples/demo/prometheus-remote-write.yaml.example).

---

## Documentation

- [docs/LOCAL_EXPERIENCE.md](docs/LOCAL_EXPERIENCE.md) — Step-by-step local try-it guide (offline HTML report & live Kind + Grafana dashboard)
- [docs/LOCAL_DEV.md](docs/LOCAL_DEV.md) — Local development & Kind/Tilt Kubernetes setup (including NixOS prerequisites)
- [docs/DESIGN.md](docs/DESIGN.md) — Full architecture, rationale, invariants, and phased roadmap
- [docs/PLAN.md](docs/PLAN.md) — Living iterative implementation plan across all phases (P2.1b–P3.5)
- [AGENT.md](AGENT.md) — High-signal guide & safety invariants for engineering/agent sessions
- [docs/CHECKPOINT.md](docs/CHECKPOINT.md) — Living progress checkpoint & session handoff tracker
