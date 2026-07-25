# octo-performance

A benchmarking lab for [Octo](https://juancavallotti.github.io/octo/) — a Go integration runtime
that executes flows defined in YAML.

The goal is boring and specific: produce **reproducible, version-stamped** performance numbers for
real integration scenarios, so that tuning is evidence-based and regressions between runtime
versions get caught.

Every result answers four questions:

- What hardware and what runtime version?
- What integration was under test?
- What does it do **out of the box**, and what does it do **tuned**?
- What did that throughput **cost** in CPU and memory?

## Quick start

```bash
brew install k6 go-task                       # prerequisites
task preflight                                # verify tooling, print versions

task smoke SCENARIO=001-template-page         # correctness gate
task bench SCENARIO=001-template-page         # baseline + tuned, 3 reps
```

Results land in `results/<run-id>/REPORT.md`. `task --list` shows every entry point.

## What gets measured

**Throughput and latency** via [Grafana k6](https://grafana.com/docs/k6/latest/using-k6/), using
open-model arrival-rate executors so that a struggling server shows up as latency and dropped
iterations rather than as quietly reduced load.

**Resource cost** via OS-level sampling of the server process — CPU-seconds, RSS, and the derived
numbers that actually matter: **CPU-ms per request**, RSS per 1k RPS, requests per CPU-core-second.

**Runtime footprint** — artifact size, cold-start time, idle memory, idle CPU. The standing cost of
the runtime before it serves a single request.

## Targets

| Target | What runs |
|---|---|
| `native` | The standalone `octo` binary on the host. |
| `docker` | The published `juancavallotti/octo-runtime` image. |

Both are benchmarked because both are how Octo actually gets deployed. On macOS they are not
directly comparable to each other — see the caveats in [METHODOLOGY.md](METHODOLOGY.md).

## Tuning knobs

Octo exposes three concurrency settings on root flows, and they are exactly what the
baseline-vs-tuned comparison explores:

| Field | Default | Meaning |
|---|---|---|
| `workers` | 8 | Consumers pulling from the flow's message channel. |
| `buffer` | 64 | Channel depth before publishers block. |
| `pool` | 8 | Shared worker pool handed to concurrent composites such as `fork`. |

`task sweep` grid-searches them and reports the winner.

**When they matter.** Only once the flow blocks. On a CPU-bound flow ([001](scenarios/001-template-page/),
[002](scenarios/002-fanout-transform/)) tuning changes nothing measurable — a worker never
waits, so there is no queue to relieve. On a flow that makes three database round trips per
request ([003](scenarios/003-postgres-crud/)), raising `workers` from the default 8 to 64
took p95 from **4,134 ms to 25.7 ms** at the same offered rate, and turned a saturated system
into one running at the full requested throughput.

The rule that falls out: for a blocking flow, `workers` needs to be at least
`target_rps × seconds_blocked_per_request`. The default of 8 suits CPU-bound work and is
badly wrong for anything that waits on I/O.

## Scenarios

| Scenario | What it exercises |
|---|---|
| [001-template-page](scenarios/001-template-page/) | HTTP route rendering a template resource as an HTML page. The floor: HTTP source, path parameters, templating, no I/O. |
| [002-fanout-transform](scenarios/002-fanout-transform/) | `fork` fan-out, `multi-transform`, mapped `foreach`, and `flow-ref` composition. The first scenario that actually schedules work on `pool`. |
| [003-postgres-crud](scenarios/003-postgres-crud/) | Write, read back, and delete a row per request against containerised Postgres. The first flow that **blocks** — where `workers` meets `maxOpenConns`. |

More scenarios — other connectors, database-backed flows, outbound REST calls, forked composites —
get added over time. See [AGENTS.md](AGENTS.md) for the checklist.

## Layout

```
lab/bin/        the harness
lab/k6/lib/     shared k6 helpers
lab/hosts/      one .env per machine that can run the lab
scenarios/      benchmark scenarios
results/        immutable run output, never hand-edited
docs/           GitHub Pages skeleton (Pages not enabled yet)
```

## Documentation

- [METHODOLOGY.md](METHODOLOGY.md) — how runs are conducted, what each metric means, and what the
  numbers do not mean. Read before interpreting any result.
- [AGENTS.md](AGENTS.md) — the working contract: golden rules, how to add a scenario, and the
  findings workflow.
- [results/index.md](results/index.md) — every run recorded so far.

## Findings

Observations about Octo made while benchmarking are logged in a
[Notion page](https://app.notion.com/p/juancavallotti/Performance-Benchmarking-3a88c36eda30803ab07dde64a2b38a1c)
for thinking. Anything we decide to act on becomes an issue on
[`juancavallotti/octo`](https://github.com/juancavallotti/octo).
