#!/usr/bin/env bash
# Measure the runtime's standing cost — what it consumes before serving anything.
#
# Usage: footprint.sh <scenario-dir> <out.json>
#
# Reads TARGET, HOST, ROUTE, IDLE_HOLD from the environment. Uses the baseline
# variant, since footprint is a property of the target and not of the tuning.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

SCEN_DIR="${1:?usage: footprint.sh <scenario-dir> <out.json>}"
OUT="${2:?out.json required}"

TARGET="${TARGET:-native}"
IDLE_HOLD="${IDLE_HOLD:-30}"
load_host "${HOST:-local}"
: "${ROUTE:?ROUTE must be set (source the scenario env first)}"

# footprint.sh is the one entry point that does not go through preflight, so it
# produces the dev artifact itself. A no-op for BUILD=release.
ensure_octo_build "$TARGET"

STAGE="$REPO_ROOT/.stage/footprint"
STATE="$REPO_ROOT/.stage/footprint-state"
DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"

step "footprint: $TARGET (idle hold ${IDLE_HOLD}s)"

"$LAB_BIN/stage-config.sh" "$SCEN_DIR" baseline "$STAGE" >/dev/null
rm -rf "$STATE"; mkdir -p "$STATE"

cleanup() {
  "$LAB_BIN/sampler-stop.sh" "$STATE" 2>/dev/null || true
  "$DRIVER" stop "$STATE" 2>/dev/null || true
}
trap cleanup EXIT

# --- artifact size ------------------------------------------------------------
ARTIFACT=""; ARTIFACT_BYTES=0
case "$TARGET" in
  native)
    # Must honour OCTO_BIN, or the footprint describes a different build than the
    # one actually under test.
    ARTIFACT="$(octo_bin)"
    [ -n "$ARTIFACT" ] && [ -f "$ARTIFACT" ] && ARTIFACT_BYTES="$(wc -c < "$ARTIFACT" | tr -d ' ')"
    ;;
  docker)
    ARTIFACT="$(octo_image)"
    ARTIFACT_BYTES="$(docker image inspect "$ARTIFACT" --format '{{.Size}}' 2>/dev/null || echo 0)"
    ;;
esac

# --- cold start ---------------------------------------------------------------
START_EPOCH_MS="$(epoch_ms)"
ID="$("$DRIVER" start "$STAGE" "$STATE")"
if ! COLD_START_MS="$(wait_ready "$(ready_url)" 60)"; then
  warn "target never became ready; see $STATE/octo.log"
  cat "$STATE/octo.log" >&2 2>/dev/null || true
  die "footprint aborted"
fi
# wait_ready starts its clock after the driver returns; include process launch.
READY_EPOCH_MS="$(epoch_ms)"
COLD_START_MS=$(( READY_EPOCH_MS - START_EPOCH_MS ))
dim "  cold start: ${COLD_START_MS}ms"

# --- idle hold ----------------------------------------------------------------
"$LAB_BIN/sampler-start.sh" "$TARGET" "$ID" "$STATE/idle.csv" "$STATE" 1.0
sleep "$IDLE_HOLD"
"$LAB_BIN/sampler-stop.sh" "$STATE"

"$DRIVER" stop "$STATE"
trap - EXIT

# --- emit ---------------------------------------------------------------------
export OUT ARTIFACT ARTIFACT_BYTES COLD_START_MS TARGET IDLE_HOLD
IDLE_CSV="$STATE/idle.csv" python3 - <<'PY'
import csv, json, os, statistics

rows = []
with open(os.environ["IDLE_CSV"]) as fh:
    for r in csv.DictReader(fh):
        try:
            rows.append({
                "cpu_pct": float(r["cpu_pct"]) if r["cpu_pct"] else None,
                "rss": int(r["rss_bytes"]),
            })
        except (ValueError, KeyError):
            continue

# Drop the first few samples: they still contain start-up work, not idle.
settled = rows[3:] if len(rows) > 6 else rows
cpus = [r["cpu_pct"] for r in settled if r["cpu_pct"] is not None]
rss  = [r["rss"] for r in settled]

fp = {
    "target":        os.environ["TARGET"],
    "artifact":      os.environ.get("ARTIFACT", ""),
    "artifactBytes": int(os.environ.get("ARTIFACT_BYTES", 0) or 0),
    "coldStartMs":   int(os.environ.get("COLD_START_MS", 0) or 0),
    "idleHoldSeconds": int(os.environ.get("IDLE_HOLD", 0) or 0),
    "idleSamples":   len(settled),
    "idleCpuPctMean": round(statistics.fmean(cpus), 3) if cpus else None,
    "idleCpuPctMax":  round(max(cpus), 3) if cpus else None,
    "idleRssBytesMean": int(statistics.fmean(rss)) if rss else None,
    "idleRssBytesMax":  max(rss) if rss else None,
}

with open(os.environ["OUT"], "w") as f:
    json.dump(fp, f, indent=2)
    f.write("\n")

mb = (fp["idleRssBytesMean"] or 0) / 1048576
print(f"  idle RSS {mb:.1f} MiB, idle CPU {fp['idleCpuPctMean']}%", flush=True)
PY

dim "  footprint -> $OUT"
