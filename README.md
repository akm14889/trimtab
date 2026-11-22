# Trimtab (`trimtab.io`)

> **Vendor-neutral, open-source adaptive telemetry control plane** written in Go.
> *Out of the data path. Fail-closed by default. Strictly complementary to Prometheus, Mimir, Thanos, OpenTelemetry Collector, Grafana Alloy, Grafana, OpenCost, and Argo CD / Flux.*

---

## What Trimtab Does

Trimtab audits real telemetry usage across **runtime query logs** (globbed Prometheus `query_log_file`, `prom-analytics-proxy`, and Loki-shipped query logs) + **static consumers** (Grafana dashboards, Grafana-managed alert rules, Prometheus alerting & recording rules), and safely rolls out tranched drop rules behind a one-time collector insertion point:

- **Metric-family modeling** — groups classic histograms (`_bucket`/`_sum`/`_count`/`_created`) and summaries as families so series are never orphaned; recognizes native histograms separately; negotiates target synthesizer capabilities (e.g., `_bucket`-only drops on Grafana Alloy vs. advisory-only on OTel Collector).
- **Mandatory runtime query-log gating & cold-start UX** — without query logs covering `>= minObservationWindow` (default `30d`), Trimtab stays in **Report-Only** mode showing projected savings.
- **Query-log origin classification** — splits `api-user`, `rule-eval`, and `excluded-explorer` so recording-rule evaluations never mask unused inputs.
- **Recording-rule transitive DAG** — traces `M -> R1 -> R2` chains backwards from live sinks; drops dead source metrics at the collector **and** lists dead recording rules (flagging `or vector(0)` rules) for deletion.
- **Safety circuit breakers** — Baseline Drift Guard (`>20%` drop in dashboards/rules/logs aborts synthesis), Tranched Rollout (`<=15%` series per tranche with `7d` soak window), Sanity Ceiling (`>80%` droppable aborts), default `neverDrop` meta-monitoring allowlist, and Wildcard-Protector detection/exclusion.
- **Honest `without`-based label-collapse advisories (read-only in v1)** — full PromQL AST semantics (`sum without`, bare `rate`, `label_replace`, `label_join`, Grafana `legendFormat`/links/overrides/transforms, alert Go templates) plus `count(count without (...) (m))` collapse estimates.
- **Forward-only Regret Detection (threshold = 1)** — watches `api-user` query logs against `active_drops`, immediately un-drops & pins regretted metrics to `neverDrop`, and blocks the next tranche.

---

## 5-Second Offline Demo

Run Trimtab against the bundled offline fixture dataset (`examples/demo/`)—no live Prometheus or Grafana required:

```bash
# Build, run all unit/safety/golden tests with -race, and run the Alloy demo
make verify
make demo

# Try the OpenTelemetry Collector target (Phase 1.1)
make demo-otel

# Render Markdown report (great for CI / PR comments)
make demo-markdown
```

---

## CLI Usage

```bash
# Audit usage, check safety gates, print report, and write synthesized rules
trimtab analyze --config trimtab.yaml

# Export Markdown or JSON reports
trimtab analyze --config trimtab.yaml --format markdown
trimtab analyze --config trimtab.yaml --format json

# Synthesize collector rules directly (to file or stdout with --out -)
trimtab synth --config trimtab.yaml --target alloy --out ./out/trimtab.alloy
trimtab synth --config trimtab.yaml --target otel  --out ./out/trimtab-rules.yaml

# Acknowledge an intentional >20% drop in dashboard/rule counts
trimtab analyze --config trimtab.yaml --accept-baseline-shift
```

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

---

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md) — Full architecture, rationale, invariants, and phased roadmap
- [AGENT.md](AGENT.md) — High-signal guide & safety invariants for engineering/agent sessions
- [docs/CHECKPOINT.md](docs/CHECKPOINT.md) — Living progress checkpoint & session handoff tracker
