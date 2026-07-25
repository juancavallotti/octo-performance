#!/usr/bin/env bash
# Remove this scenario's Postgres. Run by the harness after a benchmark completes.
#
# Set KEEP_PG=1 to leave it running — useful when iterating on the flow, since a
# fresh container costs several seconds of startup per attempt.

set -euo pipefail

NAME="${PG_CONTAINER:-octo-bench-pg}"

if [ "${KEEP_PG:-0}" = "1" ]; then
  echo "  KEEP_PG=1 — leaving $NAME running" >&2
  exit 0
fi

docker rm -f "$NAME" >/dev/null 2>&1 || true
echo "  postgres removed ($NAME)" >&2
