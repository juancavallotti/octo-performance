# 004 — Queue round trip

Two flows joined by an internal queue, with a concurrency knob on each side.

## The integration

```
POST /job
  → multi-transform        bind eventID as the job id
  → queue-dispatch         subject work.jobs, awaitReply — BLOCKS here
                                   │
                                   ▼
                           ┌─ worker flow (queue source, `listeners` handlers)
                           │    multi-transform   score + band
                           │    set-payload       reply
                           └─ reply folded back into the producer's message
  → JSON
```

| | |
|---|---|
| Connectors | `http` (HTTP Server), `queue` (Platform Queue) |
| Sources | `http` route `POST /job`, `queue` subscription on `work.jobs`, `GET /health` |
| Blocks | `multi-transform` ×2, `queue-dispatch` (awaitReply), `set-payload` ×2 |
| Flows | 2 root flows — one per side of the queue — plus health |
| External dependencies | none — the default build backs the queue in-process |

## Why this scenario

**It is the only scenario with a knob on both sides of a boundary.** The producer's
`workers` caps how many round trips can be in flight; the consumer's `listeners` caps how many
handlers drain the queue. Raising either alone should hit the other as a ceiling, which makes
this the cleanest available test of whether two independent concurrency controls compose.

**`awaitReply` makes the queue measurable.** Fire-and-forget dispatch returns before the work
happens, so an HTTP client would measure the enqueue and nothing else. Waiting for the reply
makes the round trip synchronous and puts the producer in the same blocking regime as
scenario 003 — which is what makes producer `workers` matter here at all.

## Tunables

Each is declared in exactly **one** place in `integration.yaml`, so they never collide:

| Knob | Where | What it limits |
|---|---|---|
| `workers` | `submit` flow | round trips in flight, since dispatch blocks |
| `buffer` | `submit` flow | channel depth before the HTTP source blocks |
| `pool` | `submit` flow | shared pool for composites (unused — no fork) |
| `listeners` | `worker` queue source | concurrent handlers draining the queue |

The `worker` flow deliberately declares no `workers`/`buffer`/`pool` of its own. On the
consumer side `listeners` is the control being measured, and a second knob in the same path
would confound it.

```bash
task bench SCENARIO=004-queue-roundtrip TUNED_WORKERS=64 TUNED_LISTENERS=64
```

## What to look for

The interesting result is not either knob alone but the **crossover**. If throughput tracks
`min(workers, listeners)`, the two compose as expected and the guidance is simply "raise both".
If raising `listeners` past some point stops helping while `workers` still binds — or vice
versa — the queue itself is the limit, and that is worth knowing before anyone builds a
pipeline of these.

## Running it

```bash
task smoke    SCENARIO=004-queue-roundtrip
task capacity SCENARIO=004-queue-roundtrip           # then set STEADY_RATE from the knee
task bench    SCENARIO=004-queue-roundtrip TUNED_WORKERS=64 TUNED_LISTENERS=64
```

## Checks in the smoke gate

A request/reply queue fails in a way that still returns 200: if the reply is never folded
back, the response is shaped from an empty body and every field is quietly missing. The gate
asserts the consumer's arithmetic came back, which is the only proof the round trip completed
rather than timing out silently:

- status is 200, body parses as JSON, flow reports `ok`
- the producer bound a job id before dispatch
- `handled` is true — a worker actually ran, not just an enqueue that returned
- `score` matches `weight × units × 1.37` computed consumer-side, and `band` matches
