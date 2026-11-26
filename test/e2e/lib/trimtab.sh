#!/usr/bin/env bash
# Helpers for running Trimtab Jobs on the shared PVC, inspecting ledgers,
# reloading Alloy rules, and querying Mimir/Loki/Grafana inside Kind.

ensure_curl_pod() {
  if ! kubectl -n "${MONITORING_NAMESPACE}" get pod e2e-curl >/dev/null 2>&1; then
    kubectl -n "${MONITORING_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: e2e-curl
  namespace: ${MONITORING_NAMESPACE}
  labels:
    app: e2e-curl
spec:
  restartPolicy: Always
  containers:
    - name: curl
      image: ${CURL_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "trap 'exit 0' TERM INT; while true; do sleep 30; done"]
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          cpu: 50m
          memory: 32Mi
EOF
  fi
  kubectl -n "${MONITORING_NAMESPACE}" wait --for=condition=Ready pod/e2e-curl --timeout=120s >/dev/null
}

cluster_curl() {
  ensure_curl_pod
  kubectl -n "${MONITORING_NAMESPACE}" exec e2e-curl -- curl "$@"
}

mimir_instant_query() {
  local promql="$1"
  cluster_curl -sf -G \
    -A "trimtab/e2e-probe" \
    -H "X-Scope-OrgID: ${E2E_TENANT_ID}" \
    --data-urlencode "query=${promql}" \
    "http://mimir.${MONITORING_NAMESPACE}.svc.cluster.local:8080/prometheus/api/v1/query"
}

mimir_series_count() {
  local metric="$1"
  local resp
  resp="$(mimir_instant_query "count(${metric})" 2>/dev/null || echo '{"data":{"result":[]}}')"
  jq -r '.data.result[0].value[1] // "0"' <<<"${resp}" 2>/dev/null || echo "0"
}

loki_query_range() {
  local logql="$1"
  local lookback_sec="${2:-1200}"
  local end_ns start_ns
  end_ns="$(date +%s)000000000"
  start_ns="$(( $(date +%s) - lookback_sec ))000000000"
  cluster_curl -sf -G \
    --data-urlencode "query=${logql}" \
    --data-urlencode "start=${start_ns}" \
    --data-urlencode "end=${end_ns}" \
    --data-urlencode "limit=2000" \
    --data-urlencode "direction=backward" \
    "http://loki.${MONITORING_NAMESPACE}.svc.cluster.local:3100/loki/api/v1/query_range"
}

wait_mimir_series_nonzero() {
  local metric="$1"
  local timeout_s="${2:-180}"
  wait_mimir_series_ge "${metric}" "1" "${timeout_s}"
}

wait_mimir_series_ge() {
  local metric="$1"
  local min_count="${2:-1}"
  local timeout_s="${3:-180}"
  local start_ts cnt elapsed
  start_ts="$(date +%s)"
  while true; do
    cnt="$(mimir_series_count "${metric}")"
    if awk -v c="${cnt}" -v m="${min_count}" 'BEGIN{exit !(c >= m)}'; then
      echo "  [wait] ${metric} active series = ${cnt} (>= ${min_count})"
      return 0
    fi
    elapsed=$(( $(date +%s) - start_ts ))
    if (( elapsed >= timeout_s )); then
      echo "ERROR: timed out after ${timeout_s}s waiting for ${metric} series >= ${min_count} in Mimir (last count=${cnt})" >&2
      return 1
    fi
    sleep 5
  done
}

wait_mimir_series_zero() {
  local metric="$1"
  local timeout_s="${2:-420}"
  local start_ts cnt elapsed
  start_ts="$(date +%s)"
  while true; do
    cnt="$(mimir_series_count "${metric}")"
    if [[ "${cnt}" == "0" ]]; then
      echo "  [wait] ${metric} active series dropped to 0 (stale in Mimir after ${elapsed:-0}s)"
      return 0
    fi
    elapsed=$(( $(date +%s) - start_ts ))
    if (( elapsed >= timeout_s )); then
      echo "ERROR: timed out after ${timeout_s}s waiting for ${metric} series == 0 in Mimir (last count=${cnt})" >&2
      return 1
    fi
    echo "  [wait] ${metric} remaining series=${cnt} (elapsed=${elapsed}s/${timeout_s}s)..."
    sleep 15
  done
}

wait_loki_has_logs() {
  local logql="$1"
  local lookback_sec="${2:-600}"
  local timeout_s="${3:-180}"
  local start_ts resp count elapsed
  start_ts="$(date +%s)"
  while true; do
    resp="$(loki_query_range "${logql}" "${lookback_sec}" 2>/dev/null || echo '{}')"
    count="$(jq -r '[.data.result[]?.values[]?] | length' <<<"${resp}" 2>/dev/null || echo "0")"
    if (( count > 0 )); then
      echo "  [wait] Loki query '${logql}' matched ${count} log line(s)"
      return 0
    fi
    elapsed=$(( $(date +%s) - start_ts ))
    if (( elapsed >= timeout_s )); then
      echo "ERROR: timed out after ${timeout_s}s waiting for Loki logs matching '${logql}'" >&2
      return 1
    fi
    sleep 5
  done
}

trimtab_pvc_name() {
  kubectl -n "${TRIMTAB_NAMESPACE}" get pvc -l app.kubernetes.io/instance=trimtab -o jsonpath='{.items[0].metadata.name}'
}

trimtab_cm_name() {
  kubectl -n "${TRIMTAB_NAMESPACE}" get cm -l app.kubernetes.io/instance=trimtab -o jsonpath='{.items[0].metadata.name}'
}

trimtab_sa_name() {
  kubectl -n "${TRIMTAB_NAMESPACE}" get sa -l app.kubernetes.io/instance=trimtab -o jsonpath='{.items[0].metadata.name}'
}

fetch_pvc_file() {
  local pvc_rel_path="$1"
  local local_dest="$2"
  local pvc_name sa_name pod_name
  pvc_name="$(trimtab_pvc_name)"
  sa_name="$(trimtab_sa_name)"
  pod_name="trimtab-fetch-$(( RANDOM % 100000 ))"

  mkdir -p "$(dirname "${local_dest}")"
  kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true

  kubectl -n "${TRIMTAB_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${TRIMTAB_NAMESPACE}
spec:
  restartPolicy: Never
  serviceAccountName: ${sa_name}
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: cat
      image: ${ALPINE_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "cat \"/var/lib/trimtab/${pvc_rel_path}\""]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: state
          mountPath: /var/lib/trimtab
          readOnly: true
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          cpu: 50m
          memory: 32Mi
  volumes:
    - name: state
      persistentVolumeClaim:
        claimName: ${pvc_name}
EOF

  local start_ts phase
  start_ts="$(date +%s)"
  while true; do
    phase="$(kubectl -n "${TRIMTAB_NAMESPACE}" get pod "${pod_name}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")"
    if [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]]; then
      break
    fi
    if (( $(date +%s) - start_ts >= 90 )); then
      kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true
      echo "ERROR: timed out reading /var/lib/trimtab/${pvc_rel_path}" >&2
      return 1
    fi
    sleep 1
  done

  if [[ "${phase}" == "Succeeded" ]]; then
    kubectl -n "${TRIMTAB_NAMESPACE}" logs "${pod_name}" > "${local_dest}"
    kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true
    return 0
  fi

  kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true
  return 1
}

# Run a one-off Trimtab Job mounting the exact ConfigMap, PVC, and credentials Secret as the chart CronJob.
# Usage: run_trimtab_job <job-prefix> <stdout-log-file> [extra-configmap] -- <trimtab args...>
run_trimtab_job() {
  local job_prefix="$1"
  local out_file="$2"
  shift 2

  local config_cm
  if [[ "$1" != "--" ]]; then
    config_cm="$1"
    shift
  else
    config_cm="$(trimtab_cm_name)"
  fi
  if [[ "$1" == "--" ]]; then
    shift
  fi

  local job_name pvc_name sa_name args_yaml arg
  job_name="$(printf '%s-%s' "${job_prefix}" "$(( $(date +%s) % 100000 ))-$(( RANDOM % 1000 ))" | tr '[:upper:]_' '[:lower:]-' | cut -c1-55)"
  pvc_name="$(trimtab_pvc_name)"
  sa_name="$(trimtab_sa_name)"

  args_yaml=""
  for arg in "$@"; do
    arg="${arg//\\/\\\\}"
    arg="${arg//\"/\\\"}"
    args_yaml+="            - \"${arg}\""$'\n'
  done

  mkdir -p "$(dirname "${out_file}")"
  kubectl -n "${TRIMTAB_NAMESPACE}" delete job "${job_name}" --ignore-not-found >/dev/null 2>&1 || true

  kubectl -n "${TRIMTAB_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${TRIMTAB_NAMESPACE}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 300
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: ${sa_name}
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: trimtab
          image: ${TRIMTAB_IMAGE}
          imagePullPolicy: Never
          args:
${args_yaml%$'\n'}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          envFrom:
            - secretRef:
                name: trimtab-credentials
          volumeMounts:
            - name: config
              mountPath: /etc/trimtab
              readOnly: true
            - name: state
              mountPath: /var/lib/trimtab
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 384Mi
      volumes:
        - name: config
          configMap:
            name: ${config_cm}
        - name: state
          persistentVolumeClaim:
            claimName: ${pvc_name}
EOF

  local start_ts succeeded failed exit_code=0
  start_ts="$(date +%s)"
  while true; do
    succeeded="$(kubectl -n "${TRIMTAB_NAMESPACE}" get job "${job_name}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "")"
    failed="$(kubectl -n "${TRIMTAB_NAMESPACE}" get job "${job_name}" -o jsonpath='{.status.failed}' 2>/dev/null || echo "")"
    if [[ "${succeeded}" == "1" ]]; then
      exit_code=0
      break
    fi
    if [[ -n "${failed}" && "${failed}" != "0" ]]; then
      exit_code="$(kubectl -n "${TRIMTAB_NAMESPACE}" get pods -l "job-name=${job_name}" \
        -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null || true)"
      if [[ -z "${exit_code}" || "${exit_code}" == "0" ]]; then
        exit_code=1
      fi
      break
    fi
    if (( $(date +%s) - start_ts >= 300 )); then
      kubectl -n "${TRIMTAB_NAMESPACE}" logs "job/${job_name}" > "${out_file}" 2>&1 || true
      kubectl -n "${TRIMTAB_NAMESPACE}" delete job "${job_name}" --ignore-not-found >/dev/null 2>&1 || true
      echo "ERROR: trimtab job ${job_name} timed out" >&2
      return 124
    fi
    sleep 2
  done

  kubectl -n "${TRIMTAB_NAMESPACE}" logs "job/${job_name}" > "${out_file}" 2>&1 || true
  kubectl -n "${TRIMTAB_NAMESPACE}" delete job "${job_name}" --ignore-not-found >/dev/null 2>&1 || true
  return "${exit_code}"
}

apply_alloy_module() {
  local alloy_file="$1"
  if [[ ! -f "${alloy_file}" ]]; then
    echo "ERROR: alloy module file not found: ${alloy_file}" >&2
    return 1
  fi
  kubectl -n "${MONITORING_NAMESPACE}" create configmap trimtab-alloy-rules \
    --from-file=trimtab.alloy="${alloy_file}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # Trigger reload immediately so kubelet ConfigMap sync lag doesn't slow tests;
  # import.file also runs with detector="poll" (poll_frequency="5s") over the directory mount.
  local alloy_svc
  alloy_svc="$(kubectl -n "${MONITORING_NAMESPACE}" get svc -l app.kubernetes.io/name=alloy -o jsonpath='{.items[0].metadata.name}')"
  local start_ts
  start_ts="$(date +%s)"
  # Wait up to 90s for kubelet to project the updated ConfigMap symlink, then POST /-/reload.
  local want_hash
  want_hash="$(sha256sum "${alloy_file}" | awk '{print $1}')"
  while true; do
    local pod_name got_hash
    pod_name="$(kubectl -n "${MONITORING_NAMESPACE}" get pod -l app.kubernetes.io/name=alloy -o jsonpath='{.items[0].metadata.name}')"
    got_hash="$(kubectl -n "${MONITORING_NAMESPACE}" exec "${pod_name}" -c alloy -- sha256sum /etc/alloy/trimtab/trimtab.alloy 2>/dev/null | awk '{print $1}' || true)"
    if [[ "${got_hash}" == "${want_hash}" ]]; then
      break
    fi
    if (( $(date +%s) - start_ts >= 120 )); then
      echo "ERROR: timed out waiting for Alloy pod to receive updated trimtab.alloy ConfigMap" >&2
      return 1
    fi
    sleep 3
  done

  cluster_curl -sf -X POST "http://${alloy_svc}.${MONITORING_NAMESPACE}.svc.cluster.local:12345/-/reload" >/dev/null
  echo "  [alloy] ConfigMap synced and /-/reload triggered cleanly"
}

reset_pvc_state() {
  local pvc_name sa_name pod_name
  pvc_name="$(trimtab_pvc_name)"
  sa_name="$(trimtab_sa_name)"
  pod_name="trimtab-reset-$(( RANDOM % 100000 ))"

  kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "${TRIMTAB_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${TRIMTAB_NAMESPACE}
spec:
  restartPolicy: Never
  serviceAccountName: ${sa_name}
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: reset
      image: ${ALPINE_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "rm -f /var/lib/trimtab/.trimtab-state.json /var/lib/trimtab/.trimtab-state-*.tmp /var/lib/trimtab/trimtab.alloy"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: state
          mountPath: /var/lib/trimtab
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          cpu: 50m
          memory: 32Mi
  volumes:
    - name: state
      persistentVolumeClaim:
        claimName: ${pvc_name}
EOF

  local start_ts phase
  start_ts="$(date +%s)"
  while true; do
    phase="$(kubectl -n "${TRIMTAB_NAMESPACE}" get pod "${pod_name}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")"
    if [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]]; then
      break
    fi
    if (( $(date +%s) - start_ts >= 90 )); then
      kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true
      echo "ERROR: timed out resetting PVC state" >&2
      return 1
    fi
    sleep 1
  done
  kubectl -n "${TRIMTAB_NAMESPACE}" delete pod "${pod_name}" --ignore-not-found >/dev/null 2>&1 || true
  if [[ "${phase}" != "Succeeded" ]]; then
    echo "ERROR: PVC reset pod failed (phase=${phase})" >&2
    return 1
  fi
  echo "  [reset] Cleared /var/lib/trimtab/.trimtab-state.json and trimtab.alloy on PVC ${pvc_name}"
}

grafana_admin_pass() {
  kubectl -n "${MONITORING_NAMESPACE}" get secret grafana -o jsonpath='{.data.admin-password}' | base64 -d
}

grafana_api() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  local pass
  pass="$(grafana_admin_pass)"
  if [[ -n "${body}" ]]; then
    cluster_curl -sf -X "${method}" \
      -u "admin:${pass}" \
      -H "Content-Type: application/json" \
      -d "${body}" \
      "http://grafana.${MONITORING_NAMESPACE}.svc.cluster.local:80${path}"
  else
    cluster_curl -sf -X "${method}" \
      -u "admin:${pass}" \
      "http://grafana.${MONITORING_NAMESPACE}.svc.cluster.local:80${path}"
  fi
}

render_passthrough_alloy() {
  local out_file="$1"
  cat > "${out_file}" <<'EOF'
// Managed by Trimtab E2E harness (pass-through baseline)
declare "drop_unused" {
  argument "forward_to" {
    optional = false
  }
  export "receiver" {
    value = prometheus.relabel.trimtab_drop.receiver
  }

  prometheus.relabel "trimtab_drop" {
    forward_to = argument.forward_to.value
  }
}
EOF
}
