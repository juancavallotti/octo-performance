#!/usr/bin/env bash
# Where does the time go inside a flow?
#
# Usage: run-profile.sh
# Environment: SCENARIO (required), TARGET, HOST, BUILD, VARIANT (default tuned),
#              BLOCKS (default '*'), PROFILE_RATE, PROFILE_DURATION
#
# This is a diagnostic, not a benchmark, and the distinction is the whole design.
#
# --metrics-blocks makes the engine emit a pre- and post-invoke event around EVERY
# block in every flow — the watched set filters what is recorded, not what is
# emitted. So a profiled run is measurably not the same workload as an unprofiled
# one, and its throughput and latency figures describe a runtime carrying
# instrumentation nobody deploys. What survives that is the *shape*: which block
# holds the time, relative to its siblings.
#
# Output therefore goes to diagnostics/ and never to results/. Nothing here is
# publishable, and AGENTS.md rule 4 keeps results/ for numbers that are.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required, e.g. SCENARIO=002-fanout-transform}"
TARGET="${TARGET:-native}"
VARIANT="${VARIANT:-tuned}"
# Every block, by default. On a small config that is the fastest way to find where
# the time goes; the per-block section of the report ranks by total time.
BLOCKS="${BLOCKS:-*}"

load_host "${HOST:-local}"
load_scenario "$SCENARIO"

DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"

ensure_octo_build "$TARGET"
octo_has_admin_port "$TARGET" || true

octo_has_admin_port "$TARGET" || die \
  "this build serves no admin port, so it has no per-block telemetry — measure a source build: BUILD=dev"

# METRICS_BLOCKS is what turns the machinery on; it is read by the target drivers.
export METRICS_BLOCKS="$BLOCKS"
export METRICS=1
metrics_blocks_wanted "$TARGET" || die "per-block metrics could not be enabled"

TARGET="$TARGET" HOST="${HOST:-local}" SCENARIO="$SCENARIO" "$LAB_BIN/preflight.sh"

METRICS_SCRAPE_URL="$(metrics_url "$TARGET")"
export METRICS_SCRAPE_URL

scenario_setup
trap 'scenario_teardown' EXIT

VERSION="$(version_under_test "$TARGET")"
OUT_DIR="$REPO_ROOT/diagnostics/$(today)-${HOST_PROFILE}-${SCENARIO_ID}-${TARGET}-v${VERSION}-profile"
[ -d "$OUT_DIR" ] && rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

STAGE="$REPO_ROOT/.stage/profile"
STATE="$OUT_DIR/cell"
mkdir -p "$STATE"

step "profile $SCENARIO_ID ($VARIANT) — blocks: $BLOCKS"
dim "  a diagnostic: every block emits events, so throughput here is not a result"

"$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" "$VARIANT" "$STAGE" >/dev/null
cp "$STAGE/octo.yaml" "$OUT_DIR/config.yaml"

cleanup() {
  "$LAB_BIN/sampler-stop.sh" "$STATE" 2>/dev/null || true
  "$DRIVER" stop "$STATE" 2>/dev/null || true
  scenario_teardown
}
trap cleanup EXIT

ID="$("$DRIVER" start "$STAGE" "$STATE")"
if ! readiness_probe 60 >/dev/null; then
  cat "$STATE/octo.log" >&2 2>/dev/null || true
  die "target never became ready"
fi

k6_common=(
  --no-usage-report
  --summary-trend-stats 'avg,min,med,p(90),p(95),p(99),max'
  -e "BASE_URL=$BASE_URL"
  -e "ROUTE=$ROUTE"
)

# Warm-up, discarded, exactly as a measured run does — lazy initialisation inside a
# block would otherwise land in that block's first-invocation timing and rank it
# above where it belongs.
dim "  warm-up ${WARMUP_DURATION:-10s}"
k6 run "${k6_common[@]}" -e "SUMMARY_OUT=$STAGE/warmup.json" \
  -e "DURATION=${WARMUP_DURATION:-10s}" -e "RATE=${PROFILE_RATE:-${STEADY_RATE:-500}}" \
  "$SCENARIO_DIR/k6/steady.js" >"$STATE/warmup.log" 2>&1 || \
  warn "warm-up reported a non-zero exit; continuing"

"$LAB_BIN/sampler-start.sh" "$TARGET" "$ID" "$STATE/resources.csv" "$STATE" "${SAMPLE_INTERVAL:-1.0}"

dim "  profiling ${PROFILE_DURATION:-${STEADY_DURATION:-30s}} at ${PROFILE_RATE:-${STEADY_RATE:-500}} req/s"
k6_exit=0
k6 run "${k6_common[@]}" -e "SUMMARY_OUT=$STATE/summary.json" \
  -e "DURATION=${PROFILE_DURATION:-${STEADY_DURATION:-30s}}" \
  -e "RATE=${PROFILE_RATE:-${STEADY_RATE:-500}}" \
  "$SCENARIO_DIR/k6/steady.js" >"$STATE/k6.log" 2>&1 || k6_exit=$?
echo "$k6_exit" > "$STATE/k6-exit.txt"

"$LAB_BIN/sampler-stop.sh" "$STATE"
"$DRIVER" stop "$STATE"
trap 'scenario_teardown' EXIT
rm -f "$STATE/warmup.log"

step "profile report"
SCENARIO_ID="$SCENARIO_ID" TARGET="$TARGET" VERSION="$VERSION" VARIANT="$VARIANT" \
  BLOCKS="$BLOCKS" python3 "$LAB_BIN/profile-report.py" "$OUT_DIR"

step "done"
info ""
info "  $OUT_DIR/PROFILE.md"
