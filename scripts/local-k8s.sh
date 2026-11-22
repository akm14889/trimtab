#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${TRIMTAB_KIND_CLUSTER:-trimtab-dev}"
NAMESPACE="trimtab-local"
IMAGE_NAME="trimtab-dev:latest"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

usage() {
  cat <<EOF
Manage the local Kind cluster for Trimtab development.

Usage:
  $(basename "$0") <command>

Commands:
  up       Create the Kind cluster (${CLUSTER_NAME}) if needed and prepare namespace
  down     Delete the Kind cluster (${CLUSTER_NAME})
  status   Show Kind cluster, kubectl context, and resources in ${NAMESPACE}
  smoke    Build image, load into Kind, run the trimtab-analyze Job, and print logs
  help     Show this help message

Environment:
  TRIMTAB_KIND_CLUSTER   Override Kind cluster name (default: trimtab-dev)
EOF
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "error: required command '$cmd' not found in PATH." >&2
    echo "See docs/LOCAL_DEV.md for installation and NixOS setup instructions." >&2
    exit 1
  fi
}

check_docker_daemon() {
  if ! docker info >/dev/null 2>&1; then
    echo "error: Docker daemon is not reachable." >&2
    echo "Ensure Docker is running and your user has permission to access the Docker socket." >&2
    echo "On NixOS: set virtualisation.docker.enable = true and add your user to the 'docker' group." >&2
    exit 1
  fi

  local docker_cfg="${DOCKER_CONFIG:-$HOME/.docker}/config.json"
  if [[ -f "${docker_cfg}" ]]; then
    local creds_store
    creds_store="$(sed -n 's/.*"credsStore"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${docker_cfg}" | head -n 1)"
    if [[ -n "${creds_store}" ]] && ! command -v "docker-credential-${creds_store}" >/dev/null 2>&1; then
      echo "error: Docker config (${docker_cfg}) sets credsStore='${creds_store}', but 'docker-credential-${creds_store}' is not in PATH." >&2
      echo "Remove \"credsStore\": \"${creds_store}\" from ${docker_cfg} (common leftover from Docker Desktop) and retry." >&2
      exit 1
    fi
  fi
}

cmd_up() {
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  check_docker_daemon

  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    echo "Kind cluster '${CLUSTER_NAME}' already exists."
  else
    echo "Creating Kind cluster '${CLUSTER_NAME}'..."
    kind create cluster --name "${CLUSTER_NAME}"
  fi

  kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null
  kubectl apply -f "${REPO_ROOT}/deploy/local/namespace.yaml"

  if ! command -v tilt >/dev/null 2>&1; then
    echo "warning: 'tilt' not found in PATH. You can still run '$(basename "$0") smoke' or install Tilt (see docs/LOCAL_DEV.md)."
  fi

  echo ""
  echo "Cluster 'kind-${CLUSTER_NAME}' is ready."
  echo "Next steps:"
  echo "  tilt up                  # Build, deploy, and watch logs in Tilt UI"
  echo "  $(basename "$0") smoke   # Or run a one-shot in-cluster smoke test without Tilt"
}

cmd_down() {
  require_cmd kind
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    echo "Deleting Kind cluster '${CLUSTER_NAME}'..."
    kind delete cluster --name "${CLUSTER_NAME}"
  else
    echo "Kind cluster '${CLUSTER_NAME}' does not exist."
  fi
}

cmd_status() {
  require_cmd kind
  require_cmd kubectl
  echo "=== Kind clusters ==="
  kind get clusters || true
  echo ""
  echo "=== Current kubectl context ==="
  kubectl config current-context 2>/dev/null || echo "(none)"
  echo ""
  echo "=== Resources in namespace '${NAMESPACE}' ==="
  kubectl -n "${NAMESPACE}" get all 2>/dev/null || echo "(namespace not found or cluster unreachable)"
}

cmd_smoke() {
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  check_docker_daemon

  if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    echo "Kind cluster '${CLUSTER_NAME}' not found; bringing it up first..."
    cmd_up
  fi

  kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null

  echo "Building ${IMAGE_NAME}..."
  docker build -t "${IMAGE_NAME}" "${REPO_ROOT}"

  echo "Loading ${IMAGE_NAME} into Kind cluster '${CLUSTER_NAME}'..."
  kind load docker-image "${IMAGE_NAME}" --name "${CLUSTER_NAME}"

  echo "Applying local manifests..."
  kubectl apply -f "${REPO_ROOT}/deploy/local/namespace.yaml"
  kubectl -n "${NAMESPACE}" delete job trimtab-analyze --ignore-not-found >/dev/null
  kubectl apply -f "${REPO_ROOT}/deploy/local/trimtab-job.yaml"

  echo "Waiting for trimtab-analyze Job to complete..."
  kubectl -n "${NAMESPACE}" wait --for=condition=complete job/trimtab-analyze --timeout=120s

  echo ""
  echo "=== trimtab-analyze logs ==="
  kubectl -n "${NAMESPACE}" logs job/trimtab-analyze
}

case "${1:-help}" in
  up)     cmd_up ;;
  down)   cmd_down ;;
  status) cmd_status ;;
  smoke)  cmd_smoke ;;
  help|-h|--help) usage ;;
  *)
    echo "error: unknown command '$1'" >&2
    usage >&2
    exit 2
    ;;
esac
