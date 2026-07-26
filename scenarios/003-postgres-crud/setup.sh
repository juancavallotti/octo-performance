#!/usr/bin/env bash
# Bring up this scenario's Postgres and create its schema.
#
# Run automatically by the harness before a benchmark when a scenario declares it.
# Idempotent: safe to run when the container is already up.
#
# The container is deliberately NOT torn down between repetitions — a cold
# Postgres would put schema creation and connection establishment inside the
# measured window. teardown.sh removes it at the end of a run.

set -euo pipefail

NAME="${PG_CONTAINER:-octo-bench-pg}"
IMAGE="${PG_IMAGE:-postgres:17-alpine}"
PORT="${PG_PORT:-55432}"
PASSWORD="${PG_PASSWORD:-octobench}"
DB="${PG_DB:-octobench}"

command -v docker >/dev/null 2>&1 || { echo "error: docker is required for scenario 003" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "error: docker daemon not reachable" >&2; exit 1; }

if [ "$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null || echo false)" = "true" ]; then
  echo "  postgres already running ($NAME)" >&2
else
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  echo "  starting $IMAGE on :$PORT" >&2
  docker run -d --name "$NAME" \
    -e POSTGRES_PASSWORD="$PASSWORD" \
    -e POSTGRES_DB="$DB" \
    -p "${PORT}:5432" \
    --shm-size=256m \
    "$IMAGE" \
    -c max_connections=400 \
    -c shared_buffers=256MB \
    -c fsync=off \
    -c synchronous_commit=off \
    -c full_page_writes=off \
    >/dev/null
fi

# Wait for the server to accept connections — over TCP, and twice.
#
# Both details matter. On first boot the postgres entrypoint runs initdb against a
# temporary server that listens on a unix socket ONLY, runs init scripts, then stops
# it and restarts for real. A plain `pg_isready` succeeds against that temporary
# instance, so the schema step below would race the restart and fail with
# "connection to server on socket ... failed: No such file or directory". Forcing
# TCP skips the socket-only phase, and requiring two consecutive successes a second
# apart survives the restart in between.
ready=0
for _ in $(seq 1 120); do
  if docker exec "$NAME" pg_isready -h 127.0.0.1 -p 5432 -U postgres -d "$DB" >/dev/null 2>&1; then
    ready=$((ready + 1))
    [ "$ready" -ge 2 ] && break
  else
    ready=0
  fi
  sleep 0.5
done
[ "$ready" -ge 2 ] || {
  echo "error: postgres did not become ready" >&2
  docker logs --tail 30 "$NAME" >&2 || true
  exit 1
}

# Schema. Created once, outside the measured window. Retried anyway: readiness is a
# prediction, and a benchmark that dies here has wasted the whole run.
for attempt in 1 2 3 4 5; do
  if docker exec -i "$NAME" psql -h 127.0.0.1 -U postgres -d "$DB" -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE TABLE IF NOT EXISTS orders (
  id          text PRIMARY KEY,
  customer    text        NOT NULL,
  channel     text        NOT NULL,
  amount      numeric(12,2) NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS orders_customer_idx ON orders (customer);
TRUNCATE orders;
SQL
  then
    echo "  postgres ready on :$PORT (db=$DB, table=orders)" >&2
    exit 0
  fi
  echo "  schema attempt $attempt failed; retrying" >&2
  sleep 2
done

echo "error: could not create the schema after 5 attempts" >&2
docker logs --tail 30 "$NAME" >&2 || true
exit 1
