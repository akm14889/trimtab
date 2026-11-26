# Trimtab Helm Chart

Zero-CRD Kubernetes Helm chart deploying [Trimtab](https://trimtab.io) as a single-namespace, read-only `CronJob` with a persistent State Ledger PVC and a `helm test` hook running `trimtab doctor`.

Trimtab schedules **observation audits only** (`trimtab analyze`). Drop rules are never auto-activated from the CronJob; when metrics graduate cleanly across your observation window, preview and activate tranches deliberately with `trimtab synth`.

---

## Prerequisites

- Kubernetes `>= 1.25`
- Helm `>= 3.12`
- Reachable Prometheus-compatible TSDB HTTP endpoint and Grafana (or static PromQL rules / Loki query logs)
- A `PersistentVolumeClaim` storage class supporting `ReadWriteOnce` (recommended so observation history survives across CronJob runs)

---

## Quick Install

### 1. Create a credentials Secret (recommended)

Keep bearer tokens, Grafana API keys, and basic-auth passwords out of `values.yaml` using Kubernetes `Secret` env vars (`envFrom`) or mounted certificate files (`extraVolumes`):

```bash
kubectl create namespace trimtab

kubectl -n trimtab create secret generic trimtab-credentials \
  --from-literal=GRAFANA_API_KEY="glsa_your_read_only_token" \
  --from-literal=PROMETHEUS_BEARER_TOKEN="your_prometheus_token"
```

Create a minimal override file `my-values.yaml`:

```yaml
envFrom:
  - secretRef:
      name: trimtab-credentials

config:
  inventory:
    prometheusURL: https://prometheus.monitoring.svc:9090
    auth:
      bearerTokenEnv: PROMETHEUS_BEARER_TOKEN
  usage:
    grafana:
      url: https://grafana.monitoring.svc:443
      auth:
        bearerTokenEnv: GRAFANA_API_KEY
```

### 2. Install from OCI registry or local checkout

**From GitHub Container Registry (OCI):**

```bash
helm upgrade --install trimtab oci://ghcr.io/trimtab-io/charts/trimtab \
  -n trimtab --create-namespace \
  -f my-values.yaml
```

**From a local repository checkout:**

```bash
helm upgrade --install trimtab ./charts/trimtab \
  -n trimtab --create-namespace \
  -f my-values.yaml
```

> [!NOTE]
> On clusters whose default `StorageClass` uses `volumeBindingMode: WaitForFirstConsumer` (such as Kind's `rancher.io/local-path`, AWS `ebs-sc`, or GKE `standard-rwo`), the `trimtab-state` PVC stays `Pending` until a Pod mounts it. Because this chart schedules a `CronJob` rather than an always-on Deployment, omit `--wait` during `helm upgrade --install` (especially under Helm v4) and run `helm test` or trigger an immediate manual Job (`kubectl create job --from=cronjob/trimtab ...`) to bind the volume.

### 3. Verify connectivity with `helm test`

```bash
helm test trimtab -n trimtab --logs
```

This runs a short-lived Pod executing `trimtab doctor --config /etc/trimtab/trimtab.yaml --format json` to verify backend DNS/TLS, bearer/basic auth, tenant headers, and read-only permissions before waiting for the CronJob schedule.

### 4. Trigger a manual observation run & inspect logs

```bash
kubectl create job --from=cronjob/trimtab trimtab-manual -n trimtab
kubectl logs -n trimtab -l job-name=trimtab-manual -f
```

---

## Visualizing Results (HTML Report & Grafana Dashboard)

- **Grafana Sidecar Dashboard (enabled by default):** `grafanaDashboard.enabled: true` creates a ConfigMap containing `trimtab-overview.json` labeled `grafana_dashboard: "1"` for automatic Grafana sidecar provisioning.
- **Prometheus Operational Metrics (enabled by default):** `cronjob.metricsOut` writes `trimtab_*` gauges to `/var/lib/trimtab/trimtab.prom` on every run; set `cronjob.pushgatewayURL` (e.g., `http://pushgateway.monitoring.svc:9091`) to also push metrics after each audit.
- **Standalone HTML Report:** Set `cronjob.reportFormat: html` and `cronjob.reportOut: /var/lib/trimtab/report.html` (or pass `--format html --out ...` in a manual Job) to persist a self-contained HTML report on the PVC.

---

## Fast Non-Prod Testing Guide

By default, Trimtab waits a full `30d` observation window and `7d` soak window. To test the full workflow in a non-production cluster within a day, use [examples/values-nonprod.yaml](examples/values-nonprod.yaml):

```bash
helm upgrade --install trimtab ./charts/trimtab \
  -n trimtab --create-namespace \
  -f ./charts/trimtab/examples/values-nonprod.yaml
```

Key non-prod settings in `values-nonprod.yaml`:
- `cronjob.schedule: "*/15 * * * *"` — runs observation audits every 15 minutes
- `config.inventory.probeFirstSeen: true` — seeds conservative `first_seen` history from Prometheus lookback on first run
- `config.policy.minObservationWindow: 1d` and `soakWindow: 15m` — accelerates safety gates for non-prod validation

> [!WARNING]
> Do not use `minObservationWindow: 1d` or `soakWindow: 15m` in production. Production environments should retain `30d` / `7d` (or longer for monthly/quarterly jobs).

---

## Collector Integration After Activation

Once metrics have passed your observation window, preview or activate the generated drop rules against the PVC state ledger:

- **Grafana Alloy** (`import.file` / `mode: module`):
  Include the generated `trimtab.alloy` snippet in your `prometheus.relabel` `forward_to` chain.
- **OpenTelemetry Collector** ([examples/values-otel.yaml](examples/values-otel.yaml)):
  Merge the generated `filter` processor snippet into your `metrics` pipeline (`service.pipelines.metrics.processors`).
- **Prometheus `remote_write`** ([examples/values-promrw.yaml](examples/values-promrw.yaml)):
  Merge `write_relabel_configs` into your Prometheus `remote_write` config or `PrometheusAgent` CRD patch.

---

## TLS / mTLS Certificates

Mount private CA bundles or client certificates via `extraVolumes` + `extraVolumeMounts` and reference them via `auth.tls`:

```yaml
extraVolumes:
  - name: backend-tls
    secret:
      secretName: trimtab-backend-tls
extraVolumeMounts:
  - name: backend-tls
    mountPath: /etc/trimtab/certs
    readOnly: true

config:
  inventory:
    prometheusURL: https://prometheus.monitoring.svc:9090
    auth:
      tls:
        caFile: /etc/trimtab/certs/ca.crt
        certFile: /etc/trimtab/certs/tls.crt
        keyFile: /etc/trimtab/certs/tls.key
```

---

## Override Examples

| File | Purpose |
| :--- | :--- |
| [examples/values-nonprod.yaml](examples/values-nonprod.yaml) | Fast non-prod validation (`15m` schedule, `1d` window, `15m` soak, `probeFirstSeen`, Alloy target) |
| [examples/values-prod.yaml](examples/values-prod.yaml) | Hardened production baseline (`6h` schedule, `30d` window, `7d` soak, `2Gi` PVC, secret env auth) |
| [examples/values-otel.yaml](examples/values-otel.yaml) | OpenTelemetry Collector `filter` processor synthesizer target |
| [examples/values-promrw.yaml](examples/values-promrw.yaml) | Prometheus `remote_write` `write_relabel_configs` synthesizer target |

---

## Values Reference

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `image.repository` | string | `ghcr.io/trimtab-io/trimtab` | Container image repository |
| `image.tag` | string | `""` | Overrides `.Chart.AppVersion` |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `serviceAccount.create` | bool | `true` | Create dedicated ServiceAccount |
| `serviceAccount.automount` | bool | `false` | Mount Kubernetes API token (not needed for HTTP polling) |
| `cronjob.schedule` | string | `"0 */6 * * *"` | Audit CronJob schedule |
| `cronjob.timeZone` | string | `""` | Optional IANA time zone for CronJob schedule |
| `cronjob.concurrencyPolicy` | string | `Forbid` | `Allow`, `Forbid`, or `Replace` |
| `cronjob.reportFormat` | string | `markdown` | Output log format: `table`, `markdown`, `json`, or `html` |
| `cronjob.reportOut` | string | `""` | Optional file path to save the rendered report (`--out`) |
| `cronjob.noStateWrite` | bool | `false` | Pass `--no-state-write` to `analyze` |
| `cronjob.metricsOut` | string | `"/var/lib/trimtab/trimtab.prom"` | Write Prometheus `trimtab_*` operational metrics to file (`--metrics-out`) |
| `cronjob.pushgatewayURL` | string | `""` | Push Prometheus `trimtab_*` metrics to Pushgateway URL (`--pushgateway-url`) |
| `cronjob.pushgatewayJob` | string | `"trimtab"` | Pushgateway `job` label (`--pushgateway-job`) |
| `cronjob.extraArgs` | list | `[]` | Extra CLI flags appended to `trimtab analyze` |
| `grafanaDashboard.enabled` | bool | `true` | Create Grafana sidecar ConfigMap with `trimtab-overview.json` |
| `grafanaDashboard.labels` | object | `{grafana_dashboard: "1"}` | Labels added to the Grafana dashboard ConfigMap |
| `doctorTest.enabled` | bool | `true` | Render `helm test` hook running `trimtab doctor` |
| `doctorTest.format` | string | `json` | `trimtab doctor` output format (`table` or `json`) |
| `persistence.enabled` | bool | `true` | Persist State Ledger across CronJob runs |
| `persistence.existingClaim` | string | `""` | Reuse an existing PVC instead of creating one |
| `persistence.storageClassName` | string | `""` | Optional StorageClass name (use `"-"` for empty string) |
| `persistence.size` | string | `1Gi` | PersistentVolumeClaim storage size |
| `persistence.mountPath` | string | `/var/lib/trimtab` | Container mount path for State Ledger |
| `env` / `envFrom` | list | `[]` | Env vars and Secret refs passed to CronJob and `doctor` pods |
| `extraVolumes` / `extraVolumeMounts` | list | `[]` | Extra volumes/mounts for TLS certs or fixture files |
| `config` | object | see `values.yaml` | Full Trimtab YAML configuration mounted at `/etc/trimtab/trimtab.yaml` |

---

## Uninstall

```bash
helm uninstall trimtab -n trimtab
```

Note: Kubernetes retains the PVC (`trimtab-state` by default) unless deleted explicitly (`kubectl delete pvc -n trimtab ...`) so your observation State Ledger is preserved across reinstalls.
