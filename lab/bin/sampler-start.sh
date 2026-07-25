#!/usr/bin/env bash
# Start the resource sampler in the background and record its pid.
#
# Usage: sampler-start.sh <target> <id> <out.csv> <state-dir> [interval]

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
