#!/usr/bin/env bash
# Run every scenario against every target, one after another.
#
# Deliberately sequential. Two benchmarks sharing a host measure each other, so
# parallelising this would be faster and worthless.
#
# Each scenario names its own tuned configuration below rather than taking a single
# global one, because the whole finding of this lab is that the right knob values
# are a property of the flow's shape: a CPU-bound flow wants the defaults, a flow
# that blocks for 70 ms wants two orders of magnitude more workers.
#
# Usage: run-matrix.sh [scenario ...]
#   TARGETS="native docker"   which targets to sweep (default both)
#   REPS=3                    repetitions per arm
#
# Progress is appended to results/matrix-<date>.log; a failure in one combination
# does not stop the others, and the summary at the end says which ones failed.

set -uo pipefail

LAB_BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$LAB_BIN/../.." && pwd)"
cd "$REPO_ROOT"

TARGETS="${TARGETS:-native docker}"
REPS="${REPS:-3}"
SCENARIOS=("$@")
if [ ${#SCENARIOS[@]} -eq 0 ]; then
  SCENARIOS=(001-template-page 002-fanout-transform 003-postgres-crud \
             004-queue-roundtrip 005-http-proxy 006-json-transform)
fi

LOG="$REPO_ROOT/results/matrix-$(date +%Y-%m-%d).log"
mkdir -p "$REPO_ROOT/results"

# Tuned arm per scenario. Empty means "the scenario's own defaults are already the
# tuned values worth publishing".
tuned_env() {
  case "$1" in
    001-template-page)    echo "TUNED_WORKERS=128 TUNED_BUFFER=256 TUNED_POOL=8" ;;
    002-fanout-transform) echo "TUNED_WORKERS=32 TUNED_BUFFER=256 TUNED_POOL=32" ;;
    003-postgres-crud)    echo "TUNED_WORKERS=64 TUNED_BUFFER=256 TUNED_POOL=8 TUNED_MAXOPENCONNS=64 TUNED_MAXIDLECONNS=64" ;;
    004-queue-roundtrip)  echo "TUNED_WORKERS=64 TUNED_BUFFER=256 TUNED_POOL=8 TUNED_LISTENERS=64" ;;
    # The one scenario where the tuned arm is not a refinement but the difference
    # between working and not: 8 workers against a 70 ms backend caps at 108 req/s.
    005-http-proxy)       echo "TUNED_WORKERS=512 TUNED_BUFFER=1024 TUNED_POOL=8" ;;
    006-json-transform)   echo "TUNED_WORKERS=32 TUNED_BUFFER=256 TUNED_POOL=8" ;;
    *)                    echo "" ;;
  esac
}

say() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$LOG"; }

# Leftovers from an interrupted run would collide on the port and be measured as
# though they were the thing under test.
reset_host() {
  pkill -9 -f 'octo.*run --config' 2>/dev/null
  docker rm -f octo-bench >/dev/null 2>&1
  local pids
  pids="$(lsof -nP -iTCP:8080 -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{print $2}')"
  [ -n "$pids" ] && echo "$pids" | xargs -r kill -9 2>/dev/null
  sleep 2
}

total=0; failed=0
declare -a FAILURES=()

say "=== matrix start: ${#SCENARIOS[@]} scenarios x $(echo "$TARGETS" | wc -w | tr -d ' ') targets, REPS=$REPS ==="
say "    octo native: ${OCTO_BIN:-$(command -v octo)}"
say "    octo image:  ${OCTO_IMAGE:-juancavallotti/octo-runtime:latest}"

for scenario in "${SCENARIOS[@]}"; do
  for target in $TARGETS; do
    total=$((total + 1))
    say ">>> $scenario / $target"
    reset_host
    if env $(tuned_env "$scenario") \
         SCENARIO="$scenario" TARGET="$target" REPS="$REPS" \
         "$LAB_BIN/run-bench.sh" >>"$LOG" 2>&1; then
      say "    ok"
    else
      failed=$((failed + 1))
      FAILURES+=("$scenario/$target")
      say "    FAILED (see $LOG)"
    fi
  done
done

reset_host
python3 "$LAB_BIN/index.py" "$REPO_ROOT/results" >>"$LOG" 2>&1

say "=== matrix done: $((total - failed))/$total succeeded ==="
if [ ${#FAILURES[@]} -gt 0 ]; then
  say "    failed: ${FAILURES[*]}"
fi
