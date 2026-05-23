#!/usr/bin/env bash
# scripts/lib/output.sh — terminal styling for agentmoat shell scripts.
#
# Mirrors pkg/output/style.go: same Unicode symbols, NO_COLOR aware, and
# plain text when stdout is not a TTY so CI logs stay grep-friendly.

if [[ -n "${_AGENTMOAT_OUTPUT_SH:-}" ]]; then
  return 0 2>/dev/null || exit 0
fi
_AGENTMOAT_OUTPUT_SH=1

AM_SYMBOL_OK=$'✓'
AM_SYMBOL_FAIL=$'✗'
AM_SYMBOL_WARN=$'⚠'
AM_SYMBOL_ARROW=$'→'

_am_use_color() {
  [[ -t 1 && -z "${NO_COLOR:-}" ]]
}

_am_bold() {
  if _am_use_color; then
    printf '\033[1m%s\033[0m' "$1"
  else
    printf '%s' "$1"
  fi
}

_am_muted() {
  if _am_use_color; then
    printf '\033[90m%s\033[0m' "$1"
  else
    printf '%s' "$1"
  fi
}

_am_success() {
  if _am_use_color; then
    printf '\033[32m%s\033[0m' "$1"
  else
    printf '%s' "$1"
  fi
}

_am_danger() {
  if _am_use_color; then
    printf '\033[31m%s\033[0m' "$1"
  else
    printf '%s' "$1"
  fi
}

# Populated by e2e_step_pass for the final SUMMARY block.
E2E_PASSED_STEPS=()

# AGENT_EXIT holds the exit code from the JSON capture run in
# agentmoat_table_and_json and agentmoat_explain_json.
AGENT_EXIT=0

# E2E_EXPAND_EXPLAIN: when 1, print full explain table/markdown during
# `make e2e`. Default 0 keeps explain output collapsed to a SUMMARY line.
E2E_EXPAND_EXPLAIN="${E2E_EXPAND_EXPLAIN:-0}"

# e2e_title prints the run header (like "agentmoat scan" in table output).
e2e_title() {
  printf '\n'
  _am_bold "agentmoat e2e"
  printf '\n'
}

# e2e_subtitle prints muted metadata chips on the line below the title.
e2e_subtitle() {
  _am_muted "$1"
  printf '\n\n'
}

# e2e_infra prints plain progress on stderr, matching agentmoat's Scan()
# "scanning cluster..." lines.
e2e_infra() {
  printf '%s\n' "$*" >&2
}

# e2e_step marks the start of a test phase.
e2e_step() {
  printf '\n'
  if _am_use_color; then
    printf '  %s ' "$AM_SYMBOL_ARROW"
    _am_muted "$1"
    printf '\n'
  else
    printf '  %s %s\n' "$AM_SYMBOL_ARROW" "$1"
  fi
}

# e2e_pass prints a single green check line after assertions succeed.
e2e_pass() {
  if _am_use_color; then
    printf '  %s ' "$AM_SYMBOL_OK"
    _am_success "$1"
    printf '\n'
  else
    printf '  %s %s\n' "$AM_SYMBOL_OK" "$1"
  fi
}

# e2e_step_pass records a passed phase and prints e2e_pass.
e2e_step_pass() {
  E2E_PASSED_STEPS+=("$1")
  e2e_pass "$1"
}

# e2e_collapsed prints a muted one-liner when lengthy output is hidden.
e2e_collapsed() {
  if _am_use_color; then
    printf '  '
    _am_muted "$1"
    printf '\n'
  else
    printf '  %s\n' "$1"
  fi
}

# agentmoat_explain_collapsed_summary prints a scan-style SUMMARY headline
# from an ExplainDocument JSON file, without the per-workload prose.
agentmoat_explain_collapsed_summary() {
  local json_file="$1"
  local ns total compat review incompat sections

  ns=$(jq -r '.spec.namespace.name // ""' "$json_file")
  total=$(jq -r '.spec.namespace.summary.total // 0' "$json_file")
  compat=$(jq -r '.spec.namespace.summary.compatible // 0' "$json_file")
  review=$(jq -r '.spec.namespace.summary.needsReview // 0' "$json_file")
  incompat=$(jq -r '.spec.namespace.summary.incompatible // 0' "$json_file")
  sections=$(jq -r '.spec.namespace.workloads | length' "$json_file")

  _am_bold "agentmoat explain namespace ${ns}"
  printf '\n'
  _am_bold "SUMMARY"
  printf '  %d workloads — compatible: %d, review: %d, incompatible: %d\n' \
    "$total" "$compat" "$review" "$incompat"
  e2e_collapsed "(${sections} workload section(s) collapsed; E2E_EXPAND_EXPLAIN=1 make e2e to expand)"
  printf '\n'
}

# agentmoat_explain_json captures explain output for assertions. When
# E2E_EXPAND_EXPLAIN=1 it also prints the full table renderer; otherwise
# it prints only the SUMMARY headline plus a collapse hint.
agentmoat_explain_json() {
  local json_out="$1"
  shift
  if [[ "$E2E_EXPAND_EXPLAIN" == "1" ]]; then
    agentmoat_table_and_json "$json_out" "$@"
    return
  fi
  set +e
  "$@" --output json >"$json_out"
  AGENT_EXIT=$?
  set -e
  agentmoat_explain_collapsed_summary "$json_out"
}

# agentmoat_table_and_json runs an agentmoat subcommand twice: first with
# the default table renderer (stdout), then with --output json into
# json_out. Sets AGENT_EXIT to the JSON run's exit code. Safe for
# read-only subcommands and idempotent dry-runs only.
agentmoat_table_and_json() {
  local json_out="$1"
  shift
  set +e
  "$@"
  "$@" --output json >"$json_out"
  AGENT_EXIT=$?
  set -e
}

# e2e_finish prints the final SUMMARY block and pass/fail banner.
e2e_finish() {
  local code="${1:-0}"
  local total="${#E2E_PASSED_STEPS[@]}"
  local step

  printf '\n'
  _am_bold "SUMMARY"
  if (( total > 0 )); then
    printf '  %d steps\n' "$total"
    for step in "${E2E_PASSED_STEPS[@]}"; do
      if _am_use_color; then
        printf '  %s ' "$AM_SYMBOL_OK"
        _am_success "$step"
        printf '\n'
      else
        printf '  %s %s\n' "$AM_SYMBOL_OK" "$step"
      fi
    done
  fi
  printf '\n'
  if (( code == 0 )); then
    if _am_use_color; then
      printf '%s ' "$AM_SYMBOL_OK"
      _am_success "e2e passed"
      printf '\n'
    else
      printf '%s e2e passed\n' "$AM_SYMBOL_OK"
    fi
  else
    if _am_use_color; then
      printf '%s ' "$AM_SYMBOL_FAIL"
      _am_danger "e2e failed (exit $code)"
      printf '\n'
    else
      printf '%s e2e failed (exit %d)\n' "$AM_SYMBOL_FAIL" "$code"
    fi
  fi
}
