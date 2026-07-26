#!/usr/bin/env bash
# Orchestrate a full benchmark run: baseline and tuned, N repetitions each,
# with resource sampling, and a generated report.
#
# Usage: run-bench.sh
#
# Environment:
#   SCENARIO   scenario id (required)          TARGET     native|docker      (default native)
#   HOST       host profile   (default local)  REPS       repetitions        (default 3)
#   TEST       steady|capacity (default steady)
#   VARIANTS   space separated (default "baseline tuned")
#   RUN_LABEL  optional suffix appended to the run id

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required, e.g. SCENARIO=001-template-page}"
TARGET="${TARGET:-native}"
REPS="${REPS:-3}"
TEST="${TEST:-steady}"
VARIANTS="${VARIANTS:-baseline tuned}"

load_host "${HOST:-local}"
load_scenario "$SCENARIO"

DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"

# ---------------------------------------------------------------- preflight ---
# The dev artifact is built here, in the entry point, and exported so every child
# in the run resolves the same one. Left to the children, footprint.sh would kick
# off its own compile from inside the run — see ensure_octo_build in common.sh.
ensure_octo_build "$TARGET"
# Asked once, here, and exported: for TARGET=docker the answer costs a container
# start, and every child in the run needs it. Same reasoning as the dev build above.
octo_has_admin_port "$TARGET" || true
TARGET="$TARGET" HOST="${HOST:-local}" SCENARIO="$SCENARIO" "$LAB_BIN/preflight.sh"

step "guard: both variants render from the scenario's single integration.yaml"
for v in $VARIANTS; do
  python3 "$LAB_BIN/render-config.py" "$SCENARIO_DIR/octo/integration.yaml" "$v" \
    --tunables "${TUNABLES:-workers buffer pool}" >/dev/null \
    || die "cannot render the '$v' variant"
done
dim "  ok"

# --------------------------------------------------- scenario dependencies ----
# Outside the measured window: a cold database inside a benchmark would put
# container start-up and schema creation into the numbers.
scenario_setup
trap 'scenario_teardown' EXIT

# ------------------------------------------------------------------ run id ----
VERSION="$(version_under_test "$TARGET")"
RUN_ID="$(today)-${HOST_PROFILE}-${SCENARIO_ID}-${TARGET}-v${VERSION}"
# A capped run is a different deployment, not a different day: without this in the
# id, a 1-CPU run and an unconstrained one collide and the second is filed as a
# repeat of the first.
[ -n "${CPU_LIMIT:-}" ] && RUN_ID="${RUN_ID}-${CPU_LIMIT}cpu"
# Per-block telemetry makes the engine emit an event around every block in every
# flow, so a run carrying it is not measuring the same thing as one that is not. In
# the id, or the regression view files it as a repeat of a run it cannot be compared
# with. Per-flow metrics are not stamped: their cost was measured and stated in
# METHODOLOGY.md, and they are on for every run.
metrics_blocks_wanted "$TARGET" && RUN_ID="${RUN_ID}-blockmetrics"
[ -n "${RUN_LABEL:-}" ] && RUN_ID="${RUN_ID}-${RUN_LABEL}"

RUN_DIR="$REPO_ROOT/results/$RUN_ID"
if [ -d "$RUN_DIR" ]; then
  n=2
  while [ -d "${RUN_DIR}-${n}" ]; do n=$((n+1)); done
  RUN_DIR="${RUN_DIR}-${n}"; RUN_ID="$(basename "$RUN_DIR")"
  warn "run id already existed; using $RUN_ID"
fi
mkdir -p "$RUN_DIR"

step "run $RUN_ID"
dim "  scenario=$SCENARIO_ID target=$TARGET version=$VERSION reps=$REPS test=$TEST"
dim "  readiness=$(readiness_method "$TARGET") metrics=$(metrics_wanted "$TARGET" && echo on || echo off)${METRICS_BLOCKS:+ blocks=$METRICS_BLOCKS}"

# The scrape URL for this run, or empty. Exported once so the sampler does not have
# to re-derive a decision that depends on the build under test.
METRICS_SCRAPE_URL="$(metrics_url "$TARGET")"
export METRICS_SCRAPE_URL

# ------------------------------------------------------------------ context ---
SCENARIO_ID="$SCENARIO_ID" TARGET="$TARGET" "$LAB_BIN/capture-env.sh" "$RUN_DIR/env.json"

STAGE="$REPO_ROOT/.stage/bench"

# k6 flags shared by every invocation.
k6_common=(
  --no-usage-report
  --summary-trend-stats 'avg,min,med,p(90),p(95),p(99),max'
  -e "BASE_URL=$BASE_URL"
  -e "ROUTE=$ROUTE"
)

# run_k6 <script> <summary-out> <log-out> [extra -e args...]
run_k6() {
  local script="$1" summary="$2" log="$3"; shift 3
  k6 run "${k6_common[@]}" \
    -e "SUMMARY_OUT=$summary" \
    "$@" \
    "$script" >"$log" 2>&1
}

# ------------------------------------------------------- smoke gate (rule 6) --
step "smoke gate"
SMOKE_STATE="$RUN_DIR/smoke"
mkdir -p "$SMOKE_STATE"
"$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" baseline "$STAGE" >/dev/null

smoke_cleanup() { "$DRIVER" stop "$SMOKE_STATE" 2>/dev/null || true; }
trap smoke_cleanup EXIT
"$DRIVER" start "$STAGE" "$SMOKE_STATE" >/dev/null
readiness_probe 60 >/dev/null || {
  cat "$SMOKE_STATE/octo.log" >&2 2>/dev/null || true
  die "target never became ready"
}

# Ask the running process what it is, while one happens to be up.
#
# Rule 1 is "no version, no result", and until now it was satisfied by asking the
# artifact before starting it — which answers "what did we intend to run". The
# process's own octo_build_info answers "what is running", including which services
# provider was compiled in. Those differ exactly when something is wrong, which is
# when a provenance record earns its keep.
if [ -n "$METRICS_SCRAPE_URL" ]; then
  curl -s --max-time 5 "$METRICS_SCRAPE_URL" 2>/dev/null \
    | python3 "$LAB_BIN/metrics-identity.py" > "$RUN_DIR/runtime-identity.json" \
    || rm -f "$RUN_DIR/runtime-identity.json"
fi

if run_k6 "$SCENARIO_DIR/k6/smoke.js" "$SMOKE_STATE/summary.json" "$SMOKE_STATE/k6.log"; then
  dim "  smoke passed"
else
  tail -30 "$SMOKE_STATE/k6.log" >&2
  "$DRIVER" stop "$SMOKE_STATE"
  die "smoke failed — a load run on a broken endpoint is not a result (AGENTS.md rule 6)"
fi
"$DRIVER" stop "$SMOKE_STATE"
trap 'scenario_teardown' EXIT

# ---------------------------------------------------------------- footprint ---
TARGET="$TARGET" ROUTE="$ROUTE" "$LAB_BIN/footprint.sh" "$SCENARIO_DIR" "$RUN_DIR/footprint.json"

# ------------------------------------------------------------ measured runs ---
for variant in $VARIANTS; do
  for rep in $(seq 1 "$REPS"); do
    cell="$RUN_DIR/${variant}-${TEST}-rep${rep}"
    mkdir -p "$cell"
    step "$variant / $TEST / rep $rep"

    # The knobs are baked into the rendered config rather than passed as
    # environment: the rendered file is archived as this cell's config.yaml, so a
    # literal value is provenance that survives the run. stage-config.sh reads
    # TUNED_<KNOB> for each of the scenario's declared TUNABLES; baseline ignores
    # them and strips instead.
    "$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" "$variant" "$STAGE" >/dev/null
    # Provenance: keep the exact config this cell ran, so a result can always be
    # traced back to the YAML that produced it.
    cp "$STAGE/octo.yaml" "$cell/config.yaml"

    # Record the knobs as they ended up in the config, not as they were requested.
    TUNABLES="${TUNABLES:-workers buffer pool}" python3 - "$STAGE/octo.yaml" > "$cell/knobs.json" <<'PY'
import json, os, re, sys
names = os.environ.get("TUNABLES", "workers buffer pool").split()
knob = re.compile(r"^\s*(%s):\s*(\d+)\s*$" % "|".join(re.escape(n) for n in names))
found = {}
for line in open(sys.argv[1]):
    m = knob.match(line)
    if m:
        found[m.group(1)] = m.group(2)
print(json.dumps({k: found.get(k, "runtime default") for k in names}))
PY
    dim "  knobs: $(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(" ".join(f"{k}={v}" for k,v in d.items()))' "$cell/knobs.json")"

    cell_cleanup() {
      "$LAB_BIN/sampler-stop.sh" "$cell" 2>/dev/null || true
      "$DRIVER" stop "$cell" 2>/dev/null || true
    }
    trap cell_cleanup EXIT

    start_ms="$(epoch_ms)"
    ID="$("$DRIVER" start "$STAGE" "$cell")"
    if ! readiness_probe 60 >/dev/null; then
      cat "$cell/octo.log" >&2 2>/dev/null || true
      die "target never became ready"
    fi
    ready_ms="$(epoch_ms)"
    echo $(( ready_ms - start_ms )) > "$cell/cold-start-ms.txt"

    # Warm-up, discarded: pays for Go runtime warm-up, connection setup, and any
    # lazy initialisation so the measured window is steady state.
    dim "  warm-up ${WARMUP_DURATION:-10s}"
    run_k6 "$SCENARIO_DIR/k6/${TEST}.js" "$STAGE/warmup-summary.json" "$cell/warmup.log" \
      -e "DURATION=${WARMUP_DURATION:-10s}" -e "RATE=${WARMUP_RATE:-$STEADY_RATE}" || \
      warn "warm-up reported a non-zero exit; continuing"

    "$LAB_BIN/sampler-start.sh" "$TARGET" "$ID" "$cell/resources.csv" "$cell" "${SAMPLE_INTERVAL:-1.0}"

    dim "  measuring"
    k6_exit=0
    run_k6 "$SCENARIO_DIR/k6/${TEST}.js" "$cell/summary.json" "$cell/k6.log" \
      -e "RATE=${STEADY_RATE:-500}" -e "DURATION=${STEADY_DURATION:-60s}" || k6_exit=$?
    echo "$k6_exit" > "$cell/k6-exit.txt"
    # A non-zero exit means a threshold was breached. That is data, not a crash:
    # the report records it and the run continues.
    [ "$k6_exit" -ne 0 ] && warn "k6 exited $k6_exit (threshold breach) — recorded"

    "$LAB_BIN/sampler-stop.sh" "$cell"
    "$DRIVER" stop "$cell"
    trap 'scenario_teardown' EXIT

    rm -f "$cell/warmup.log"

    # Cool down between measured runs. Not politeness: back-to-back load runs leave
    # ~a million sockets in TIME_WAIT and a hot chassis, and the next run inherits
    # both. The resulting ordering artifacts are big enough to look like a real
    # difference between identical configurations. See METHODOLOGY.md.
    dim "  cooldown ${COOLDOWN_SECONDS:-15}s"
    sleep "${COOLDOWN_SECONDS:-15}"
  done
done

# ------------------------------------------------------------------- report ---
step "report"
python3 "$LAB_BIN/report.py" "$RUN_DIR"
python3 "$LAB_BIN/index.py" "$REPO_ROOT/results"

step "done"
info ""
info "  $RUN_DIR/REPORT.md"

# A caller orchestrating several runs (run-compare.sh) needs the run directory,
# and parsing it out of the log would break the first time a message changes.
#
# The explicit `exit 0` is not decoration: as the last statement of the script, a
# false `[ -n ... ]` becomes the script's exit status, so every successful run that
# did not set RUN_DIR_OUT — which is every direct `task bench` — reported failure.
if [ -n "${RUN_DIR_OUT:-}" ]; then
  printf '%s\n' "$RUN_DIR" > "$RUN_DIR_OUT"
fi
exit 0
