#!/usr/bin/env bash
# Closed-model VU sweep: throughput and CPU against virtual users, one k6 execution
# per VU level.
#
# This exists for one reason: to produce a curve shaped like the ones most published
# benchmarks use, so that our numbers can be read next to them. Those charts plot
# average throughput and CPU against a fixed virtual-user population and call the
# peak the "knee point". That knee only exists under a closed model — offer load at
# a fixed rate instead and the same server has no knee, it just queues — so matching
# the x-axis is the only honest way to land on the same chart.
#
# Everything else in this lab stays open-model. See METHODOLOGY.md and COMPARISON.md.
#
# Usage: run-vuramp.sh
#
# Environment:
#   SCENARIO   scenario id (required)     TARGET     native|docker  (default native)
#   HOST       host profile (default local)
#   VARIANT    which arm to sweep         (default baseline)
#   VU_LEVELS  space-separated VU counts  (default from scenario.env, else a ladder)
#   VU_DURATION  dwell at each level      (default 30s)
#   CPU_LIMIT  cap the container's CPU to a known size (docker target only)

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required, e.g. SCENARIO=005-http-proxy}"
TARGET="${TARGET:-native}"
VARIANT="${VARIANT:-baseline}"

load_host "${HOST:-local}"
load_scenario "$SCENARIO"

VU_LEVELS="${VU_LEVELS:-${SCENARIO_VU_LEVELS:-1 5 10 25 50 100 200 400}}"
VU_DURATION="${VU_DURATION:-30s}"

DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"
[ -f "$SCENARIO_DIR/k6/vus.js" ] || die "scenario $SCENARIO_ID has no k6/vus.js — the closed-model test is opt-in per scenario"

# The dev artifact is built here, in the entry point, and exported so every child
# in the run resolves the same one. Left to the children, footprint.sh would kick
# off its own compile from inside the run — see ensure_octo_build in common.sh.
ensure_octo_build "$TARGET"
octo_has_admin_port "$TARGET" || true
TARGET="$TARGET" HOST="${HOST:-local}" SCENARIO="$SCENARIO" "$LAB_BIN/preflight.sh"

# The closed-model sweep is where in-flight matters most: it is the signal that
# says whether a VU level is queueing behind busy workers or genuinely idle, which
# is the whole question a knee point is trying to answer.
METRICS_SCRAPE_URL="$(metrics_url "$TARGET")"
export METRICS_SCRAPE_URL

scenario_setup
trap 'scenario_teardown' EXIT

# ------------------------------------------------------------------ run id ----
VERSION="$(version_under_test "$TARGET")"
RUN_ID="$(today)-${HOST_PROFILE}-${SCENARIO_ID}-${TARGET}-v${VERSION}-vuramp"
[ -n "${CPU_LIMIT:-}" ] && RUN_ID="${RUN_ID}-${CPU_LIMIT}cpu"
[ -n "${RUN_LABEL:-}" ] && RUN_ID="${RUN_ID}-${RUN_LABEL}"

RUN_DIR="$REPO_ROOT/results/$RUN_ID"
if [ -d "$RUN_DIR" ]; then
  n=2
  while [ -d "${RUN_DIR}-${n}" ]; do n=$((n+1)); done
  RUN_DIR="${RUN_DIR}-${n}"; RUN_ID="$(basename "$RUN_DIR")"
  warn "run id already existed; using $RUN_ID"
fi
mkdir -p "$RUN_DIR"

step "vu ramp $RUN_ID"
dim "  variant=$VARIANT levels='$VU_LEVELS' dwell=$VU_DURATION"

SCENARIO_ID="$SCENARIO_ID" TARGET="$TARGET" "$LAB_BIN/capture-env.sh" "$RUN_DIR/env.json"

STAGE="$REPO_ROOT/.stage/vuramp"
"$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" "$VARIANT" "$STAGE" >/dev/null
cp "$STAGE/octo.yaml" "$RUN_DIR/config.yaml"

k6_common=(
  --no-usage-report
  --summary-trend-stats 'avg,min,med,p(90),p(95),p(99),max'
  -e "BASE_URL=$BASE_URL"
  -e "ROUTE=$ROUTE"
)

# The server is restarted between levels on purpose. A single long-lived process
# carries state across levels — connection pools, heap, TIME_WAIT — and the later,
# higher levels would inherit whatever the earlier ones left behind, which is
# exactly the ordering artifact this lab has been bitten by before.
for vus in $VU_LEVELS; do
  cell="$RUN_DIR/vus-${vus}"
  mkdir -p "$cell"
  step "vus=$vus"

  cell_cleanup() {
    "$LAB_BIN/sampler-stop.sh" "$cell" 2>/dev/null || true
    "$DRIVER" stop "$cell" 2>/dev/null || true
  }
  trap cell_cleanup EXIT

  ID="$("$DRIVER" start "$STAGE" "$cell")"
  if ! readiness_probe 60 >/dev/null; then
    cat "$cell/octo.log" >&2 2>/dev/null || true
    die "target never became ready"
  fi

  # Short warm-up at the same concurrency, discarded.
  k6 run "${k6_common[@]}" -e "SUMMARY_OUT=$STAGE/warmup.json" \
    -e "VUS=$vus" -e "DURATION=${WARMUP_DURATION:-10s}" \
    "$SCENARIO_DIR/k6/vus.js" >"$cell/warmup.log" 2>&1 || \
    warn "warm-up reported a non-zero exit; continuing"

  "$LAB_BIN/sampler-start.sh" "$TARGET" "$ID" "$cell/resources.csv" "$cell" "${SAMPLE_INTERVAL:-1.0}"

  k6_exit=0
  k6 run "${k6_common[@]}" -e "SUMMARY_OUT=$cell/summary.json" \
    -e "VUS=$vus" -e "DURATION=$VU_DURATION" \
    "$SCENARIO_DIR/k6/vus.js" >"$cell/k6.log" 2>&1 || k6_exit=$?
  echo "$k6_exit" > "$cell/k6-exit.txt"

  "$LAB_BIN/sampler-stop.sh" "$cell"
  "$DRIVER" stop "$cell"
  trap 'scenario_teardown' EXIT
  rm -f "$cell/warmup.log"

  tps="$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(f"{d[\"metrics\"][\"http_reqs\"][\"rate\"]:,.0f}")' "$cell/summary.json" 2>/dev/null || echo '?')"
  dim "  $tps req/s"

  dim "  cooldown ${COOLDOWN_SECONDS:-15}s"
  sleep "${COOLDOWN_SECONDS:-15}"
done

step "report"
python3 "$LAB_BIN/vuramp-report.py" "$RUN_DIR"

step "done"
info ""
info "  $RUN_DIR/REPORT.md"
