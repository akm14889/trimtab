# Try Trimtab Locally (Offline Report & Live Grafana UI)

Experience Trimtab in two ways: a **10-second offline run** using bundled fixture data, or a **live local Kubernetes stack** in Kind with Mimir, Loki, Grafana Alloy, Pushgateway, and live Grafana dashboards.

---

## Option 1: 10-Second Offline Experience (Go Only)

No Docker or Kubernetes cluster required—runs against the bundled [`examples/demo/`](../examples/demo/) dataset:

```bash
# Build trimtab and generate an HTML audit report + Prometheus metrics sample
make demo-html
```

Open the generated outputs:

- **Standalone HTML Audit Report** (zero external dependencies):
  - Linux: `xdg-open examples/demo/out/report.html`
  - macOS: `open examples/demo/out/report.html`
- **Operational `trimtab_*` Metrics Sample:**
  ```bash
  cat examples/demo/out/trimtab.prom
  ```

Want to inspect the CLI terminal output or other collector targets instead?

```bash
make demo           # Alloy analyzer summary + drop-rule preview
make demo-otel      # OpenTelemetry Collector target
make demo-promrw    # Prometheus remote_write target
make demo-markdown  # Markdown report output
```

---

## Option 2: Live Kubernetes Experience (`Kind` + Grafana Dashboards)

Spin up a local Kind cluster (`trimtab-e2e`) with **Mimir**, **Loki**, **Grafana**, **Grafana Alloy**, **Pushgateway**, simulated noisy workloads, and your local `trimtab:dev` container image—without waiting for the full E2E test suite.

### Prerequisites

Ensure `docker`, `kind`, `kubectl`, `helm`, `jq`, `curl`, and `make` are installed (and Docker is running). Fits comfortably on a 16 GB laptop (~1.3–1.5 GiB RSS in Docker).

### 1. Start the Local Stack

```bash
make e2e-dev
```

This creates the `trimtab-e2e` Kind cluster, deploys the observability stack and workloads, builds `trimtab:dev`, provisions the Grafana dashboards, runs an audit job (which writes `report.html` and pushes `trimtab_*` metrics through Pushgateway -> Alloy -> Mimir), and copies `report.html` and `trimtab.prom` locally.

### 2. Open the UI & Reports

Once `make e2e-dev` finishes:

1. **Grafana Login Credentials:**
   - Username: `admin`
   - Password:
     ```bash
     kubectl -n monitoring get secret grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
     ```

2. **Grafana Dashboards** (NodePort `:30300`):
   - **Trimtab Control Plane Dashboard:** [http://localhost:30300/d/trimtab-control-plane](http://localhost:30300/d/trimtab-control-plane)
   - **Workload Telemetry Overview (`e2e_*`):** [http://localhost:30300/d/e2e-overview](http://localhost:30300/d/e2e-overview)

   *(If you prefer `kubectl port-forward` instead of NodePort `:30300`, run `kubectl -n monitoring port-forward svc/grafana 8080:80` and open [http://localhost:8080/d/trimtab-control-plane](http://localhost:8080/d/trimtab-control-plane).)*

3. **HTML Audit Report (copied from the Trimtab PVC):**
   - Linux: `xdg-open test/e2e/artifacts/dev/report.html`
   - macOS: `open test/e2e/artifacts/dev/report.html`

4. **Prometheus Metrics Snapshot & Mimir API (optional):**
   ```bash
   cat test/e2e/artifacts/dev/trimtab.prom

   curl -sf -H "X-Scope-OrgID: e2e" \
     "http://localhost:30808/prometheus/api/v1/query" \
     --data-urlencode 'query=trimtab_active_series'
   ```

### 3. Iterate After Code Changes

After editing Go code or Helm templates, rebuild and reload only `trimtab:dev` + dashboard + metrics without recreating the Kind cluster:

```bash
make e2e-dev-reload
```

### 4. Clean Up

```bash
make e2e-down   # Delete the trimtab-e2e Kind cluster
make clean      # Remove local binaries and offline demo outputs
```

---

## Going Deeper

- [`test/e2e/README.md`](../test/e2e/README.md) — Automated E2E scenarios (`S0`–`S14`), manual step-by-step deployment, and troubleshooting
- [`docs/LOCAL_DEV.md`](LOCAL_DEV.md) — Tilt workflow and NixOS prerequisite setup
- [`charts/trimtab/README.md`](../charts/trimtab/README.md) — Deploying Trimtab into your own Kubernetes clusters
