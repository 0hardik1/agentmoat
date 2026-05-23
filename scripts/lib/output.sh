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
E2E_REPLAY_COMMANDS=()

# Optional footer line (artifact path, KEEP_CLUSTER hint) set by e2e.sh cleanup.
E2E_ARTIFACT_HINT="${E2E_ARTIFACT_HINT:-}"

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

# e2e_step_pass records a passed phase and prints e2e_pass. An optional
# second argument is the shell command to replay that step manually.
e2e_step_pass() {
  local step="$1"
  local replay="${2:-}"
  E2E_PASSED_STEPS+=("$step")
  E2E_REPLAY_COMMANDS+=("$replay")
  e2e_pass "$step"
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

# e2e_finish prints the final SUMMARY table and pass/fail banner.
_e2e_replay_pad() {
  local step_col="$1"
  echo $((2 + 6 + 2 + step_col + 2))
}

_e2e_print_replay_row() {
  local status="$1"
  local step="$2"
  local replay="$3"
  local step_col="$4"
  local first=1
  local line pad

  pad=$(_e2e_replay_pad "$step_col")
  while IFS= read -r line || [[ -n "$line" ]]; do
    if (( first )); then
      if _am_use_color; then
        printf '  '
        _am_success "$(printf '%-6s  %-*s  %s' "$status" "$step_col" "$step" "$line")"
        printf '\n'
      else
        printf '  %-6s  %-*s  %s\n' "$status" "$step_col" "$step" "$line"
      fi
      first=0
    elif _am_use_color; then
      printf '%*s' "$pad" ''
      _am_success "$line"
      printf '\n'
    else
      printf '%*s%s\n' "$pad" '' "$line"
    fi
  done <<< "$replay"
}

_e2e_print_muted_block() {
  local text="$1"
  local line
  while IFS= read -r line || [[ -n "$line" ]]; do
    _am_muted "$line"
    printf '\n'
  done <<< "$text"
}

e2e_finish() {
  local code="${1:-0}"
  local total="${#E2E_PASSED_STEPS[@]}"
  local i step replay step_width=4
  local step_col=32
  local replay_col=40

  printf '\n'
  _am_bold "SUMMARY"
  printf '\n'

  if (( total > 0 )); then
    for step in "${E2E_PASSED_STEPS[@]}"; do
      if (( ${#step} > step_width )); then
        step_width=${#step}
      fi
    done
    if (( step_width < 4 )); then
      step_width=4
    fi
    if (( step_width > step_col )); then
      step_col=$step_width
    fi

    printf '\n'
    if _am_use_color; then
      printf '  '
      _am_bold "$(printf '%-6s  %-*s  %s' 'STATUS' "$step_col" 'STEP' 'REPLAY')"
      printf '\n'
    else
      printf '  %-6s  %-*s  %s\n' 'STATUS' "$step_col" 'STEP' 'REPLAY'
    fi

    printf '  '
    for ((i = 0; i < 6; i++)); do printf '─'; done
    printf '  '
    for ((i = 0; i < step_col; i++)); do printf '─'; done
    printf '  '
    for ((i = 0; i < replay_col; i++)); do printf '─'; done
    printf '\n'

    for i in "${!E2E_PASSED_STEPS[@]}"; do
      step="${E2E_PASSED_STEPS[$i]}"
      replay="${E2E_REPLAY_COMMANDS[$i]:-}"
      _e2e_print_replay_row "$AM_SYMBOL_OK" "$step" "$replay" "$step_col"
    done

    printf '\n'
    if _am_use_color; then
      _am_success "$(printf '%d steps passed' "$total")"
      printf '\n'
    else
      printf '%d steps passed\n' "$total"
    fi
    if [[ -n "$E2E_ARTIFACT_HINT" ]]; then
      _e2e_print_muted_block "$E2E_ARTIFACT_HINT"
    fi
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
