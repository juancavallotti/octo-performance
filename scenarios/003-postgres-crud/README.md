# 003 — Postgres CRUD

A flow that writes, reads back, and deletes a row per request against a containerised
Postgres.

## The integration

```
POST /order
  → multi-transform    bind eventID as the primary key
  → sql  INSERT        exec
  → sql  SELECT        single row — read your own write
  → sql  DELETE        exec
  → JSON
```

| | |
|---|---|
| Connectors | `http` (HTTP Server), `database` (Postgres) |
| Sources | `http` route `POST /order`, plus `GET /health` which issues a `SELECT 1` |
| Blocks | `multi-transform`, `sql` ×3 (+1 in health), `set-variable`, `set-payload` |
| External dependencies | Postgres 17, started by `setup.sh` on port 55432 |

## Why this scenario

**This is the first scenario whose flow blocks.** Scenarios 001 and 002 are CPU-bound: a
worker never waits, so there is nothing for `workers` or `buffer` to relieve and tuning has
nowhere to help. Here every request makes three round trips to the database, so a worker
spends most of its life idle-but-occupied — the regime the concurrency knobs exist for.

It is also the first scenario with **two competing limits**. The flow's `workers` decides how
many messages are in flight; the connector's `maxOpenConns` decides how many can actually
talk to Postgres. Raising `workers` past `maxOpenConns` does not add throughput, it just
moves the queue from the message channel to the connection pool. Finding where those two
cross is the point.

## What this scenario has already shown

Measured on `m1pro-16gb`, Octo 0.4.3, Postgres 17 in a container on the same host.

**Tuning `workers` is worth 161× on p95 here.** Same flow, same offered rate (4,000 req/s),
25 seconds:

| arm | `workers` | throughput | p95 | p99 | dropped |
|---|---|---|---|---|---|
| baseline | 8 (runtime default) | 2,599 req/s | **4,134 ms** | 5,137 ms | 20,948 |
| tuned | 64 | 3,999 req/s — the full offered rate | **25.7 ms** | 62.8 ms | 0 |

The arithmetic predicts it. Three round trips at roughly 1.5 ms each is ~4.5 ms of blocked
time per message, so eight workers cap the flow at 8 ÷ 4.5 ms ≈ 1,780 req/s. The capacity
ramp measured a ceiling of 1,664 req/s — and confirmed the flow was never CPU-bound: Octo's
CPU peaked at 136% of one core, then *fell* to 42% while throughput collapsed and latency
climbed past nine seconds. The workers were sitting on blocked sockets, not working.

**The rule:** for a blocking flow, `workers` needs to be at least
`target_rps × seconds_blocked_per_request`. The default of 8 suits CPU-bound work and is
badly wrong for anything that waits on I/O.

Note what this does *not* say. Scenarios 001 and 002 showed no benefit from tuning at all,
because a non-blocking flow has no queue to relieve. The knob only earns its keep once
something waits.

## Tunables

Declared in `scenario.yaml` under `tunables`:

| Knob | Where | What it limits |
|---|---|---|
| `workers` | root flow | messages processed concurrently |
| `buffer` | root flow | channel depth before the source blocks |
| `pool` | root flow | shared pool for concurrent composites (unused here — no fork) |
| `maxOpenConns` | `database` connector | open connections to Postgres |
| `maxIdleConns` | `database` connector | connections kept warm |

Baseline strips all five; tuned sets whichever are passed as `TUNED_<KNOB>`:

```bash
task bench SCENARIO=003-postgres-crud \
  TUNED_WORKERS=64 TUNED_MAXOPENCONNS=64 TUNED_MAXIDLECONNS=64
```

## The database

`setup.sh` starts `postgres:17-alpine` on port 55432 and creates the `orders` table; the
harness runs it automatically before a benchmark and `teardown.sh` afterwards, both outside
the measured window so container start-up and schema creation never land in the numbers.

The server runs with `fsync=off`, `synchronous_commit=off`, and `full_page_writes=off`. That
is **deliberately unsafe** and correct here: durability settings would make this a benchmark
of disk flush behaviour rather than of the runtime's concurrency handling. `max_connections`
is raised to 400 so the connection-pool sweep is not capped by the server.

Set `KEEP_PG=1` to leave the container running between attempts while iterating on the flow.

## Keeping the table bounded

Each request writes a row keyed by `eventID` — unique per message, so concurrent requests
never collide on the primary key — and deletes it again before responding. At thousands of
requests per second over sixty seconds the table would otherwise grow by millions of rows
mid-run and the measurement would drift as the index grew. The smoke gate asserts
`rowsAffected == 1` on the delete, so a silent failure of that cleanup fails the run rather
than quietly changing what is being measured.

## Running it

```bash
task smoke    SCENARIO=003-postgres-crud
task capacity SCENARIO=003-postgres-crud          # then set STEADY_RATE from the knee
task sweep    SCENARIO=003-postgres-crud          # workers axis
task bench    SCENARIO=003-postgres-crud TUNED_WORKERS=64 TUNED_MAXOPENCONNS=64
```

## Checks in the smoke gate

A CRUD flow can return 200 while doing nothing — an INSERT that failed silently, a SELECT
matching no row, a DELETE removing nothing — so each statement's effect is asserted:

- status is 200, body parses as JSON, flow reports `ok`
- the INSERT bound its arguments (a non-empty `orderId` came back)
- the SELECT read the row back (`customer` matches what was written)
- the DELETE matched exactly one row
