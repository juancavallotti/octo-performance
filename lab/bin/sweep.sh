#!/usr/bin/env bash
# Grid-search the tuning knobs to find the configuration worth publishing as "tuned".
#
# Usage: sweep.sh
#
# Environment:
#   SCENARIO  (required)   TARGET native|docker (default native)   HOST (default local)
#   SWEEP_WORKERS / SWEEP_BUFFER / SWEEP_POOL   space-separated lists; default from scenario.env
#   SWEEP_DURATION  per-combination load duration (default 20s)
#
# One repetition per combination — this is a search, not a publication. Re-run
# `task bench` with the winner to produce numbers worth publishing.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required}"
TARGET="${TARGET:-native}"

load_host "${HOST:-local}"
load_scenario "$SCENARIO"

WORKERS_LIST="${SWEEP_WORKERS:-${SCENARIO_SWEEP_WORKERS:-8}}"
BUFFER_LIST="${SWEEP_BUFFER:-${SCENARIO_SWEEP_BUFFER:-64}}"
POOL_LIST="${SWEEP_POOL:-${SCENARIO_SWEEP_POOL:-8}}"
SWEEP_DURATION="${SWEEP_DURATION:-20s}"

DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"

TARGET="$TARGET" HOST="${HOST:-local}" "$LAB_BIN/preflight.sh"
python3 "$LAB_BIN/render-config.py" "$SCENARIO_DIR/octo/integration.yaml" tuned \
  --tunables "${TUNABLES:-workers buffer pool}" >/dev/null \
  || die "cannot render the tuned variant — the scenario's root flow declares no tuning knobs"

scenario_setup
trap 'scenario_teardown' EXIT

VERSION="$(version_under_test "$TARGET")"
SWEEP_ID="$(today)-${HOST_PROFILE}-${SCENARIO_ID}-${TARGET}-v${VERSION}"
SWEEP_DIR="$REPO_ROOT/results/sweeps/$SWEEP_ID"
if [ -d "$SWEEP_DIR" ]; then
  n=2; while [ -d "${SWEEP_DIR}-${n}" ]; do n=$((n+1)); done
  SWEEP_DIR="${SWEEP_DIR}-${n}"
fi
mkdir -p "$SWEEP_DIR"

total=0
for w in $WORKERS_LIST; do for b in $BUFFER_LIST; do for p in $POOL_LIST; do
  total=$((total+1))
done; done; done

step "sweep $SWEEP_ID — $total combinations × $SWEEP_DURATION"
dim "  workers: $WORKERS_LIST"
dim "  buffer:  $BUFFER_LIST"
dim "  pool:    $POOL_LIST"

SCENARIO_ID="$SCENARIO_ID" TARGET="$TARGET" "$LAB_BIN/capture-env.sh" "$SWEEP_DIR/env.json"

STAGE="$REPO_ROOT/.stage/sweep"

i=0
for w in $WORKERS_LIST; do
 for b in $BUFFER_LIST; do
  for p in $POOL_LIST; do
    i=$((i+1))
    cell="$SWEEP_DIR/w${w}-b${b}-p${p}"
    mkdir -p "$cell"
    step "[$i/$total] workers=$w buffer=$b pool=$p"
    printf '{"workers":"%s","buffer":"%s","pool":"%s"}\n' "$w" "$b" "$p" > "$cell/knobs.json"

    # Re-render per combination: the knobs are baked into the config, because
    # Octo's ${ENV} substitution does not reach root-flow fields.
    export TUNED_WORKERS="$w" TUNED_BUFFER="$b" TUNED_POOL="$p"
    "$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" tuned "$STAGE" >/dev/null
    cp "$STAGE/octo.yaml" "$cell/config.yaml"

    cell_cleanup() {
      "$LAB_BIN/sampler-stop.sh" "$cell" 2>/dev/null || true
      "$DRIVER" stop "$cell" 2>/dev/null || true
    }
    trap cell_cleanup EXIT

    ID="$("$DRIVER" start "$STAGE" "$cell")"
    if ! wait_ready "$(ready_url)" 60 >/dev/null; then
      warn "combination did not start; skipping"
      cat "$cell/octo.log" >&2 2>/dev/null || true
      "$DRIVER" stop "$cell" 2>/dev/null || true
      trap - EXIT
      continue
    fi

    k6 run --no-usage-report --summary-trend-stats 'avg,min,med,p(90),p(95),p(99),max' \
      -e "BASE_URL=$BASE_URL" -e "ROUTE=$ROUTE" \
      -e "SUMMARY_OUT=$STAGE/warmup.json" \
      -e "DURATION=${SWEEP_WARMUP:-5s}" -e "RATE=${STEADY_RATE:-500}" \
      "$SCENARIO_DIR/k6/steady.js" >/dev/null 2>&1 || true

    "$LAB_BIN/sampler-start.sh" "$TARGET" "$ID" "$cell/resources.csv" "$cell" "${SAMPLE_INTERVAL:-1.0}"

    k6_exit=0
    k6 run --no-usage-report --summary-trend-stats 'avg,min,med,p(90),p(95),p(99),max' \
      -e "BASE_URL=$BASE_URL" -e "ROUTE=$ROUTE" \
      -e "SUMMARY_OUT=$cell/summary.json" \
      -e "DURATION=$SWEEP_DURATION" -e "RATE=${STEADY_RATE:-500}" \
      "$SCENARIO_DIR/k6/steady.js" >"$cell/k6.log" 2>&1 || k6_exit=$?
    echo "$k6_exit" > "$cell/k6-exit.txt"

    "$LAB_BIN/sampler-stop.sh" "$cell"
    "$DRIVER" stop "$cell"
    trap 'scenario_teardown' EXIT

    # Mandatory between combinations: without it a sweep measures the order in
    # which combinations happened to run as much as the combinations themselves.
    # See METHODOLOGY.md.
    sleep "${COOLDOWN_SECONDS:-15}"
  done
 done
done

step "ranking"
python3 "$LAB_BIN/sweep-report.py" "$SWEEP_DIR"

info ""
info "  $SWEEP_DIR/SWEEP.md"
