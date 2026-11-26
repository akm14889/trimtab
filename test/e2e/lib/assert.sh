#!/usr/bin/env bash
# jq-based assertion helpers and scenario summary tracker for Trimtab E2E tests.

E2E_CUR_SCENARIO=""
E2E_CUR_TITLE=""
E2E_CUR_STATUS="PASS"
E2E_CUR_NOTES=()
E2E_RESULTS=()

scenario_begin() {
  E2E_CUR_SCENARIO="$1"
  E2E_CUR_TITLE="$2"
  E2E_CUR_STATUS="PASS"
  E2E_CUR_NOTES=()
  echo ""
  echo "========================================================================"
  echo "[SCENARIO ${E2E_CUR_SCENARIO}] ${E2E_CUR_TITLE}"
  echo "========================================================================"
}

scenario_note() {
  E2E_CUR_NOTES+=("$1")
  echo "  note: $1"
}

scenario_finding() {
  if [[ "${E2E_CUR_STATUS}" != "FAIL" ]]; then
    E2E_CUR_STATUS="FINDING"
  fi
  E2E_CUR_NOTES+=("FINDING: $1")
  echo "  [FINDING] $1"
}

scenario_fail() {
  E2E_CUR_STATUS="FAIL"
  E2E_CUR_NOTES+=("FAIL: $1")
  echo "  [FAIL] $1" >&2
}

scenario_skip() {
  local id="$1"
  local title="$2"
  local reason="$3"
  E2E_RESULTS+=("${id}|${title}|SKIPPED|${reason}")
  echo "[SCENARIO ${id}] ${title} -> SKIPPED (${reason})"
}

scenario_end() {
  local joined=""
  if [[ ${#E2E_CUR_NOTES[@]} -gt 0 ]]; then
    local IFS="; "
    joined="${E2E_CUR_NOTES[*]}"
  fi
  joined="${joined//$'\n'/ }"
  joined="${joined//|//}"
  E2E_RESULTS+=("${E2E_CUR_SCENARIO}|${E2E_CUR_TITLE}|${E2E_CUR_STATUS}|${joined}")
  echo "=> [${E2E_CUR_SCENARIO}] Result: ${E2E_CUR_STATUS}"
}

assert_eq() {
  local actual="$1"
  local expected="$2"
  local msg="${3:-expected equality}"
  if [[ "${actual}" == "${expected}" ]]; then
    echo "  [OK] ${msg}: ${actual}"
    return 0
  fi
  scenario_fail "${msg}: got '${actual}', want '${expected}'"
  return 1
}

assert_ne() {
  local actual="$1"
  local unexpected="$2"
  local msg="${3:-expected inequality}"
  if [[ "${actual}" != "${unexpected}" ]]; then
    echo "  [OK] ${msg}: '${actual}' != '${unexpected}'"
    return 0
  fi
  scenario_fail "${msg}: got unexpected '${actual}'"
  return 1
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="${3:-expected substring}"
  if grep -Fq -- "${needle}" <<<"${haystack}"; then
    echo "  [OK] ${msg} (found '${needle}')"
    return 0
  fi
  scenario_fail "${msg}: substring '${needle}' not found"
  return 1
}

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="${3:-expected substring absent}"
  if ! grep -Fq -- "${needle}" <<<"${haystack}"; then
    echo "  [OK] ${msg} ('${needle}' absent)"
    return 0
  fi
  scenario_fail "${msg}: unexpected substring '${needle}' found"
  return 1
}

assert_gt() {
  local actual="$1"
  local threshold="$2"
  local msg="${3:-expected > ${threshold}}"
  if awk -v a="${actual}" -v b="${threshold}" 'BEGIN{exit !(a > b)}'; then
    echo "  [OK] ${msg}: ${actual} > ${threshold}"
    return 0
  fi
  scenario_fail "${msg}: got ${actual}, want > ${threshold}"
  return 1
}

assert_ge() {
  local actual="$1"
  local threshold="$2"
  local msg="${3:-expected >= ${threshold}}"
  if awk -v a="${actual}" -v b="${threshold}" 'BEGIN{exit !(a >= b)}'; then
    echo "  [OK] ${msg}: ${actual} >= ${threshold}"
    return 0
  fi
  scenario_fail "${msg}: got ${actual}, want >= ${threshold}"
  return 1
}

assert_le() {
  local actual="$1"
  local threshold="$2"
  local msg="${3:-expected <= ${threshold}}"
  if awk -v a="${actual}" -v b="${threshold}" 'BEGIN{exit !(a <= b)}'; then
    echo "  [OK] ${msg}: ${actual} <= ${threshold}"
    return 0
  fi
  scenario_fail "${msg}: got ${actual}, want <= ${threshold}"
  return 1
}

json_get() {
  local file="$1"
  local expr="$2"
  jq -r "${expr}" "${file}"
}

assert_json_path() {
  local file="$1"
  local expr="$2"
  local msg="${3:-json path ${expr}}"
  local val
  val="$(jq -r "${expr}" "${file}" 2>/dev/null || echo "__JQ_ERROR__")"
  if [[ "${val}" != "null" && "${val}" != "__JQ_ERROR__" && -n "${val}" ]]; then
    echo "  [OK] ${msg}: ${val}"
    return 0
  fi
  scenario_fail "${msg}: path '${expr}' was missing/null/empty (got '${val}')"
  return 1
}

assert_json_eq() {
  local file="$1"
  local expr="$2"
  local expected="$3"
  local msg="${4:-json ${expr} == ${expected}}"
  local val
  val="$(jq -r "${expr}" "${file}" 2>/dev/null || echo "__JQ_ERROR__")"
  assert_eq "${val}" "${expected}" "${msg}"
}

assert_json_true() {
  local file="$1"
  local expr="$2"
  local msg="${3:-json ${expr} == true}"
  assert_json_eq "${file}" "${expr}" "true" "${msg}"
}

assert_json_false() {
  local file="$1"
  local expr="$2"
  local msg="${3:-json ${expr} == false}"
  assert_json_eq "${file}" "${expr}" "false" "${msg}"
}

assert_json_array_contains() {
  local file="$1"
  local array_expr="$2"
  local item="$3"
  local msg="${4:-${array_expr} contains ${item}}"
  local found
  found="$(jq -r --arg item "${item}" "(${array_expr} // []) | index(\$item) != null" "${file}" 2>/dev/null || echo "false")"
  if [[ "${found}" == "true" ]]; then
    echo "  [OK] ${msg}"
    return 0
  fi
  scenario_fail "${msg}: '${item}' not present"
  return 1
}

assert_json_array_not_contains() {
  local file="$1"
  local array_expr="$2"
  local item="$3"
  local msg="${4:-${array_expr} excludes ${item}}"
  local found
  found="$(jq -r --arg item "${item}" "(${array_expr} // []) | index(\$item) != null" "${file}" 2>/dev/null || echo "false")"
  if [[ "${found}" == "false" ]]; then
    echo "  [OK] ${msg}"
    return 0
  fi
  scenario_fail "${msg}: unexpected '${item}' present"
  return 1
}

write_summary_md() {
  local out_file="$1"
  local artifacts_dir="$2"
  {
    echo "# Trimtab E2E Summary"
    echo ""
    echo "- **Generated (UTC):** $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "- **Artifacts Directory:** \`${artifacts_dir}\`"
    echo ""
    echo "| Scenario | Title | Status | Notes |"
    echo "| :--- | :--- | :--- | :--- |"
    local row id title status notes
    for row in "${E2E_RESULTS[@]}"; do
      IFS='|' read -r id title status notes <<<"${row}"
      echo "| **${id}** | ${title} | **${status}** | ${notes} |"
    done
  } > "${out_file}"
}

results_have_failure() {
  local row id title status notes
  for row in "${E2E_RESULTS[@]}"; do
    IFS='|' read -r id title status notes <<<"${row}"
    if [[ "${status}" == "FAIL" ]]; then
      return 0
    fi
  done
  return 1
}
