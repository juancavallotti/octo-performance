#!/usr/bin/env bash
# Benchmark the released distribution and a source build of the same scenario,
# back to back, and write the delta.
#
# Usage: run-compare.sh
#
# Environment:
#   SCENARIO   scenario id (required)        TARGET   native|docker (default native)
#   HOST       host profile (default local)  REPS     repetitions   (default 3)
#   ORDER      "release dev" (default) — the order the two arms are measured in
#   COMPARE_COOLDOWN  seconds between the arms (default 30)
#
# Everything else is inherited, so the two arms differ in the build under test
# and nothing else.
#
# The arms run in one invocation rather than as two separate commands because the
# comparison is only worth as much as the conditions it was measured under: the
# same laptop, the same thermal state, the same cooldown between them. Two runs a
# day apart on a machine that throttles are not a before-and-after.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required, e.g. SCENARIO=005-http-proxy}"
TARGET="${TARGET:-native}"
REPS="${REPS:-3}"
ORDER="${ORDER:-release dev}"

[ -d "$REPO_ROOT/scenarios/$SCENARIO" ] || die "no scenario at scenarios/$SCENARIO"
for arm in $ORDER; do
  case "$arm" in release|dev) ;; *) die "ORDER may only contain 'release' and 'dev', got '$arm'" ;; esac
done

step "compare: $SCENARIO on $TARGET — arms: $ORDER, REPS=$REPS"

# Build the dev artifact before either arm runs. A compile error should cost
# seconds at the start rather than surface after the release arm has already
# spent several minutes measuring.
DEV_REF="$("$LAB_BIN/build-octo.sh" "$TARGET")" || die "dev build failed; nothing to compare"
DEV_ID="$("$LAB_BIN/build-octo.sh" "$TARGET" --meta \
          | python3 -c 'import json,sys; print(json.load(sys.stdin)["buildId"])')"
dim "  dev artifact: $DEV_REF ($DEV_ID)"

TMP="$(mktemp -d -t octo-compare)"
trap 'rm -rf "$TMP"' EXIT

declare -a RUN_DIRS=()
first=1
for arm in $ORDER; do
  # Between arms, not only between repetitions: the second arm otherwise inherits
  # a hot chassis and a socket table full of TIME_WAIT from the first, and that
  # ordering artifact is easily large enough to look like the change under test.
  if [ "$first" -eq 0 ]; then
    dim "  inter-arm cooldown ${COMPARE_COOLDOWN:-30}s"
    sleep "${COMPARE_COOLDOWN:-30}"
  fi
  first=0

  step "arm: $arm"
  BUILD="$arm" RUN_DIR_OUT="$TMP/$arm" \
    SCENARIO="$SCENARIO" TARGET="$TARGET" HOST="${HOST:-local}" REPS="$REPS" \
    "$LAB_BIN/run-bench.sh" || die "the $arm arm failed; nothing to compare"

  [ -s "$TMP/$arm" ] || die "the $arm arm did not report a run directory"
  RUN_DIRS+=("$(cat "$TMP/$arm")")
done

# ------------------------------------------------------------------- report ---
step "comparison report"
python3 "$LAB_BIN/compare-report.py" "${RUN_DIRS[@]}"
python3 "$LAB_BIN/index.py" "$REPO_ROOT/results"
