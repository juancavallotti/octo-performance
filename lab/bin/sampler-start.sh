#!/usr/bin/env bash
# Start the resource sampler in the background and record its pid.
#
# Usage: sampler-start.sh <target> <id> <out.csv> <state-dir> [interval]
#
# Two observers, started together so they cover the same window:
#
#   sample-resources.py  the process from outside — ps, or the Docker Engine API
#   scrape-metrics.py    the process from inside — its own /metrics, when it has one
#
# The outside view is kept even when the inside view is available, and not out of
# caution. They measure different things: /usr/bin/time and ps see the whole process
# as the OS accounts for it, while process_cpu_seconds_total is what the process
# believes it consumed. Recording both means a divergence is visible rather than
# averaged away — and on the container target, where the outside view comes from the
# Docker VM's accounting, that divergence is the interesting part.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

TGT="${1:?target required}"
ID="${2:?pid or container id required}"
OUT="${3:?out csv required}"
STATE="${4:?state dir required}"
INTERVAL="${5:-1.0}"

mkdir -p "$STATE" "$(dirname "$OUT")"

python3 "$LAB_BIN/sample-resources.py" \
  --target "$TGT" --id "$ID" --out "$OUT" --interval "$INTERVAL" &

echo $! > "$STATE/sampler.pid"

# METRICS_SCRAPE_URL is set by the caller, which is the only place that knows
# whether this run has metrics at all — it depends on the build under test, not on
# anything the sampler can see.
if [ -n "${METRICS_SCRAPE_URL:-}" ]; then
  python3 "$LAB_BIN/scrape-metrics.py" \
    --url "$METRICS_SCRAPE_URL" \
    --out "$(dirname "$OUT")/metrics.csv" \
    --interval "$INTERVAL" 2>"$STATE/scrape-metrics.log" &
  echo $! > "$STATE/scraper.pid"
fi
