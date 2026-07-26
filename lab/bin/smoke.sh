#!/usr/bin/env bash
# Correctness gate on its own: start the runtime, assert the endpoint serves what
# the scenario says it should, stop.
#
# Usage: smoke.sh
# Environment: SCENARIO (required), TARGET (default native), HOST (default local),
#              VARIANT (default baseline)

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required}"
TARGET="${TARGET:-native}"
VARIANT="${VARIANT:-baseline}"

load_host "${HOST:-local}"
load_scenario "$SCENARIO"

DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"

# The dev artifact is built here, in the entry point, and exported so every child
# in the run resolves the same one. Left to the children, footprint.sh would kick
# off its own compile from inside the run — see ensure_octo_build in common.sh.
ensure_octo_build "$TARGET"
TARGET="$TARGET" HOST="${HOST:-local}" SCENARIO="$SCENARIO" "$LAB_BIN/preflight.sh"

step "guard: the $VARIANT variant renders from integration.yaml"
python3 "$LAB_BIN/render-config.py" "$SCENARIO_DIR/octo/integration.yaml" "$VARIANT" \
  --tunables "${TUNABLES:-workers buffer pool}" >/dev/null \
  || die "cannot render the '$VARIANT' variant"
dim "  ok"

scenario_setup
trap 'scenario_teardown' EXIT

STAGE="$REPO_ROOT/.stage/smoke"
STATE="$REPO_ROOT/.stage/smoke-state"
rm -rf "$STATE"; mkdir -p "$STATE"

"$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" "$VARIANT" "$STAGE" >/dev/null

cleanup() { "$DRIVER" stop "$STATE" 2>/dev/null || true; scenario_teardown; }
trap cleanup EXIT

step "starting $TARGET ($VARIANT)"
"$DRIVER" start "$STAGE" "$STATE" >/dev/null
if ! ready_ms="$(readiness_probe 60)"; then
  cat "$STATE/octo.log" >&2 2>/dev/null || true
  die "target never became ready"
fi
dim "  ready in ${ready_ms}ms (via $(readiness_method "$TARGET"))"

step "smoke: ${BASE_URL}${ROUTE}"
exit_code=0
k6 run \
  --no-usage-report \
  --summary-trend-stats 'avg,min,med,p(90),p(95),p(99),max' \
  -e "BASE_URL=$BASE_URL" \
  -e "ROUTE=$ROUTE" \
  -e "SUMMARY_OUT=$STATE/summary.json" \
  "$SCENARIO_DIR/k6/smoke.js" 2>&1 | tee "$STATE/k6.log" || exit_code=$?

"$DRIVER" stop "$STATE"
trap - EXIT
scenario_teardown

if [ "$exit_code" -ne 0 ]; then
  die "smoke failed — see $STATE/k6.log"
fi
step "smoke passed"
