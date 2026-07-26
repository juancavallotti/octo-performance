---
title: Octo performance lab
---

# Octo performance lab

Reproducible, version-stamped benchmarks for [Octo](https://juancavallotti.github.io/octo/),
a Go integration runtime that executes flows defined in YAML.

Every result answers four questions: **what hardware and what runtime version**, **what
integration** was under test, what it does **out of the box versus tuned**, and what that
throughput **cost** in CPU and memory.

- **[Results index](results/index.md)** — every run recorded so far, with a regression view
  across Octo versions and a native-against-container view
- **[Methodology](METHODOLOGY.md)** — how runs are conducted, what each metric means, and
  what the numbers do not mean. Read this before quoting any of them.
- **[Comparison to other runtimes](COMPARISON.md)** — what has to match before two
  benchmark figures describe the same thing, and what may honestly be concluded today
- **[Working contract](AGENTS.md)** — the rules a result has to satisfy to be published

## Scenarios

| # | Scenario | What it exercises |
|---|---|---|
| 001 | [template-page](scenarios/001-template-page/) | HTTP source, path params, templating. No I/O — the floor. |
| 002 | [fanout-transform](scenarios/002-fanout-transform/) | `fork` fan-out, `multi-transform`, mapped `foreach`, `flow-ref` |
| 003 | [postgres-crud](scenarios/003-postgres-crud/) | Write, read back and delete a row per request. The first flow that blocks. |
| 004 | [queue-roundtrip](scenarios/004-queue-roundtrip/) | Two flows joined by an internal queue with `awaitReply` |
| 005 | [http-proxy](scenarios/005-http-proxy/) | Pass a request to a backend that takes 70 ms — the canonical API-gateway shape. |
| 006 | [json-transform](scenarios/006-json-transform/) | Reshape a JSON collection two ways. Payload transformation, the workload every runtime is measured on. |

## The finding that runs through all of them

**Tuning matters only when the flow blocks.** On CPU-bound work — 001, 002, 004, 006 —
raising `workers`, `buffer` and `pool` changes nothing measurable, and sometimes costs a
little. A worker that never waits has no queue to relieve.

Once a flow waits on something, the same knob decides throughput outright:

| Scenario | What it waits on | Out of the box | Tuned |
|---|---|---|---|
| [005](scenarios/005-http-proxy/) | 70 ms backend | **106.6 req/s**, p95 26.9 s | **799.1 req/s**, p95 71.5 ms |
| [003](scenarios/003-postgres-crud/) | 3 Postgres round trips | 2,598 req/s, p95 4,134 ms | 3,999 req/s, p95 25.7 ms |

The arithmetic predicts both: a flow needs at least
`target_rps × seconds_blocked_per_request` workers. The default of 8 is right for
CPU-bound work and badly wrong for anything that waits on I/O — and the symptom
(multi-second latency while CPU sits idle) points away from the cause.

## What these numbers are not

Everything here was measured on a single Apple M1 Pro laptop **that was also running the
load generator**. That is a recorded limitation, not a controlled variable. Throughput
comparisons against other runtimes are not defensible from this host; footprint and CPU-ms
per request are. See [COMPARISON.md](COMPARISON.md) for where that line is drawn and why.
