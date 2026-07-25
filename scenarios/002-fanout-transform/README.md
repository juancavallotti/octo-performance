# 002 — Fan-out and transform

An order-processing flow that fans out across parallel branches and composes other flows.

## The integration

```
POST /order/{id}
  → multi-transform            normalise, 4 accumulating CEL edits
  → fork                       3 parallel branches, scheduled on the shared pool
      ├─ pricing               foreach (map) over the order lines
      ├─ notify                multi-transform
      └─ audit                 multi-transform
  → flow-ref  score-risk       callable flow, runs on the main message
  → flow-ref  finalize         callable flow, builds the response
  → JSON
```

| | |
|---|---|
| Connectors | `http` (HTTP Server) |
| Sources | `http` route `POST /order/{id}`, plus `GET /health` for readiness |
| Blocks | `multi-transform`, `fork`, `foreach` (map mode), `flow-ref`, `set-payload` |
| Flows | 1 measured root flow, 2 callable flows, 1 health flow |
| External dependencies | none |

## Why this scenario

**It is the first scenario that actually exercises `pool`.** Scenario 001 declared the knob
but never scheduled anything on it — `pool` is the shared worker pool that composite blocks
use for concurrent work, and with no `fork`, `enrich`, or parallel branch in the flow it sat
idle. Here the `fork` hands three branches to it on every request, so `pool` and the branch
count interact directly.

It also puts real work in front of the concurrency knobs. Scenario 001 established that a
single non-blocking block leaves nothing for `workers` or `buffer` to relieve. This flow runs
a multi-transform, a three-branch fan-out, a mapped `foreach` over every order line, and two
cross-flow calls per request — enough per-message CPU that queueing behaviour becomes
observable.

## Semantics worth knowing

**`fork` branches do not merge back.** The block scatters a *clone* of the message to each
branch, then joins and passes the **original** through. Variables written inside a branch are
not visible downstream. That is why `score-risk` is invoked on the main path rather than
inside a branch — the fork branches exist to consume work, not to contribute to the response.

**Callable flows have no knobs of their own.** `score-risk` and `finalize` declare no source
and no `workers`/`buffer`/`pool`; they run on the calling flow's workers. Only root flows
carry concurrency settings.

**`multi-transform` takes `transforms` under `settings`.** It is a regular block field, not a
top-level slot like `branches` or `process`.

## What is compared

| Arm | `workers` / `buffer` / `pool` |
|---|---|
| baseline | stripped — the runtime's own defaults |
| tuned | set explicitly; `pool` is the interesting axis |

The sweep grid deliberately spans `pool` from 2 to 32 while holding the others coarse: the
question this scenario asks is whether the fan-out is pool-limited.

## Running it

```bash
task smoke    SCENARIO=002-fanout-transform
task capacity SCENARIO=002-fanout-transform     # then set STEADY_RATE from the knee
task sweep    SCENARIO=002-fanout-transform     # pool 2 / 8 / 32
task bench    SCENARIO=002-fanout-transform TUNED_POOL=32
```

## Load profile

`STEADY_RATE` starts at 4,000 req/s — far below scenario 001's 16,000, because each request
now does substantially more work. `ORDER_LINES` (default 8) controls how many lines the
payload carries and therefore how many `foreach` iterations each message costs; raising it is
the cheapest way to make the scenario more CPU-heavy without changing the flow.

Run `task capacity` on any new hardware or Octo version before trusting `STEADY_RATE`.

## Checks in the smoke gate

Every composite here can fail silently — a fork branch that errors is joined over, a
`flow-ref` that no-ops just leaves a variable unset — so the gate asserts each stage's output
rather than only the status code:

- status is 200 and the body parses as JSON
- `multi-transform` applied (`lineCount` matches the payload, `gross` computed)
- `flow-ref` reached `score-risk` (`riskBand` is present and a valid band, not `unscored`)
- `flow-ref` reached `finalize` (`status` is `accepted`, order id echoed)
