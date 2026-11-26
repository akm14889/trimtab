#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# shellcheck source=versions.env
source "${SCRIPT_DIR}/versions.env"
# shellcheck source=lib/assert.sh
source "${SCRIPT_DIR}/lib/assert.sh"
# shellcheck source=lib/trimtab.sh
source "${SCRIPT_DIR}/lib/trimtab.sh"

ARTIFACTS_BASE="${SCRIPT_DIR}/artifacts"
RUN_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
ARTIFACTS_DIR="${ARTIFACTS_BASE}/${RUN_STAMP}"
OBSERVATION_WAIT_SECONDS="${E2E_OBS_WAIT_SECONDS:-1260}" # 21m > 20m minObservationWindow
STALENESS_WAIT_SECONDS="${E2E_STALE_WAIT_SECONDS:-420}" # >5m Mimir staleness + scrape
SOAK_WAIT_SECONDS="${E2E_SOAK_WAIT_SECONDS:-330}"        # >5m soakWindow

usage() {
  cat <<EOF
Trimtab Local Kind E2E Harness

Usage:
  $(basename "$0") <subcommand>

Subcommands:
  up             Create Kind cluster (${KIND_CLUSTER_NAME}) and install Mimir, Loki, Grafana, Alloy
  build          Docker build trimtab:e2e, noisy-app, traffic-gen and load into Kind
  deploy         Install Trimtab Helm chart with values/trimtab-e2e.yaml and run helm test (doctor)
  seed           Deploy noisy-app & traffic-gen, load Mimir ruler rules, provision Grafana dashboards & queries
  test           Run scenarios S0..S14 in order, write artifacts + summary.md, exit non-zero on FAIL
  scenario <id>  Run a single scenario (e.g. S0, S2, S7, S14) assuming cluster is up & seeded
  reset          Wipe PVC ledger & synthesized file, restore Alloy pass-through and workload env defaults
  artifacts      Print path of latest artifacts directory
  down           Delete Kind cluster (${KIND_CLUSTER_NAME})
  all            Run: up -> build -> deploy -> seed -> test
EOF
}

require_prereqs() {
  local missing=()
  local cmd
  for cmd in docker kind kubectl helm jq curl; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing+=("${cmd}")
    fi
  done
  if (( ${#missing[@]} > 0 )); then
    echo "ERROR: missing required prerequisite(s): ${missing[*]}" >&2
    echo "Install docker, kind, kubectl, helm, jq, and curl before running E2E tests." >&2
    exit 1
  fi
  if ! docker info >/dev/null 2>&1; then
    echo "ERROR: Docker daemon is not reachable." >&2
    exit 1
  fi
}

use_kind_context() {
  kubectl config use-context "kind-${KIND_CLUSTER_NAME}" >/dev/null
}

ensure_helm_repos() {
  helm repo add grafana "${GRAFANA_HELM_REPO}" --force-update >/dev/null
  helm repo add grafana-community "${LOKI_HELM_REPO}" --force-update >/dev/null
  helm repo update grafana grafana-community >/dev/null
}

cmd_up() {
  require_prereqs
  echo ">>> [up] Preparing Kind cluster '${KIND_CLUSTER_NAME}'..."

  if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
    echo "Kind cluster '${KIND_CLUSTER_NAME}' already exists."
  else
    kind create cluster --config "${SCRIPT_DIR}/kind-config.yaml" --name "${KIND_CLUSTER_NAME}" --wait 120s
  fi
  use_kind_context

  kubectl create namespace "${MONITORING_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl create namespace "${TRIMTAB_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # Ensure Alloy's trimtab-alloy-rules ConfigMap starts as a valid pass-through before Alloy boots.
  local tmp_alloy
  tmp_alloy="$(mktemp)"
  render_passthrough_alloy "${tmp_alloy}"
  kubectl -n "${MONITORING_NAMESPACE}" create configmap trimtab-alloy-rules \
    --from-file=trimtab.alloy="${tmp_alloy}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  rm -f "${tmp_alloy}"

  echo ">>> [up] Deploying Mimir (${MIMIR_IMAGE})..."
  kubectl apply -f "${SCRIPT_DIR}/values/mimir.yaml" >/dev/null

  ensure_helm_repos

  echo ">>> [up] Installing Loki (chart ${LOKI_CHART_VERSION})..."
  helm upgrade --install loki grafana-community/loki \
    --version "${LOKI_CHART_VERSION}" \
    -n "${MONITORING_NAMESPACE}" \
    -f "${SCRIPT_DIR}/values/loki.yaml" \
    --wait --timeout 5m >/dev/null

  echo ">>> [up] Installing Grafana (chart ${GRAFANA_CHART_VERSION})..."
  local admin_pass
  if kubectl -n "${MONITORING_NAMESPACE}" get secret grafana >/dev/null 2>&1; then
    admin_pass="$(grafana_admin_pass)"
  else
    admin_pass="e2e-$(date +%s)-$(( RANDOM % 9000 + 1000 ))"
  fi
  helm upgrade --install grafana grafana/grafana \
    --version "${GRAFANA_CHART_VERSION}" \
    -n "${MONITORING_NAMESPACE}" \
    -f "${SCRIPT_DIR}/values/grafana.yaml" \
    --set "adminPassword=${admin_pass}" \
    --wait --timeout 5m >/dev/null

  echo ">>> [up] Installing Alloy (chart ${ALLOY_CHART_VERSION})..."
  helm upgrade --install alloy grafana/alloy \
    --version "${ALLOY_CHART_VERSION}" \
    -n "${MONITORING_NAMESPACE}" \
    -f "${SCRIPT_DIR}/values/alloy.yaml" \
    --wait --timeout 5m >/dev/null

  echo ">>> [up] Waiting for Mimir Deployment..."
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/mimir --timeout=300s

  ensure_curl_pod
  echo ">>> [up] Provisioning Grafana ServiceAccount token for Trimtab..."
  provision_grafana_token_secret

  echo ">>> [up] Cluster '${KIND_CLUSTER_NAME}' and LGTM stack are ready."
}

provision_grafana_token_secret() {
  use_kind_context
  local sa_name="trimtab-reader"
  local sa_list sa_id token_resp token

  sa_list="$(grafana_api GET "/api/serviceaccounts/search?query=${sa_name}")"
  sa_id="$(jq -r --arg name "${sa_name}" '.serviceAccounts[]? | select(.name == $name) | .id' <<<"${sa_list}" | head -n1)"

  # Provision with Admin role? Start with Viewer or Editor; let's check both in FINDINGS if provisioning API rejects Viewer!
  # First check what E2E_GRAFANA_ROLE is set to (default Viewer to test least-privilege as documented).
  local role="${E2E_GRAFANA_ROLE:-Viewer}"
  if [[ -z "${sa_id}" ]]; then
    sa_id="$(grafana_api POST "/api/serviceaccounts" "{\"name\":\"${sa_name}\",\"role\":\"${role}\"}" | jq -r '.id')"
  fi

  local tok_name="e2e-token-$(date +%s)"
  token_resp="$(grafana_api POST "/api/serviceaccounts/${sa_id}/tokens" "{\"name\":\"${tok_name}\"}")"
  token="$(jq -r '.key' <<<"${token_resp}")"
  if [[ -z "${token}" || "${token}" == "null" ]]; then
    echo "ERROR: failed to generate Grafana service account token: ${token_resp}" >&2
    exit 1
  fi

  kubectl -n "${TRIMTAB_NAMESPACE}" create secret generic trimtab-credentials \
    --from-literal=GRAFANA_API_KEY="${token}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

cmd_build() {
  require_prereqs
  use_kind_context

  local git_commit
  git_commit="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo "e2e")"

  echo ">>> [build] Building ${TRIMTAB_IMAGE}..."
  docker build \
    --build-arg "VERSION=v0.0.1-beta.3-e2e" \
    --build-arg "COMMIT=${git_commit}" \
    -t "${TRIMTAB_IMAGE}" "${REPO_ROOT}"

  echo ">>> [build] Building ${NOISY_APP_IMAGE}..."
  docker build -t "${NOISY_APP_IMAGE}" "${SCRIPT_DIR}/workloads/noisy-app"

  echo ">>> [build] Building ${TRAFFIC_GEN_IMAGE}..."
  docker build -t "${TRAFFIC_GEN_IMAGE}" "${SCRIPT_DIR}/workloads/traffic-gen"

  echo ">>> [build] Loading images into Kind cluster '${KIND_CLUSTER_NAME}'..."
  kind load docker-image "${TRIMTAB_IMAGE}" "${NOISY_APP_IMAGE}" "${TRAFFIC_GEN_IMAGE}" --name "${KIND_CLUSTER_NAME}"
}

cmd_deploy() {
  require_prereqs
  use_kind_context
  ensure_curl_pod

  if ! kubectl -n "${TRIMTAB_NAMESPACE}" get secret trimtab-credentials >/dev/null 2>&1; then
    provision_grafana_token_secret
  fi

  echo ">>> [deploy] Installing Trimtab Helm chart..."
  local rel_status
  rel_status="$(helm status trimtab -n "${TRIMTAB_NAMESPACE}" -o json 2>/dev/null | jq -r '.info.status // ""' || true)"
  if [[ "${rel_status}" == pending-* ]]; then
    helm uninstall trimtab -n "${TRIMTAB_NAMESPACE}" >/dev/null 2>&1 || true
  fi
  helm upgrade --install trimtab "${REPO_ROOT}/charts/trimtab" \
    -n "${TRIMTAB_NAMESPACE}" \
    -f "${SCRIPT_DIR}/values/trimtab-e2e.yaml" \
    --timeout 3m

  echo ">>> [deploy] Running pre-seed helm test (connectivity check)..."
  helm test trimtab -n "${TRIMTAB_NAMESPACE}" --logs || true
}

cmd_seed() {
  require_prereqs
  use_kind_context
  ensure_curl_pod

  echo ">>> [seed] Ensuring pass-through Alloy module..."
  local tmp_alloy
  tmp_alloy="$(mktemp)"
  render_passthrough_alloy "${tmp_alloy}"
  apply_alloy_module "${tmp_alloy}"
  rm -f "${tmp_alloy}"

  echo ">>> [seed] Loading Mimir ruler rules..."
  local rule_file
  for rule_file in "${SCRIPT_DIR}"/workloads/rules/*.yaml; do
    local ns_name="e2e-rules"
    local payload
    payload="$(cat "${rule_file}")"
    cluster_curl -sf -X POST \
      -H "X-Scope-OrgID: ${E2E_TENANT_ID}" \
      -H "Content-Type: application/yaml" \
      --data-binary "${payload}" \
      "http://mimir.${MONITORING_NAMESPACE}.svc.cluster.local:8080/prometheus/config/v1/rules/${ns_name}" >/dev/null
    echo "  loaded rule group from $(basename "${rule_file}") into namespace '${ns_name}'"
  done

  # Wait for Mimir Ruler API to list the rules
  local start_ts
  start_ts="$(date +%s)"
  while true; do
    if cluster_curl -sf -H "X-Scope-OrgID: ${E2E_TENANT_ID}" \
      "http://mimir.${MONITORING_NAMESPACE}.svc.cluster.local:8080/prometheus/config/v1/rules" | grep -q "E2EAlertOnlyHigh"; then
      echo "  [wait] Mimir ruler API serving loaded rules"
      break
    fi
    if (( $(date +%s) - start_ts >= 60 )); then
      echo "ERROR: timed out waiting for Mimir ruler API to serve rules" >&2
      exit 1
    fi
    sleep 2
  done

  echo ">>> [seed] Provisioning Grafana dashboards..."
  local dash_file
  for dash_file in "${SCRIPT_DIR}"/workloads/dashboards/*.json; do
    local body
    body="$(cat "${dash_file}")"
    grafana_api POST "/api/dashboards/db" "${body}" >/dev/null
    echo "  provisioned dashboard $(basename "${dash_file}")"
  done

  echo ">>> [seed] Deploying noisy-app (3 replicas)..."
  kubectl apply -f "${SCRIPT_DIR}/workloads/noisy-app/k8s.yaml" >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/noisy-app --timeout=180s

  echo ">>> [seed] Waiting for noisy-app metrics in Mimir..."
  wait_mimir_series_ge "e2e_used_requests_total" 1080 180
  wait_mimir_series_ge "e2e_unused_big_gauge" 600 180
  wait_mimir_series_nonzero "e2e:rr_live:rate5m" 180
  wait_mimir_series_nonzero "e2e:rr_dead:sum" 180

  echo ">>> [seed] Deploying traffic-gen..."
  kubectl apply -f "${SCRIPT_DIR}/workloads/traffic-gen/k8s.yaml" >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/traffic-gen --timeout=180s

  echo ">>> [seed] Triggering real Grafana dashboard queries via /api/ds/query..."
  trigger_grafana_dashboard_queries

  echo ">>> [seed] Waiting for Mimir query-frontend logs to land in Loki..."
  wait_loki_has_logs '{namespace="monitoring", component="query-frontend"} |= "param_query"' 600 180
}

trigger_grafana_dashboard_queries() {
  local now_ms from_ms
  now_ms="$(( $(date +%s) * 1000 ))"
  from_ms="$(( now_ms - 600000 ))"
  local queries=(
    'sum by (route, status) (rate(e2e_used_requests_total[5m]))'
    'histogram_quantile(0.95, sum by (le, route) (rate(e2e_hist_full_seconds_bucket[5m])))'
    'sum by (le, route) (rate(e2e_labels_http_duration_seconds_bucket[5m]))'
    'sum by (route) (rate(e2e_latency_seconds_sum[5m])) / sum by (route) (rate(e2e_latency_seconds_count[5m]))'
    'sum by (job) (rate(e2e_scalar_cmp_total[5m]) > 0)'
    'sum by (service) (e2e:rr_live:rate5m)'
  )
  local q pass
  pass="$(grafana_admin_pass)"
  for q in "${queries[@]}"; do
    local payload
    payload="$(jq -nc --arg from "${from_ms}" --arg to "${now_ms}" --arg expr "${q}" '{
      from: $from,
      to: $to,
      queries: [{
        refId: "A",
        datasource: { type: "prometheus", uid: "mimir" },
        expr: $expr,
        range: true,
        intervalMs: 30000,
        maxDataPoints: 100
      }]
    }')"
    cluster_curl -sf -X POST -u "admin:${pass}" \
      -H "Content-Type: application/json" \
      -H "X-Dashboard-UID: e2e-overview" \
      -d "${payload}" \
      "http://grafana.${MONITORING_NAMESPACE}.svc.cluster.local:80/api/ds/query" >/dev/null || true
  done
}

extract_json_stdout() {
  local raw="$1"
  local out="$2"
  # Print from first '{' to last '}' in case stderr/warnings were mixed into pod logs
  awk 'BEGIN{p=0} /^\{/{p=1} {if(p) print}' "${raw}" > "${out}"
}

snapshot_artifacts_pod_logs() {
  local dir="${ARTIFACTS_DIR}/pod-logs"
  mkdir -p "${dir}"
  kubectl -n "${MONITORING_NAMESPACE}" logs deployment/mimir --tail=500 > "${dir}/mimir.log" 2>&1 || true
  kubectl -n "${MONITORING_NAMESPACE}" logs statefulset/loki --tail=500 > "${dir}/loki.log" 2>&1 || true
  kubectl -n "${MONITORING_NAMESPACE}" logs deployment/grafana --tail=300 > "${dir}/grafana.log" 2>&1 || true
  kubectl -n "${MONITORING_NAMESPACE}" logs deployment/alloy -c alloy --tail=300 > "${dir}/alloy.log" 2>&1 || true
  kubectl -n "${MONITORING_NAMESPACE}" logs deployment/traffic-gen --tail=200 > "${dir}/traffic-gen.log" 2>&1 || true
}

wait_for_condition_poll() {
  local desc="$1"
  local timeout_s="$2"
  local interval_s="$3"
  local check_fn="$4"
  local start_ts elapsed
  start_ts="$(date +%s)"
  while true; do
    if "${check_fn}"; then
      return 0
    fi
    elapsed=$(( $(date +%s) - start_ts ))
    if (( elapsed >= timeout_s )); then
      echo "ERROR: timed out after ${timeout_s}s waiting for: ${desc}" >&2
      return 1
    fi
    echo "  [wait] ${desc} (${elapsed}s/${timeout_s}s)..."
    sleep "${interval_s}"
  done
}

# ==============================================================================
# Scenarios S0 .. S14
# ==============================================================================

run_S0() {
  local sdir="${ARTIFACTS_DIR}/S0"
  mkdir -p "${sdir}"
  scenario_begin "S0" "Doctor / connectivity"

  local helm_test_ok=true
  helm test trimtab -n "${TRIMTAB_NAMESPACE}" --logs > "${sdir}/helm-test.log" 2>&1 || helm_test_ok=false

  local doc_rc=0
  run_trimtab_job "e2e-s0-doctor" "${sdir}/doctor-raw.txt" -- \
    doctor --config /etc/trimtab/trimtab.yaml --format json || doc_rc=$?
  extract_json_stdout "${sdir}/doctor-raw.txt" "${sdir}/doctor.json"

  cat "${sdir}/helm-test.log" | sed 's/^/  [helm-test] /'

  if ! "${helm_test_ok}"; then
    scenario_finding "helm test failed with Viewer role or pre-seed config (see S0/helm-test.log and S0/doctor.json)"
  fi

  if [[ -s "${sdir}/doctor.json" ]]; then
    assert_json_path "${sdir}/doctor.json" '.checks[] | select(.component == "inventory:prometheus") | .status' "inventory:prometheus present" || true
    assert_json_eq "${sdir}/doctor.json" '.checks[] | select(.component == "inventory:prometheus") | .status' "OK" "Mimir Prometheus API OK" || true
    assert_json_eq "${sdir}/doctor.json" '.checks[] | select(.component == "querylog:loki[0]") | .status' "OK" "Loki query log API OK" || true
    assert_json_eq "${sdir}/doctor.json" '.checks[] | select(.component == "ruler:mimir[0]") | .status' "OK" "Mimir Ruler API OK" || true

    local graf_status graf_detail
    graf_status="$(jq -r '.checks[] | select(.component == "grafana:http") | .status // "MISSING"' "${sdir}/doctor.json")"
    graf_detail="$(jq -r '.checks[] | select(.component == "grafana:http") | .detail // ""' "${sdir}/doctor.json")"
    if [[ "${graf_status}" != "OK" ]]; then
      scenario_finding "Grafana doctor check failed (likely Viewer SA token denied /api/v1/provisioning/alert-rules): status=${graf_status}, detail=${graf_detail}"
      echo ">>> Upgrading Grafana SA role for harness continuity? Recording Viewer behavior and switching role=Admin so dashboards+alerts can be scanned in subsequent scenarios..."
      local sa_id token
      sa_id="$(grafana_api GET "/api/serviceaccounts/search?query=trimtab-reader" | jq -r '.serviceAccounts[0].id')"
      grafana_api PATCH "/api/serviceaccounts/${sa_id}" '{"role":"Admin"}' >/dev/null
      # Re-run doctor to verify Grafana reaches OK with Admin role
      run_trimtab_job "e2e-s0-doctor-admin" "${sdir}/doctor-admin-raw.txt" -- \
        doctor --config /etc/trimtab/trimtab.yaml --format json || doc_rc=$?
      extract_json_stdout "${sdir}/doctor-admin-raw.txt" "${sdir}/doctor-admin.json"
      assert_json_eq "${sdir}/doctor-admin.json" '.passed' "true" "doctor passed after Admin SA role" || true
      helm test trimtab -n "${TRIMTAB_NAMESPACE}" --logs > "${sdir}/helm-test-admin.log" 2>&1 || scenario_fail "helm test still failing even with Admin SA role"
    else
      assert_eq "${doc_rc}" "0" "doctor exit code == 0" || true
    fi
  else
    scenario_fail "doctor produced no JSON output"
  fi

  scenario_end
}

run_S1() {
  local sdir="${ARTIFACTS_DIR}/S1"
  mkdir -p "${sdir}"
  scenario_begin "S1" "Cold start (query log coverage < minObservationWindow)"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json" 2>/dev/null || echo "{}" > "${sdir}/ledger-before.json"

  local rc=0
  run_trimtab_job "e2e-s1-analyze" "${sdir}/analyze-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json || rc=$?
  assert_eq "${rc}" "0" "analyze exits 0" || true
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"

  run_trimtab_job "e2e-s1-synth" "${sdir}/synth-logs.txt" -- \
    synth --config /etc/trimtab/trimtab.yaml || true
  fetch_pvc_file "trimtab.alloy" "${sdir}/trimtab.alloy" 2>/dev/null || true
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json" 2>/dev/null || echo "{}" > "${sdir}/ledger-after.json"

  assert_json_eq "${sdir}/report.json" '.summary.synthesizedFamilies' "0" "0 synthesized families on cold start" || true
  assert_json_eq "${sdir}/report.json" '.summary.synthesizedSeries' "0" "0 synthesized series on cold start" || true
  if [[ -f "${sdir}/trimtab.alloy" ]]; then
    assert_not_contains "$(cat "${sdir}/trimtab.alloy")" 'action        = "drop"' "trimtab.alloy is pass-through on cold start" || true
  fi

  local report_only cov_days req_days proj_len
  report_only="$(jq -r '.safety.reportOnly' "${sdir}/report.json")"
  cov_days="$(jq -r '.safety.queryLogCoveredDays' "${sdir}/report.json")"
  req_days="$(jq -r '.safety.requiredWindowDays' "${sdir}/report.json")"
  proj_len="$(jq -r '(.projectedDrops // []) | length' "${sdir}/report.json")"

  scenario_note "reportOnly=${report_only}, coverage=${cov_days}/${req_days}d, projectedDrops=${proj_len}"
  assert_json_true "${sdir}/report.json" '.safety.reportOnly' "cold start stays in reportOnly mode" || true
  assert_json_eq "${sdir}/report.json" '.summary.confidence' "MEDIUM" "cold start confidence is MEDIUM" || true
  assert_gt "${proj_len}" "0" "projectedDrops populated during cold start (${proj_len} families)" || true

  scenario_end
}

wait_for_observation_window() {
  local ledger_first_seen now_epoch target_epoch remaining
  local tmp_ledger
  tmp_ledger="$(mktemp)"
  fetch_pvc_file ".trimtab-state.json" "${tmp_ledger}" 2>/dev/null || echo "{}" > "${tmp_ledger}"
  local earliest_seen
  earliest_seen="$(jq -r '[.seen_families[]?.first_seen_at] | sort | .[0] // empty' "${tmp_ledger}")"
  rm -f "${tmp_ledger}"

  if [[ -z "${earliest_seen}" ]]; then
    echo ">>> [window] No first_seen_at recorded yet; sleeping ${OBSERVATION_WAIT_SECONDS}s..."
    target_epoch=$(( $(date +%s) + OBSERVATION_WAIT_SECONDS ))
  else
    local first_epoch
    first_epoch="$(date -d "${earliest_seen}" +%s 2>/dev/null || date +%s)"
    target_epoch=$(( first_epoch + OBSERVATION_WAIT_SECONDS ))
  fi

  # Keep ROTATION_ACTIVE=true until ~45% through the observation window (~9m), stamp
  # last_queried_at in the ledger, then pause it so by S9 (~T+23m) it is >5m absent
  # from Loki's 5m window yet still <20m old inside the ledger!
  local pause_epoch=$(( target_epoch - (OBSERVATION_WAIT_SECONDS * 55 / 100) ))
  local rot_paused=false

  while true; do
    now_epoch="$(date +%s)"
    if [[ "${rot_paused}" == "false" && "${now_epoch}" -ge "${pause_epoch}" ]]; then
      echo ">>> [window] Recording e2e_rotation_gauge in ledger, then pausing rotation traffic for S9..."
      wait_loki_has_logs '{namespace="monitoring", component="query-frontend"} |= "e2e_rotation_gauge"' 600 120 || true
      local pre_rot_log
      pre_rot_log="$(mktemp)"
      run_trimtab_job "e2e-pre-rot" "${pre_rot_log}" -- analyze --config /etc/trimtab/trimtab.yaml --format json || true
      rm -f "${pre_rot_log}"
      kubectl -n "${MONITORING_NAMESPACE}" set env deployment/traffic-gen ROTATION_ACTIVE=false >/dev/null
      kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/traffic-gen --timeout=120s >/dev/null
      rot_paused=true
    fi

    remaining=$(( target_epoch - now_epoch ))
    if (( remaining <= 0 )); then
      echo ">>> [window] 20m observation window elapsed!"
      break
    fi
    local step=60
    if (( remaining < step )); then
      step="${remaining}"
    fi
    echo "  [wait] Observation window remaining: ${remaining}s (~$(( (remaining + 59) / 60 ))m)..."
    sleep "${step}"
  done
}

run_S2() {
  local sdir="${ARTIFACTS_DIR}/S2"
  mkdir -p "${sdir}"
  scenario_begin "S2" "Steady state after observation window"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json" 2>/dev/null || echo "{}" > "${sdir}/ledger-before.json"
  local rc=0
  run_trimtab_job "e2e-s2-analyze" "${sdir}/analyze-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json || rc=$?
  assert_eq "${rc}" "0" "analyze exits 0" || true
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json" 2>/dev/null || echo "{}" > "${sdir}/ledger-after.json"

  assert_json_eq "${sdir}/report.json" '.summary.confidence' "HIGH" "confidence is HIGH" || true
  assert_json_false "${sdir}/report.json" '.safety.reportOnly' "reportOnly is false" || true

  # Collect all droppable item names across tranches + oversized
  local droppables_json="${sdir}/droppables.json"
  jq -c '[(.tranches[]?.items[]?), (.oversizedFamilies[]?)]' "${sdir}/report.json" > "${droppables_json}"

  local fam
  for fam in e2e_unused_legacy_gauge e2e_unused_big_gauge e2e_rr_dead_source_total; do
    assert_json_array_contains "${droppables_json}" '[.[].familyName]' "${fam}" "droppable contains ${fam}" || true
  done

  # Assert bucket-only action for e2e_latency_seconds
  local lat_action
  lat_action="$(jq -r '.[] | select(.familyName == "e2e_latency_seconds") | .action // "MISSING"' "${droppables_json}")"
  assert_eq "${lat_action}" "DROP_HISTOGRAM_BUCKETS" "e2e_latency_seconds recommended as DROP_HISTOGRAM_BUCKETS" || true

  # Protected families (including bare-token autoscaler query in Mimir logfmt!)
  for fam in e2e_used_requests_total e2e_alert_only_gauge e2e_rr_source_total e2e_hist_full_seconds e2e_autoscaler_metric up; do
    assert_json_path "${sdir}/report.json" ".protectedReasons[\"${fam}\"]" "protectedReasons includes ${fam}" || true
  done
  assert_json_array_not_contains "${droppables_json}" '[.[].familyName]' "e2e_autoscaler_metric" "e2e_autoscaler_metric NOT in droppables" || true
  assert_json_array_not_contains "${droppables_json}" '[.[].familyName]' "e2e_new_metric" "0-series historical e2e_new_metric NOT in droppables" || true

  # Dead recording rules advisory (both rules + or vector(0) flag)
  assert_json_array_contains "${sdir}/report.json" '[.deadRecordingRules[]?.ruleName]' "e2e:rr_dead:rate5m" "deadRecordingRules includes e2e:rr_dead:rate5m" || true
  assert_json_array_contains "${sdir}/report.json" '[.deadRecordingRules[]?.ruleName]' "e2e:rr_dead:sum" "deadRecordingRules includes e2e:rr_dead:sum" || true
  assert_json_true "${sdir}/report.json" '.deadRecordingRules[] | select(.ruleName == "e2e:rr_dead:sum") | .hasOrVectorZero' "e2e:rr_dead:sum hasOrVectorZero == true" || true

  scenario_end
}

run_S3() {
  local sdir="${ARTIFACTS_DIR}/S3"
  mkdir -p "${sdir}"
  scenario_begin "S3" "Analyze is strictly observation-only"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"
  local before_updated before_tranches before_active
  before_updated="$(jq -r '.updated_at' "${sdir}/ledger-before.json")"
  before_tranches="$(jq -c '.tranches // []' "${sdir}/ledger-before.json")"
  before_active="$(jq -c '.active_drops // {}' "${sdir}/ledger-before.json")"

  sleep 2
  run_trimtab_job "e2e-s3-analyze" "${sdir}/analyze-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json"

  local after_updated after_tranches after_active
  after_updated="$(jq -r '.updated_at' "${sdir}/ledger-after.json")"
  after_tranches="$(jq -c '.tranches // []' "${sdir}/ledger-after.json")"
  after_active="$(jq -c '.active_drops // {}' "${sdir}/ledger-after.json")"

  assert_eq "${after_tranches}" "${before_tranches}" "analyze added 0 tranches" || true
  assert_eq "${after_active}" "${before_active}" "analyze added 0 active_drops" || true
  assert_ne "${after_updated}" "${before_updated}" "analyze updated observation timestamp in ledger" || true
  assert_json_path "${sdir}/ledger-after.json" '.seen_families["e2e_used_requests_total"].last_queried_at' "ledger tracks last_queried_at" || true
  assert_json_path "${sdir}/ledger-after.json" '.query_log_coverage.covered_dates | keys | .[0]' "ledger tracks covered_dates" || true

  scenario_end
}

run_S4() {
  local sdir="${ARTIFACTS_DIR}/S4"
  mkdir -p "${sdir}"
  scenario_begin "S4" "Wildcard detection (explorer excluded, normal callers NOT flagged)"

  local ref_report="${ARTIFACTS_DIR}/S2/report.json"
  if [[ ! -f "${ref_report}" ]]; then
    run_trimtab_job "e2e-s4-analyze" "${sdir}/analyze-raw.txt" -- \
      analyze --config /etc/trimtab/trimtab.yaml --format json
    extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"
  else
    cp "${ref_report}" "${sdir}/report.json"
  fi

  assert_json_true "${sdir}/report.json" '.wildcardSources[] | select(.sourceID == "grafana-dashboard:e2e-cardinality-explorer") | .excluded' "explorer dashboard flagged & excluded" || true
  assert_json_array_not_contains "${sdir}/report.json" '[.wildcardSources[]?.sourceID]' "grafana-dashboard:e2e-overview" "normal overview dashboard NOT flagged" || true
  assert_json_array_not_contains "${sdir}/report.json" '[.wildcardSources[]?.sourceID]' "grafana-dashboard:e2e-partial-labels" "partial-labels dashboard NOT flagged" || true

  local non_excl_wc
  non_excl_wc="$(jq -c '[.wildcardSources[]? | select(.excluded == false)]' "${sdir}/report.json")"
  assert_eq "${non_excl_wc}" "[]" "all wildcard sources (dashboard_uid & query_caller) excluded" || true
  if jq -e '.wildcardSources[]? | select(.origin == "api-user")' "${sdir}/report.json" >/dev/null 2>&1; then
    assert_json_true "${sdir}/report.json" '.wildcardSources[] | select(.origin == "api-user") | .excluded' "api-user explorer query excluded via query_caller (user_agent)" || true
  fi

  scenario_end
}

run_S5() {
  local sdir="${ARTIFACTS_DIR}/S5"
  mkdir -p "${sdir}"
  scenario_begin "S5" "Self-query exclusion (Trimtab queries logged by Mimir but excluded)"

  # Run a fresh analyze (--no-state-write) immediately after S2/S3 so S2/S3 collapse queries are within the 20m window.
  run_trimtab_job "e2e-s5-analyze" "${sdir}/analyze-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json --no-state-write
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"

  loki_query_range '{namespace="monitoring", component="query-frontend"} |= "trimtab/"' 1800 > "${sdir}/loki-self-queries.json"
  local self_hits
  self_hits="$(jq -r '[.data.result[]?.values[]?] | length' "${sdir}/loki-self-queries.json")"
  assert_gt "${self_hits}" "0" "Mimir logged Trimtab User-Agent queries to Loki" || true

  local excl_cnt sample_line
  excl_cnt="$(jq -r '.safety.excludedQueries' "${sdir}/report.json")"
  assert_gt "${excl_cnt}" "0" "excludedQueries > 0" || true
  assert_json_eq "${sdir}/report.json" '.regrets // [] | length' "0" "self-queries triggered 0 regrets" || true
  sample_line="$(jq -r '.data.result[0].values[0][1] // ""' "${sdir}/loki-self-queries.json")"
  echo "${sample_line}" > "${sdir}/sample-mimir-query-log.txt"
  scenario_note "Loki self-query matches=${self_hits}, report.safety.excludedQueries=${excl_cnt}"

  scenario_end
}

run_S6() {
  local sdir="${ARTIFACTS_DIR}/S6"
  mkdir -p "${sdir}"
  scenario_begin "S6" "Label precision (scalar binary & without-collapse honesty)"

  local ref_report="${ARTIFACTS_DIR}/S2/report.json"
  if [[ ! -f "${ref_report}" ]]; then
    run_trimtab_job "e2e-s6-analyze" "${sdir}/analyze-raw.txt" -- \
      analyze --config /etc/trimtab/trimtab.yaml --format json
    extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"
  else
    cp "${ref_report}" "${sdir}/report.json"
  fi

  # 1. e2e_labels_http_duration_seconds: single pod_uid alone == 0%, combined > 0%, le protected
  assert_json_array_not_contains "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_labels_http_duration_seconds") | .unusedLabels)' "le" "le is protected (never in unusedLabels)" || true
  assert_json_array_contains "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_labels_http_duration_seconds") | .unusedLabels)' "pod_uid" "pod_uid in unusedLabels" || true
  assert_json_array_contains "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_labels_http_duration_seconds") | .unusedLabels)' "pod" "pod in unusedLabels" || true
  assert_json_eq "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_labels_http_duration_seconds") | .singleLabelEstimates[] | select(.droppedLabels == ["pod_uid"]) | .savingsPercent)' "0" "single-label without(pod_uid) savings == 0%" || true
  local comb_pct
  comb_pct="$(jq -r '(.labelCollapseAdvisories[] | select(.familyName == "e2e_labels_http_duration_seconds") | .combinedEstimate.savingsPercent) // 0' "${sdir}/report.json")"
  assert_gt "${comb_pct}" "0" "combined without(...) savings > 0%" || true

  # 2. e2e_scalar_cmp_total: job used, env & tier unused (proving scalar binary didn't mark preservesAll=true)
  assert_json_array_not_contains "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_scalar_cmp_total") | .unusedLabels)' "job" "e2e_scalar_cmp_total job marked used" || true
  assert_json_array_contains "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_scalar_cmp_total") | .unusedLabels)' "env" "e2e_scalar_cmp_total env marked unused" || true
  assert_json_array_contains "${sdir}/report.json" '(.labelCollapseAdvisories[] | select(.familyName == "e2e_scalar_cmp_total") | .unusedLabels)' "tier" "e2e_scalar_cmp_total tier marked unused" || true

  scenario_end
}

run_S12() {
  local sdir="${ARTIFACTS_DIR}/S12"
  mkdir -p "${sdir}"
  scenario_begin "S12" "Observation window protects newly introduced metric"

  kubectl -n "${MONITORING_NAMESPACE}" set env deployment/noisy-app ENABLE_NEW_METRIC=true >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/noisy-app --timeout=180s >/dev/null
  wait_mimir_series_nonzero "e2e_new_metric" 180

  run_trimtab_job "e2e-s12-analyze" "${sdir}/analyze-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"

  local prot_reason
  prot_reason="$(jq -r '.protectedReasons["e2e_new_metric"] // ""' "${sdir}/report.json")"
  assert_contains "${prot_reason}" "minObservationWindow" "e2e_new_metric protected by minObservationWindow (${prot_reason})" || true

  scenario_end
}

run_S9() {
  local sdir="${ARTIFACTS_DIR}/S9"
  mkdir -p "${sdir}"
  scenario_begin "S9" "Rotation survival (Loki window < minObservationWindow protects early-queried metric via ledger)"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"
  local rot_lq
  rot_lq="$(jq -r '.seen_families["e2e_rotation_gauge"].last_queried_at // ""' "${sdir}/ledger-before.json")"
  assert_ne "${rot_lq}" "" "ledger has prior last_queried_at for e2e_rotation_gauge (${rot_lq})" || true

  # Build temporary ConfigMap with loki.window = 5m (< 20m minObservationWindow)
  local cm_yaml
  cm_yaml="$(kubectl -n "${TRIMTAB_NAMESPACE}" get cm "$(trimtab_cm_name)" -o json | jq -r '.data["trimtab.yaml"]' | sed 's/window: 20m/window: 5m/')"
  kubectl -n "${TRIMTAB_NAMESPACE}" create configmap trimtab-e2e-s9 \
    --from-literal=trimtab.yaml="${cm_yaml}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # Wait until ROTATION_ACTIVE=false has been off for >5m so Loki's 5m window contains 0 queries for e2e_rotation_gauge
  local start_ts
  start_ts="$(date +%s)"
  while true; do
    local resp hits
    resp="$(loki_query_range '{namespace="monitoring", component="query-frontend"} |= "e2e_rotation_gauge"' 310 2>/dev/null || echo '{}')"
    hits="$(jq -r '[.data.result[]?.values[]?] | length' <<<"${resp}")"
    if (( hits == 0 )); then
      echo "  [wait] Verified 0 e2e_rotation_gauge queries in trailing 5m Loki window"
      break
    fi
    if (( $(date +%s) - start_ts >= 360 )); then
      echo "  [warn] e2e_rotation_gauge still appeared in trailing 310s (hits=${hits}), proceeding..."
      break
    fi
    echo "  [wait] Waiting for e2e_rotation_gauge queries to age past 5m Loki window (hits=${hits})..."
    sleep 15
  done

  run_trimtab_job "e2e-s9-analyze" "${sdir}/analyze-raw.txt" trimtab-e2e-s9 -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json --no-state-write
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"
  kubectl -n "${TRIMTAB_NAMESPACE}" delete configmap trimtab-e2e-s9 --ignore-not-found >/dev/null

  local rot_reason
  rot_reason="$(jq -r '.protectedReasons["e2e_rotation_gauge"] // ""' "${sdir}/report.json")"
  assert_contains "${rot_reason}" "ledger" "e2e_rotation_gauge protected via ledger rotation survival (${rot_reason})" || true
  assert_json_false "${sdir}/report.json" '.safety.baselineDriftDetected' "narrowing loki.window (20m -> 5m) does not trip query_log_daily_rate drift" || true

  local cov_days
  cov_days="$(jq -r '.safety.queryLogCoveredDays' "${sdir}/report.json")"
  scenario_note "With 5m Loki window & 20m policy window: rot_reason='${rot_reason}', queryLogCoveredDays=${cov_days}, baselineDriftDetected=false"

  scenario_end
}

run_S7() {
  local sdir="${ARTIFACTS_DIR}/S7"
  mkdir -p "${sdir}"
  scenario_begin "S7" "Activate -> apply -> confirm -> soak -> tranche 2"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"

  # 1. Activate tranche 1
  run_trimtab_job "e2e-s7-act1" "${sdir}/synth-t1.log" -- \
    synth --config /etc/trimtab/trimtab.yaml --activate
  fetch_pvc_file "trimtab.alloy" "${sdir}/trimtab-t1.alloy"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-t1-pending.json"

  assert_json_eq "${sdir}/ledger-t1-pending.json" '.tranches[0].status' "pending_confirmation" "tranche 1 entered pending_confirmation" || true
  assert_json_array_contains "${sdir}/ledger-t1-pending.json" '.tranches[0].families' "e2e_unused_big_gauge" "tranche 1 drops largest family e2e_unused_big_gauge" || true
  if [[ -f "${ARTIFACTS_DIR}/S2/report.json" ]]; then
    assert_json_eq "${ARTIFACTS_DIR}/S2/report.json" '.tranches[0].items[0].familyName' "e2e_unused_big_gauge" "largest droppable family e2e_unused_big_gauge sorted first in tranche 1 plan" || true
  fi
  assert_contains "$(cat "${sdir}/trimtab-t1.alloy")" "e2e_unused_big_gauge" "trimtab-t1.alloy drops e2e_unused_big_gauge" || true

  # 2. Analyze BEFORE apply: must stay pending_confirmation (soak NOT started)
  run_trimtab_job "e2e-s7-before-apply" "${sdir}/analyze-before-apply-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json
  extract_json_stdout "${sdir}/analyze-before-apply-raw.txt" "${sdir}/report-before-apply.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before-apply.json"

  assert_json_true "${sdir}/report-before-apply.json" '.safety.pendingConfirmation' "pendingConfirmation == true before Alloy apply" || true
  assert_json_eq "${sdir}/ledger-before-apply.json" '.tranches[0].status' "pending_confirmation" "tranche 1 still pending_confirmation before apply" || true

  # 3. Apply Alloy module & wait for all tranche 1 families to go stale in Mimir
  apply_alloy_module "${sdir}/trimtab-t1.alloy"
  local t1_fams fam stale_ok=true
  t1_fams="$(jq -r '.tranches[0].families[]?' "${sdir}/ledger-t1-pending.json")"
  for fam in ${t1_fams}; do
    if ! wait_mimir_series_zero "${fam}" "${STALENESS_WAIT_SECONDS}"; then
      scenario_fail "timed out waiting for tranche 1 family ${fam} to go stale in Mimir"
      stale_ok=false
    fi
  done

  if "${stale_ok}"; then
    # 4. Analyze AFTER staleness: should transition to soaking with confirmed_at
    run_trimtab_job "e2e-s7-confirm" "${sdir}/analyze-confirm-raw.txt" -- \
      analyze --config /etc/trimtab/trimtab.yaml --format json
    extract_json_stdout "${sdir}/analyze-confirm-raw.txt" "${sdir}/report-soaking.json"
    fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-soaking.json"

    assert_json_eq "${sdir}/ledger-soaking.json" '.tranches[0].status' "soaking" "tranche 1 transitioned to soaking" || true
    assert_json_path "${sdir}/ledger-soaking.json" '.tranches[0].confirmed_at' "tranche 1 has confirmed_at" || true
    assert_json_true "${sdir}/report-soaking.json" '.safety.soakBlocked' "next tranche soakBlocked == true" || true

    # 5. Attempt synth --activate mid-soak: tranche 2 MUST stay blocked!
    run_trimtab_job "e2e-s7-midsoak" "${sdir}/synth-midsoak.log" -- \
      synth --config /etc/trimtab/trimtab.yaml --activate
    fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-midsoak.json"
    local mid_tranches
    mid_tranches="$(jq -r '.tranches | length' "${sdir}/ledger-midsoak.json")"
    assert_eq "${mid_tranches}" "1" "tranche 2 blocked during soak (still 1 tranche)" || true

    # 6. Wait for soakWindow (5m) to elapse, then activate tranche 2
    local soak_until soak_epoch now_epoch rem
    soak_until="$(jq -r '.tranches[0].soak_until' "${sdir}/ledger-soaking.json")"
    soak_epoch="$(date -d "${soak_until}" +%s 2>/dev/null || echo $(( $(date +%s) + SOAK_WAIT_SECONDS )))"
    now_epoch="$(date +%s)"
    rem=$(( soak_epoch - now_epoch + 5 ))
    if (( rem > 0 )); then
      echo "  [wait] Waiting ${rem}s until soak_until (${soak_until})..."
      local elapsed=0
      while (( elapsed < rem )); do
        local sl=30
        if (( rem - elapsed < sl )); then sl=$(( rem - elapsed )); fi
        sleep "${sl}"
        elapsed=$(( elapsed + sl ))
      done
    fi

    run_trimtab_job "e2e-s7-act2" "${sdir}/synth-t2.log" -- \
      synth --config /etc/trimtab/trimtab.yaml --activate
    fetch_pvc_file "trimtab.alloy" "${sdir}/trimtab-t2.alloy"
    fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json"

    local t2_len
    t2_len="$(jq -r '.tranches | length' "${sdir}/ledger-after.json")"
    assert_ge "${t2_len}" "2" "tranche 2 activated after soak (${t2_len} tranches)" || true
    assert_contains "$(cat "${sdir}/trimtab-t2.alloy")" "e2e_regret_victim_gauge" "synthesized artifact includes e2e_regret_victim_gauge" || true
    assert_contains "$(cat "${sdir}/trimtab-t2.alloy")" "e2e_latency_seconds_bucket" "tranche 2 includes bucket-only drop for e2e_latency_seconds" || true

    # Apply tranche 2 so S8 (Regret) tests against dropped telemetry & S14 restores multiple tranches
    apply_alloy_module "${sdir}/trimtab-t2.alloy"
    wait_mimir_series_zero "e2e_regret_victim_gauge" "${STALENESS_WAIT_SECONDS}" || scenario_fail "timed out waiting for e2e_regret_victim_gauge to go stale"
  fi

  scenario_end
}

run_S8() {
  local sdir="${ARTIFACTS_DIR}/S8"
  mkdir -p "${sdir}"
  scenario_begin "S8" "Forward-only Regret detection"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"

  # Enable regret mode -> traffic-gen queries e2e_regret_victim_gauge every 15s
  kubectl -n "${MONITORING_NAMESPACE}" set env deployment/traffic-gen REGRET_MODE=true >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/traffic-gen --timeout=120s >/dev/null
  wait_loki_has_logs '{namespace="monitoring", component="query-frontend"} |= "e2e_regret_victim_gauge"' 120 120

  local rc=0
  run_trimtab_job "e2e-s8-analyze" "${sdir}/analyze-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json || rc=$?
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json"

  assert_json_array_contains "${sdir}/report.json" '[.regrets[]?.familyName]' "e2e_regret_victim_gauge" "RegretEvent emitted for e2e_regret_victim_gauge" || true
  assert_json_array_contains "${sdir}/ledger-after.json" '.pinned_never_drop' "e2e_regret_victim_gauge" "e2e_regret_victim_gauge pinned to neverDrop" || true
  assert_json_array_contains "${sdir}/ledger-after.json" '.tranches[-1].pending_revert_families // []' "e2e_regret_victim_gauge" "pending_revert_families tracks e2e_regret_victim_gauge" || true
  local still_active
  still_active="$(jq -r '.active_drops["e2e_regret_victim_gauge"] // "null"' "${sdir}/ledger-after.json")"
  assert_eq "${still_active}" "null" "e2e_regret_victim_gauge removed from active_drops" || true
  assert_json_eq "${sdir}/ledger-after.json" '.tranches[-1].status' "blocked_regret" "latest tranche marked blocked_regret" || true
  assert_eq "${rc}" "3" "analyze exits 3 on RegretEvent" || true

  # Re-synthesize and apply to restore e2e_regret_victim_gauge telemetry!
  run_trimtab_job "e2e-s8-resynth" "${sdir}/synth-revert.log" -- \
    synth --config /etc/trimtab/trimtab.yaml
  fetch_pvc_file "trimtab.alloy" "${sdir}/trimtab-reverted.alloy"
  assert_not_contains "$(cat "${sdir}/trimtab-reverted.alloy")" "e2e_regret_victim_gauge" "re-synthesized artifact removes e2e_regret_victim_gauge drop" || true
  apply_alloy_module "${sdir}/trimtab-reverted.alloy"
  wait_mimir_series_nonzero "e2e_regret_victim_gauge" 180 || scenario_fail "e2e_regret_victim_gauge did not reappear after reverted artifact applied"

  # Run post-recovery analyze: pending_revert_families clears automatically, while blocked_regret stays until --resolve-regret
  run_trimtab_job "e2e-s8-post-revert" "${sdir}/analyze-post-revert-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json || true
  extract_json_stdout "${sdir}/analyze-post-revert-raw.txt" "${sdir}/report-post-revert.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-post-revert.json"
  assert_json_eq "${sdir}/ledger-post-revert.json" '.tranches[-1].pending_revert_families // [] | length' "0" "pending_revert_families cleared once series returned in Mimir" || true
  assert_json_eq "${sdir}/ledger-post-revert.json" '.tranches[-1].status' "blocked_regret" "blocked_regret persists until operator runs --resolve-regret" || true

  # Disable REGRET_MODE and resolve the regret block via --resolve-regret
  kubectl -n "${MONITORING_NAMESPACE}" set env deployment/traffic-gen REGRET_MODE=false >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/traffic-gen --timeout=120s >/dev/null

  run_trimtab_job "e2e-s8-resolve" "${sdir}/analyze-resolve-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json --resolve-regret
  extract_json_stdout "${sdir}/analyze-resolve-raw.txt" "${sdir}/report-resolved.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-resolved.json"
  assert_json_eq "${sdir}/report-resolved.json" '.safety.unresolvedRegrets' "0" "unresolvedRegrets == 0 after --resolve-regret" || true
  assert_ne "$(jq -r '.tranches[-1].status' "${sdir}/ledger-resolved.json")" "blocked_regret" "tranche unblocked after --resolve-regret" || true
  assert_json_array_contains "${sdir}/ledger-resolved.json" '.pinned_never_drop' "e2e_regret_victim_gauge" "e2e_regret_victim_gauge stays pinned to neverDrop" || true

  scenario_end
}

run_S10() {
  local sdir="${ARTIFACTS_DIR}/S10"
  mkdir -p "${sdir}"
  scenario_begin "S10" "Baseline drift circuit breaker & high-water mark"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"
  local base_dash
  base_dash="$(jq -r '.baseline_counts.dashboards // 0' "${sdir}/ledger-before.json")"
  assert_eq "${base_dash}" "7" "initial Grafana dashboard HWM baseline == 7" || true

  # 1. Delete 1 dashboard (7 -> 6, -14.3% < 20% threshold): allowed, HWM MUST remain 7!
  grafana_api DELETE "/api/dashboards/uid/e2e-aux-4" >/dev/null
  run_trimtab_job "e2e-s10-drop1" "${sdir}/analyze-drop1-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json
  extract_json_stdout "${sdir}/analyze-drop1-raw.txt" "${sdir}/report-drop1.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-drop1.json"

  assert_json_false "${sdir}/report-drop1.json" '.safety.baselineDriftDetected' "14.3% drop does not trip drift guard" || true
  assert_json_eq "${sdir}/ledger-drop1.json" '.baseline_counts.dashboards' "7" "HWM baseline remained 7 after 7->6 drop" || true

  # 2. Delete 1 more dashboard (6 -> 5, 28.6% below HWM 7 even though only 16.7% below 6): MUST trip!
  grafana_api DELETE "/api/dashboards/uid/e2e-aux-3" >/dev/null
  run_trimtab_job "e2e-s10-drop2" "${sdir}/analyze-drop2-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json
  extract_json_stdout "${sdir}/analyze-drop2-raw.txt" "${sdir}/report-drop2.json"
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-drop2.json"

  assert_json_true "${sdir}/report-drop2.json" '.safety.baselineDriftDetected' "cumulative HWM drop (7->5 = 28.6%) tripped drift guard" || true
  assert_json_true "${sdir}/report-drop2.json" '.safety.blocked' "synthesis blocked on baseline drift" || true

  # 3. Restore deleted dashboards and verify --accept-baseline-shift clears/accepts baseline cleanly
  run_trimtab_job "e2e-s10-accept" "${sdir}/analyze-accept-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json --accept-baseline-shift
  extract_json_stdout "${sdir}/analyze-accept-raw.txt" "${sdir}/report-accept.json"
  assert_json_false "${sdir}/report-accept.json" '.safety.baselineDriftDetected' "--accept-baseline-shift unblocked drift" || true

  # Re-seed restored dashboards & wait for Grafana RBAC cache before resetting baseline HWM back to 7
  grafana_api POST "/api/dashboards/db" "$(cat "${SCRIPT_DIR}/workloads/dashboards/e2e-aux-3.json")" >/dev/null
  grafana_api POST "/api/dashboards/db" "$(cat "${SCRIPT_DIR}/workloads/dashboards/e2e-aux-4.json")" >/dev/null
  local sa_token rbac_start
  sa_token="$(kubectl -n "${TRIMTAB_NAMESPACE}" get secret trimtab-credentials -o jsonpath='{.data.GRAFANA_API_KEY}' | base64 -d)"
  rbac_start="$(date +%s)"
  while (( $(date +%s) - rbac_start < 180 )); do
    if cluster_curl -sf -H "Authorization: Bearer ${sa_token}" \
      "http://grafana.${MONITORING_NAMESPACE}.svc.cluster.local:80/api/dashboards/uid/e2e-aux-3" >/dev/null 2>&1; then
      break
    fi
    sleep 5
  done
  run_trimtab_job "e2e-s10-restore" "${sdir}/analyze-restore-raw.txt" -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json --accept-baseline-shift >/dev/null 2>&1 || true
  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json"

  scenario_end
}

run_S11() {
  local sdir="${ARTIFACTS_DIR}/S11"
  mkdir -p "${sdir}"
  scenario_begin "S11" "Query-log outage / sanity ceiling prevents mass drop"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"

  # Break the Loki LogQL selector in a temporary ConfigMap
  local broken_yaml
  broken_yaml="$(kubectl -n "${TRIMTAB_NAMESPACE}" get cm "$(trimtab_cm_name)" -o json | jq -r '.data["trimtab.yaml"]' | sed 's/component="query-frontend"/component="broken-query-frontend"/')"
  kubectl -n "${TRIMTAB_NAMESPACE}" create configmap trimtab-e2e-s11 \
    --from-literal=trimtab.yaml="${broken_yaml}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  run_trimtab_job "e2e-s11-analyze" "${sdir}/analyze-raw.txt" trimtab-e2e-s11 -- \
    analyze --config /etc/trimtab/trimtab.yaml --format json --no-state-write || true
  extract_json_stdout "${sdir}/analyze-raw.txt" "${sdir}/report.json"

  run_trimtab_job "e2e-s11-synth" "${sdir}/synth-out.txt" trimtab-e2e-s11 -- \
    synth --config /etc/trimtab/trimtab.yaml --out - --no-state-write > "${sdir}/synth-stdout.alloy" 2>&1 || true

  kubectl -n "${TRIMTAB_NAMESPACE}" delete configmap trimtab-e2e-s11 --ignore-not-found >/dev/null

  assert_json_eq "${sdir}/report.json" '.summary.synthesizedFamilies' "0" "0 synthesized families on Loki outage" || true
  assert_not_contains "$(cat "${sdir}/synth-stdout.alloy")" 'action        = "drop"' "synth emitted pass-through on Loki outage" || true

  local drift sanity ro reasons
  drift="$(jq -r '.safety.baselineDriftDetected' "${sdir}/report.json")"
  sanity="$(jq -r '.safety.sanityCeilingTripped' "${sdir}/report.json")"
  ro="$(jq -r '.safety.reportOnly' "${sdir}/report.json")"
  reasons="$(jq -c '.safety.blockReasons // []' "${sdir}/report.json")"
  scenario_note "Guards tripped: baselineDrift=${drift}, sanityCeiling=${sanity}, reportOnly=${ro}, blockReasons=${reasons}"

  scenario_end
}

run_S13() {
  local sdir="${ARTIFACTS_DIR}/S13"
  mkdir -p "${sdir}"
  scenario_begin "S13" "Persistence & CronJob ledger survival across runs"

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-before.json"
  local ts_before
  ts_before="$(jq -r '.updated_at' "${sdir}/ledger-before.json")"

  # Unsuspend the chart CronJob (schedule */2 * * * *) and wait for a real CronJob-scheduled Job to finish
  kubectl -n "${TRIMTAB_NAMESPACE}" patch cronjob trimtab -p '{"spec":{"suspend":false}}' >/dev/null

  local prev_job start_ts new_job=""
  prev_job="$(kubectl -n "${TRIMTAB_NAMESPACE}" get jobs -l app.kubernetes.io/instance=trimtab --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}' 2>/dev/null || echo "")"
  start_ts="$(date +%s)"
  while true; do
    new_job="$(kubectl -n "${TRIMTAB_NAMESPACE}" get jobs -l app.kubernetes.io/instance=trimtab --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}' 2>/dev/null || echo "")"
    if [[ -n "${new_job}" && "${new_job}" != "${prev_job}" ]]; then
      echo "  [cronjob] Observed newly scheduled Job: ${new_job}"
      kubectl -n "${TRIMTAB_NAMESPACE}" wait --for=condition=complete "job/${new_job}" --timeout=180s >/dev/null
      kubectl -n "${TRIMTAB_NAMESPACE}" logs "job/${new_job}" > "${sdir}/cronjob-logs.txt" 2>&1 || true
      break
    fi
    if (( $(date +%s) - start_ts >= 180 )); then
      scenario_fail "timed out waiting for CronJob to trigger a scheduled Job"
      break
    fi
    echo "  [wait] Waiting for CronJob schedule (*/2 * * * *) to trigger..."
    sleep 10
  done

  kubectl -n "${TRIMTAB_NAMESPACE}" patch cronjob trimtab -p '{"spec":{"suspend":true}}' >/dev/null

  fetch_pvc_file ".trimtab-state.json" "${sdir}/ledger-after.json"
  local ts_after
  ts_after="$(jq -r '.updated_at' "${sdir}/ledger-after.json")"

  assert_ne "${ts_after}" "${ts_before}" "CronJob updated persisted ledger timestamp" || true
  assert_json_array_contains "${sdir}/ledger-after.json" '.pinned_never_drop' "e2e_regret_victim_gauge" "CronJob preserved pinned_never_drop across PVC mount" || true
  assert_ge "$(jq -r '.tranches | length' "${sdir}/ledger-after.json")" "2" "CronJob preserved tranche history on PVC" || true

  scenario_end
}

run_S14() {
  local sdir="${ARTIFACTS_DIR}/S14"
  mkdir -p "${sdir}"
  scenario_begin "S14" "Kill switch (pass-through restores all dropped series)"

  render_passthrough_alloy "${sdir}/passthrough.alloy"
  apply_alloy_module "${sdir}/passthrough.alloy"
  wait_mimir_series_ge "e2e_unused_big_gauge" 600 180 || scenario_fail "timed out waiting for all 600 e2e_unused_big_gauge series to restore"

  local restored_cnt
  restored_cnt="$(mimir_series_count "e2e_unused_big_gauge")"
  assert_ge "${restored_cnt}" "600" "e2e_unused_big_gauge restored in Mimir (>= 600 series)" || true

  scenario_end
}

run_single_scenario() {
  local id="$1"
  mkdir -p "${ARTIFACTS_DIR}"
  case "${id}" in
    S0)  run_S0 ;;
    S1)  run_S1 ;;
    S2)  run_S2 ;;
    S3)  run_S3 ;;
    S4)  run_S4 ;;
    S5)  run_S5 ;;
    S6)  run_S6 ;;
    S7)  run_S7 ;;
    S8)  run_S8 ;;
    S9)  run_S9 ;;
    S10) run_S10 ;;
    S11) run_S11 ;;
    S12) run_S12 ;;
    S13) run_S13 ;;
    S14) run_S14 ;;
    *)
      echo "ERROR: unknown scenario '${id}' (expected S0..S14)" >&2
      exit 2
      ;;
  esac
  snapshot_artifacts_pod_logs
  write_summary_md "${ARTIFACTS_DIR}/summary.md" "${ARTIFACTS_DIR}"
}

cmd_test() {
  require_prereqs
  use_kind_context
  ensure_curl_pod
  mkdir -p "${ARTIFACTS_DIR}"
  echo ">>> [test] Writing E2E artifacts to ${ARTIFACTS_DIR}"

  run_S0
  run_S1
  wait_for_observation_window
  run_S2
  run_S3
  run_S4
  run_S5
  run_S6
  run_S12
  run_S9
  run_S7
  run_S8
  run_S10
  run_S11
  run_S13
  run_S14

  # Record explicitly skipped Kubernetes API scanner coverage
  scenario_skip "K8s-RBAC" "Live KEDA / Argo / Flagger K8s API scanning" "chart disables automountServiceAccountToken by default; covered by offline pkg/usage/k8sconsumers unit tests"

  snapshot_artifacts_pod_logs
  write_summary_md "${ARTIFACTS_DIR}/summary.md" "${ARTIFACTS_DIR}"

  echo ""
  echo "========================================================================"
  cat "${ARTIFACTS_DIR}/summary.md"
  echo "========================================================================"

  if results_have_failure; then
    echo "E2E suite completed with FAIL status(es). Inspect ${ARTIFACTS_DIR}/summary.md." >&2
    return 1
  fi
  return 0
}

cmd_scenario() {
  local sid="${1:-}"
  if [[ -z "${sid}" ]]; then
    echo "Usage: $(basename "$0") scenario <S0..S14>" >&2
    exit 2
  fi
  require_prereqs
  use_kind_context
  ensure_curl_pod
  run_single_scenario "${sid}"
  cat "${ARTIFACTS_DIR}/summary.md"
  if results_have_failure; then
    return 1
  fi
  return 0
}

cmd_artifacts() {
  if [[ ! -d "${ARTIFACTS_BASE}" ]]; then
    echo "(no artifacts directory found at ${ARTIFACTS_BASE})" >&2
    exit 1
  fi
  local latest
  latest="$(find "${ARTIFACTS_BASE}" -mindepth 1 -maxdepth 1 -type d | sort | tail -n1)"
  if [[ -z "${latest}" ]]; then
    echo "(no E2E runs recorded yet)" >&2
    exit 1
  fi
  echo "${latest}"
}

cmd_down() {
  require_prereqs
  if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
    echo ">>> [down] Deleting Kind cluster '${KIND_CLUSTER_NAME}'..."
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
  else
    echo "Kind cluster '${KIND_CLUSTER_NAME}' does not exist."
  fi
}

cmd_reset() {
  require_prereqs
  use_kind_context
  ensure_curl_pod
  echo ">>> [reset] Resetting workloads, PVC ledger, Alloy rules, and TSDB/Loki stores..."
  kubectl -n "${TRIMTAB_NAMESPACE}" patch cronjob trimtab -p '{"spec":{"suspend":true}}' >/dev/null 2>&1 || true
  kubectl -n "${MONITORING_NAMESPACE}" set env deployment/noisy-app ENABLE_NEW_METRIC=false >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" set env deployment/traffic-gen REGRET_MODE=false ROTATION_ACTIVE=true >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/noisy-app --timeout=180s >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/traffic-gen --timeout=180s >/dev/null

  reset_pvc_state

  local tmp_alloy
  tmp_alloy="$(mktemp)"
  render_passthrough_alloy "${tmp_alloy}"
  apply_alloy_module "${tmp_alloy}"
  rm -f "${tmp_alloy}"

  # Flush Alloy WAL and ephemeral Mimir/Loki emptyDir stores so prior S12 metrics cannot replay into fresh Mimir
  kubectl -n "${MONITORING_NAMESPACE}" scale deployment/alloy --replicas=0 >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout restart deployment/mimir statefulset/loki >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/mimir --timeout=180s >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status statefulset/loki --timeout=180s >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" scale deployment/alloy --replicas=1 >/dev/null
  kubectl -n "${MONITORING_NAMESPACE}" rollout status deployment/alloy --timeout=180s >/dev/null

  cmd_seed
  echo ">>> [reset] Done."
}

cmd_all() {
  cmd_up
  cmd_build
  cmd_deploy
  cmd_seed
  cmd_test
}

case "${1:-help}" in
  up)        cmd_up ;;
  build)     cmd_build ;;
  deploy)    cmd_deploy ;;
  seed)      cmd_seed ;;
  test)      cmd_test ;;
  scenario)  shift; cmd_scenario "${1:-}" ;;
  reset)     cmd_reset ;;
  artifacts) cmd_artifacts ;;
  down)      cmd_down ;;
  all)       cmd_all ;;
  help|-h|--help) usage ;;
  *)
    echo "ERROR: unknown subcommand '$1'" >&2
    usage >&2
    exit 2
    ;;
esac
