# Local Development & Kind/Tilt Setup Guide

This guide covers running Trimtab locally—both directly with the Go toolchain and inside a local Kubernetes cluster using **Kind** and **Tilt**, including prerequisite setup on **NixOS**.

---

## 1. Prerequisites

| Tool | Purpose | Minimum / Notes |
| --- | --- | --- |
| `go` | Build and test Trimtab | `1.27+` (see [`go.mod`](../go.mod)) |
| `docker` | Container runtime for Kind and Tilt builds | Docker daemon running & accessible to your user |
| `kind` | Local Kubernetes-in-Docker cluster | Any recent release |
| `kubectl` | Kubernetes CLI | Matching your cluster |
| `tilt` | Live build/deploy/log loop in Kind | Optional if using `scripts/local-k8s.sh smoke` |
| `make` | Convenience targets in [`Makefile`](../Makefile) | GNU Make |

---

## 2. Installing Prerequisites on NixOS

### Option A: Declarative System Configuration (Recommended)

Kind requires a running container runtime (`docker`). Add the following to `/etc/nixos/configuration.nix` (replace `<your-username>` with your Linux username):

```nix
{ pkgs, ... }:
{
  virtualisation.docker.enable = true;

  users.users.<your-username>.extraGroups = [ "docker" ];

  environment.systemPackages = with pkgs; [
    go
    gnumake
    docker
    kind
    kubectl
    tilt
  ];
}
```

Apply the configuration and refresh your group membership (log out/in or run `newgrp docker`):

```bash
sudo nixos-rebuild switch
newgrp docker
```

Verify the setup:

```bash
docker info >/dev/null && go version && kind version && kubectl version --client && tilt version
```

### Option B: Ephemeral `nix-shell`

If Docker is already enabled in `configuration.nix` (`virtualisation.docker.enable = true`), you can pull the remaining CLI tools into a temporary shell without modifying system packages:

```bash
nix-shell -p go gnumake kind kubectl tilt
```

### Standard Linux / macOS

Install `go`, `docker`, `kind`, `kubectl`, and `tilt` via your system package manager (`brew`, `apt`, `dnf`, etc.) and ensure the Docker daemon is running.

---

## 3. Fast Offline Go Loop (No Kubernetes Required)

Before spinning up a cluster, you can run tests and the bundled fixture demo directly:

```bash
make verify         # Run unit/safety/golden tests with -race and build ./bin/trimtab
make demo           # Run Alloy analyzer demo against examples/demo/
make demo-otel      # Run OTel Collector analyzer demo
```

---

## 4. Local Kubernetes Cluster with Kind & Tilt

### Step 1: Create the Kind Cluster

Run the setup script (idempotent):

```bash
./scripts/local-k8s.sh up
# or: make kind-up
```

This checks your prerequisites, creates a Kind cluster named `trimtab-dev`, switches your `kubectl` context to `kind-trimtab-dev`, and creates the `trimtab-local` namespace.

### Step 2: Start Tilt

```bash
tilt up
# or: make tilt-up
```

Open the Tilt UI (by default `http://localhost:10350`, or press `space` in the terminal):

- **`trimtab-analyze`**: Builds [`Dockerfile`](../Dockerfile), loads `trimtab-dev:latest` into Kind, applies [`deploy/local/trimtab-job.yaml`](../deploy/local/trimtab-job.yaml), and streams the `trimtab analyze` report logs. Whenever you edit files under `cmd/`, `pkg/`, or `examples/demo/`, Tilt automatically rebuilds and re-runs the Job.
- **`unit-tests`** (manual trigger): Runs `go test -race ./...` locally from the Tilt UI.
- **`synth-otel-preview`** (manual trigger): Runs `trimtab synth --target otel --out -` inside the cluster to preview synthesized OpenTelemetry Collector filter rules.

### Step 3: Running Without Tilt (Smoke Test)

If you want to verify the containerized Kind workflow without installing Tilt:

```bash
./scripts/local-k8s.sh smoke
# or: make kind-smoke
```

This builds `trimtab-dev:latest`, loads it into `trimtab-dev` via `kind load docker-image`, runs the `trimtab-analyze` Job in `trimtab-local`, waits for completion, and prints the pod logs.

### Step 4: Check Status or Tear Down

```bash
./scripts/local-k8s.sh status   # Inspect Kind clusters, context, and trimtab-local resources
./scripts/local-k8s.sh down     # Delete the trimtab-dev Kind cluster (or: make kind-down)
```

---

## 5. Customizing the In-Cluster Run

The default [`deploy/local/trimtab-job.yaml`](../deploy/local/trimtab-job.yaml) runs against the bundled `/workspace/examples/demo/trimtab.yaml` fixtures baked into the dev image.

To test against an in-cluster Prometheus or Grafana instance:

1. Create a `ConfigMap` containing your custom `trimtab.yaml` pointing `inventory.prometheusURL`, `usage.queryLogs`, and `usage.grafana.url` at your cluster service DNS names (for example, `http://prometheus-server.monitoring.svc.cluster.local:9090`).
2. Mount that `ConfigMap` into the `trimtab-analyze` Job container and pass `--config /etc/trimtab/trimtab.yaml`.

---

## 6. Troubleshooting

- **`docker-credential-desktop: executable file not found in $PATH`**: Your `~/.docker/config.json` has `"credsStore": "desktop"` (a leftover from Docker Desktop) even though the native Linux Docker daemon is used. Remove the `"credsStore"` line from `~/.docker/config.json` and retry.
- **`permission denied` talking to Docker socket**: Ensure your user is in the `docker` group and you started a new login session (or ran `newgrp docker`).
- **Kind fails to start on NixOS (cgroup errors)**: Modern NixOS uses cgroups v2 by default, which works with recent `kind` and `docker` releases. If you see cgroup errors after an upgrade, ensure `virtualisation.docker.enable = true` is active and reboot or restart `docker.service` (`sudo systemctl restart docker`).
- **Wrong `kubectl` context**: Run `kubectl config use-context kind-trimtab-dev`. [`Tiltfile`](../Tiltfile) sets `allow_k8s_contexts('kind-trimtab-dev')` as a safety guard so Tilt never deploys to another cluster by mistake.
